// Command carsight-logger connects to an ELM327 adapter, identifies the car
// by VIN and logs live engine data to SQLite until stopped.
//
//	carsight-logger [flags] log         log a drive (default)
//	carsight-logger [flags] scan        show protocol, VIN, supported PIDs, DTCs and readiness
//	carsight-logger [flags] clear-dtcs  clear stored trouble codes (asks for confirmation)
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"maps"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/jnszkr/carsight/internal/elm327"
	"github.com/jnszkr/carsight/internal/obd"
	"github.com/jnszkr/carsight/internal/store"
)

func main() {
	var (
		port       = flag.String("port", "auto", "serial device, 'auto', or 'sim:volvo' / 'sim:citroen' for the simulator")
		baud       = flag.Int("baud", 0, "baud rate (0 = auto-detect)")
		dbPath     = flag.String("db", "carsight.db", "SQLite database path")
		name       = flag.String("name", "", "friendly vehicle name, e.g. 'Volvo V70'")
		notes      = flag.String("notes", "", "notes for this session, e.g. 'cold start, commute'")
		slowEvery  = flag.Int("slow-every", 5, "poll slow PIDs (temperatures, voltage, LTFT) every N cycles")
		duration   = flag.Duration("duration", 0, "stop logging after this long (0 = until Ctrl-C)")
		yes        = flag.Bool("yes", false, "clear-dtcs: don't ask for confirmation")
		simSpeedup = flag.Float64("sim-speedup", 1, "simulator: run simulated time this many times faster")
	)
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: carsight-logger [flags] [log|scan|clear-dtcs]\n\n")
		flag.PrintDefaults()
	}
	flag.Parse()
	cmd := "log"
	if flag.NArg() > 0 {
		cmd = flag.Arg(0)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	rw, desc, err := openAdapter(*port, *baud, *simSpeedup)
	if err != nil {
		log.Fatal(err)
	}
	defer rw.Close()
	conn := elm327.New(rw)
	if err := conn.Init(ctx); err != nil {
		log.Fatalf("initialise adapter on %s: %v", desc, err)
	}
	log.Printf("connected: %s, %s, %s", desc, conn.Version, conn.Protocol)
	client := &obd.Client{R: conn, CAN: conn.Protocol.IsCAN()}

	switch cmd {
	case "scan":
		err = scan(ctx, client)
	case "clear-dtcs":
		err = clearDTCs(ctx, client, *yes)
	case "log":
		err = logDrive(ctx, client, conn.Protocol.String(), *dbPath, *name, *notes, *slowEvery, *duration)
	default:
		flag.Usage()
		os.Exit(2)
	}
	if err != nil {
		log.Fatal(err)
	}
}

func scan(ctx context.Context, c *obd.Client) error {
	vin, err := c.VIN(ctx)
	if err != nil {
		fmt.Printf("VIN: unavailable (%v)\n", err)
	} else {
		fmt.Printf("VIN: %s\n", vin)
	}
	sup, err := c.SupportedPIDs(ctx)
	if err != nil {
		return fmt.Errorf("supported PIDs: %w", err)
	}
	fmt.Println("\nLogged PIDs:")
	for _, p := range obd.Default {
		mark := "no "
		if sup[p.Code] {
			mark = "yes"
		}
		fmt.Printf("  %s  %02X %-16s %s\n", mark, p.Code, p.Name, p.Desc)
	}
	var all []string
	for code := 1; code < 0xE0; code++ {
		if sup[byte(code)] && code%0x20 != 0 {
			all = append(all, fmt.Sprintf("%02X", code))
		}
	}
	fmt.Printf("\nAll supported mode 01 PIDs: %s\n", strings.Join(all, " "))

	for _, pending := range []bool{false, true} {
		kind := "Stored"
		if pending {
			kind = "Pending"
		}
		codes, err := c.DTCs(ctx, pending)
		switch {
		case err != nil:
			fmt.Printf("\n%s DTCs: error: %v\n", kind, err)
		case len(codes) == 0:
			fmt.Printf("\n%s DTCs: none\n", kind)
		default:
			fmt.Printf("\n%s DTCs: %s\n", kind, strings.Join(codes, ", "))
		}
	}
	if r, err := c.ReadReadiness(ctx); err == nil {
		fmt.Printf("\nMIL: %v, DTC count: %d\nMonitors:\n", r.MIL, r.DTCCount)
		for _, k := range slices.Sorted(maps.Keys(r.Monitors)) {
			fmt.Printf("  %-18s %s\n", k, r.Monitors[k])
		}
	}
	return nil
}

func clearDTCs(ctx context.Context, c *obd.Client, yes bool) error {
	codes, err := c.DTCs(ctx, false)
	if err != nil {
		return err
	}
	fmt.Printf("Stored DTCs: %v\n", codes)
	if !yes {
		fmt.Print("Clearing DTCs also resets freeze frames and readiness monitors (the car may fail an inspection until they re-run).\n" +
			"Engine must be off with ignition on. Type 'clear' to continue: ")
		line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		if strings.TrimSpace(line) != "clear" {
			return errors.New("aborted")
		}
	}
	if err := c.ClearDTCs(ctx); err != nil {
		return err
	}
	fmt.Println("DTCs cleared.")
	return nil
}

func logDrive(ctx context.Context, c *obd.Client, protocol, dbPath, name, notes string, slowEvery int, duration time.Duration) error {
	db, err := store.Open(dbPath)
	if err != nil {
		return err
	}
	defer db.Close()

	vin, err := c.VIN(ctx)
	if err != nil {
		// Some older ECUs don't support mode 09; fall back to a stable placeholder.
		log.Printf("VIN unavailable (%v); logging under UNKNOWN", err)
		vin = "UNKNOWN"
	}
	if err := db.UpsertVehicle(ctx, store.Vehicle{VIN: vin, Name: name, Protocol: protocol}); err != nil {
		return err
	}
	sup, err := c.SupportedPIDs(ctx)
	if err != nil {
		return fmt.Errorf("supported PIDs: %w", err)
	}
	pids := obd.Filter(obd.Default, sup)
	if len(pids) == 0 {
		return errors.New("car supports none of the logged PIDs")
	}
	var fast, slow []obd.PID
	var names []string
	for _, p := range pids {
		names = append(names, p.Name)
		if p.Slow {
			slow = append(slow, p)
		} else {
			fast = append(fast, p)
		}
	}

	start := time.Now()
	id, err := db.StartSession(ctx, vin, protocol, start)
	if err != nil {
		return err
	}
	if notes != "" {
		_ = db.SetNotes(ctx, id, notes)
	}
	log.Printf("session %d for %s: logging %s", id, vin, strings.Join(names, ", "))

	// Once-per-session data. DTCs are read again at the end in case
	// something set during the drive.
	readDTCs := func(ctx context.Context) {
		for _, pending := range []bool{false, true} {
			kind := map[bool]string{false: "stored", true: "pending"}[pending]
			codes, err := c.DTCs(ctx, pending)
			if err != nil {
				log.Printf("read %s DTCs: %v", kind, err)
				continue
			}
			if len(codes) > 0 {
				log.Printf("%s DTCs: %s", kind, strings.Join(codes, ", "))
			}
			_ = db.AddDTCs(ctx, id, kind, codes, time.Now())
		}
	}
	readDTCs(ctx)
	if r, err := c.ReadReadiness(ctx); err == nil {
		b, _ := json.Marshal(r)
		_ = db.SetReadiness(ctx, id, string(b))
	}

	if duration > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, duration)
		defer cancel()
	}
	log.Print("logging; press Ctrl-C to stop")

	var buf []store.Sample
	var total, cycles, errs int
	lastFlush, lastStatus := time.Now(), time.Now()
	flush := func(ctx context.Context) {
		if err := db.AddSamples(ctx, id, buf); err != nil {
			log.Printf("write samples: %v", err)
		}
		total += len(buf)
		buf = buf[:0]
	}
	poll := func(p obd.PID) bool {
		v, err := c.Read(ctx, p)
		if ctx.Err() != nil {
			return false
		}
		if err != nil {
			errs++
			if errors.Is(err, elm327.ErrUnableConnect) {
				log.Printf("lost connection to the car: %v", err)
				return false
			}
			return true
		}
		buf = append(buf, store.Sample{T: time.Now(), PID: p.Name, Value: v})
		return true
	}
loop:
	for {
		group := fast
		if cycles%max(slowEvery, 1) == 0 {
			group = append(append([]obd.PID{}, fast...), slow...)
		}
		for _, p := range group {
			if !poll(p) {
				break loop
			}
		}
		cycles++
		if time.Since(lastFlush) > 2*time.Second {
			flush(ctx)
			lastFlush = time.Now()
		}
		if time.Since(lastStatus) > 30*time.Second {
			el := time.Since(start).Seconds()
			log.Printf("%s: %d samples, %.1f cycles/s, %d errors", time.Since(start).Round(time.Second), total+len(buf), float64(cycles)/el, errs)
			lastStatus = time.Now()
		}
	}

	// The run context is done; finish up with a fresh one.
	end := time.Now()
	fin, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	flush(fin)
	readDTCs(fin)
	if err := db.EndSession(fin, id, end); err != nil {
		return err
	}
	el := end.Sub(start).Seconds()
	log.Printf("session %d done: %s, %d samples, %.1f cycles/s (fast group), %d read errors",
		id, end.Sub(start).Round(time.Second), total, float64(cycles)/el, errs)
	fmt.Println(id)
	return nil
}
