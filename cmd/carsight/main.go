// Command carsight logs OBD-II data from a car through an ELM327 adapter,
// stores it in SQLite, writes reports, and serves the data to AI assistants
// over MCP.
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
	"path/filepath"
	"runtime/debug"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/jnszkr/carsight/internal/analyze"
	"github.com/jnszkr/carsight/internal/elm327"
	"github.com/jnszkr/carsight/internal/mcpserver"
	"github.com/jnszkr/carsight/internal/obd"
	"github.com/jnszkr/carsight/internal/store"
)

const usage = `carsight: log, store and analyse OBD-II data.

Usage:
  carsight <command> [flags]

Car commands (need the adapter plugged in):
  scan        show protocol, VIN, supported PIDs, DTCs and monitor readiness
  log         log a drive until Ctrl-C (or -duration)
  clear-dtcs  clear stored trouble codes (asks for confirmation)

Data commands:
  sessions    list vehicles and logged drives
  report      Markdown report for one drive (default: latest)
  trend       Markdown trend report across drives
  summary     drive summary as JSON
  mcp         serve the database to an AI assistant over MCP (stdio)

Run 'carsight <command> -h' for the command's flags.
`

func main() {
	log.SetFlags(log.Ltime)
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var err error
	switch cmd {
	case "scan", "log", "clear-dtcs":
		err = carCommand(ctx, cmd, args)
	case "sessions":
		err = sessionsCmd(ctx, args)
	case "report":
		err = reportCmd(ctx, args)
	case "trend":
		err = trendCmd(ctx, args)
	case "summary":
		err = summaryCmd(ctx, args)
	case "mcp":
		err = mcpCmd(ctx, args)
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", cmd, usage)
		os.Exit(2)
	}
	if err != nil {
		log.Fatal(err)
	}
}

// defaultDB is the database used when -db isn't given: $CARSIGHT_DB, or
// carsight.db in the user's data directory so every command (and the MCP
// server, which an AI client starts from an arbitrary directory) agrees.
func defaultDB() string {
	if p := os.Getenv("CARSIGHT_DB"); p != "" {
		return p
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "carsight.db"
	}
	return filepath.Join(dir, "carsight", "carsight.db")
}

func openDB(path string) (*store.Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	return store.Open(path)
}

func newFlags(name, args string) (*flag.FlagSet, *string) {
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: carsight %s [flags] %s\n\n", name, args)
		fs.PrintDefaults()
	}
	return fs, fs.String("db", defaultDB(), "SQLite database path (env CARSIGHT_DB)")
}

func carCommand(ctx context.Context, cmd string, args []string) error {
	fs, dbPath := newFlags(cmd, "")
	port := fs.String("port", "auto", "serial device, 'auto', or 'sim:volvo' / 'sim:citroen' for the simulator")
	baud := fs.Int("baud", 0, "baud rate (0 = auto-detect)")
	simSpeedup := fs.Float64("sim-speedup", 1, "simulator: run simulated time this many times faster")
	var name, notes *string
	var slowEvery *int
	var duration *time.Duration
	var yes *bool
	switch cmd {
	case "log":
		name = fs.String("name", "", "friendly vehicle name, e.g. 'Volvo V70'")
		notes = fs.String("notes", "", "notes for this session, e.g. 'cold start, commute'")
		slowEvery = fs.Int("slow-every", 5, "poll slow PIDs (temperatures, voltage, LTFT) every N cycles")
		duration = fs.Duration("duration", 0, "stop logging after this long (0 = until Ctrl-C)")
	case "clear-dtcs":
		yes = fs.Bool("yes", false, "don't ask for confirmation")
	}
	fs.Parse(args)

	rw, desc, err := openAdapter(*port, *baud, *simSpeedup)
	if err != nil {
		return err
	}
	defer rw.Close()
	conn := elm327.New(rw)
	if err := conn.Init(ctx); err != nil {
		return fmt.Errorf("initialise adapter on %s: %w", desc, err)
	}
	log.Printf("connected: %s, %s, %s", desc, conn.Version, conn.Protocol)
	client := &obd.Client{R: conn, CAN: conn.Protocol.IsCAN()}

	switch cmd {
	case "scan":
		return scan(ctx, client)
	case "clear-dtcs":
		return clearDTCs(ctx, client, *yes)
	default:
		db, err := openDB(*dbPath)
		if err != nil {
			return err
		}
		defer db.Close()
		return logDrive(ctx, client, db, conn.Protocol.String(), *name, *notes, *slowEvery, *duration)
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

func logDrive(ctx context.Context, c *obd.Client, db *store.Store, protocol, name, notes string, slowEvery int, duration time.Duration) error {
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

func sessionsCmd(ctx context.Context, args []string) error {
	fs, dbPath := newFlags("sessions", "")
	vin := fs.String("vin", "", "only this vehicle")
	fs.Parse(args)
	db, err := openDB(*dbPath)
	if err != nil {
		return err
	}
	defer db.Close()

	vs, err := db.Vehicles(ctx)
	if err != nil {
		return err
	}
	for _, v := range vs {
		if *vin != "" && v.VIN != *vin {
			continue
		}
		fmt.Printf("%s  %s  (%s)\n", v.VIN, v.Name, v.Protocol)
		ss, err := db.Sessions(ctx, v.VIN, time.Time{}, time.Time{})
		if err != nil {
			return err
		}
		for _, s := range ss {
			dur := "logging"
			if !s.EndedAt.IsZero() {
				dur = s.EndedAt.Sub(s.StartedAt).Round(time.Second).String()
			}
			fmt.Printf("  #%-5d %s  %-10s %s\n", s.ID, s.StartedAt.Local().Format("2006-01-02 15:04"), dur, s.Notes)
		}
	}
	return nil
}

type driveFlags struct {
	db        *string
	session   *int64
	vin       *string
	baselineN *int
	minDur    *time.Duration
}

func newDriveFlags(name string) (*flag.FlagSet, driveFlags) {
	fs, db := newFlags(name, "")
	return fs, driveFlags{
		db:        db,
		session:   fs.Int64("session", 0, "session to analyse (default: latest)"),
		vin:       fs.String("vin", "", "with no -session: latest drive of this vehicle"),
		baselineN: fs.Int("baseline", 10, "number of earlier drives used as the baseline"),
		minDur:    fs.Duration("min-duration", 5*time.Minute, "ignore drives shorter than this for the baseline"),
	}
}

func (f driveFlags) analysis(ctx context.Context) (analyze.DriveAnalysis, error) {
	db, err := openDB(*f.db)
	if err != nil {
		return analyze.DriveAnalysis{}, err
	}
	defer db.Close()
	var sess store.Session
	if *f.session != 0 {
		sess, err = db.Session(ctx, *f.session)
	} else {
		sess, err = analyze.LatestSession(ctx, db, *f.vin)
	}
	if errors.Is(err, store.ErrNotFound) {
		return analyze.DriveAnalysis{}, errors.New("session not found")
	}
	if err != nil {
		return analyze.DriveAnalysis{}, err
	}
	return analyze.AnalyzeDrive(ctx, db, sess, *f.baselineN, *f.minDur)
}

func reportCmd(ctx context.Context, args []string) error {
	fs, f := newDriveFlags("report")
	out := fs.String("o", "-", "output file ('-' for stdout)")
	fs.Parse(args)
	a, err := f.analysis(ctx)
	if err != nil {
		return err
	}
	return write(*out, func(fh *os.File) error { return analyze.WriteReport(fh, a.Drive, a.Baseline) })
}

func summaryCmd(ctx context.Context, args []string) error {
	fs, f := newDriveFlags("summary")
	fs.Parse(args)
	a, err := f.analysis(ctx)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(a)
}

func trendCmd(ctx context.Context, args []string) error {
	fs, dbPath := newFlags("trend", "")
	vin := fs.String("vin", "", "vehicle (required when there is more than one)")
	since := fs.String("since", "", "first day to include (YYYY-MM-DD)")
	until := fs.String("until", "", "last day to include (YYYY-MM-DD)")
	minDur := fs.Duration("min-duration", 5*time.Minute, "ignore drives shorter than this")
	out := fs.String("o", "-", "output file ('-' for stdout)")
	fs.Parse(args)

	db, err := openDB(*dbPath)
	if err != nil {
		return err
	}
	defer db.Close()
	if *vin == "" {
		vs, err := db.Vehicles(ctx)
		if err != nil {
			return err
		}
		if len(vs) != 1 {
			return fmt.Errorf("%d vehicles in the database; pass -vin (see 'carsight sessions')", len(vs))
		}
		*vin = vs[0].VIN
	}
	s, err := parseDay(*since, false)
	if err != nil {
		return err
	}
	u, err := parseDay(*until, true)
	if err != nil {
		return err
	}
	t, err := analyze.AnalyzeTrends(ctx, db, *vin, s, u, *minDur)
	if err != nil {
		return err
	}
	if len(t.Drives) < 2 {
		return fmt.Errorf("%d usable drives in range; need at least 2", len(t.Drives))
	}
	name := t.Vehicle
	if name == "" {
		name = t.VIN
	}
	return write(*out, func(fh *os.File) error { return analyze.WriteTrendReport(fh, name, t.Summaries(), t.Trends) })
}

func mcpCmd(ctx context.Context, args []string) error {
	fs, dbPath := newFlags("mcp", "")
	fs.Parse(args)
	db, err := openDB(*dbPath)
	if err != nil {
		return err
	}
	defer db.Close()
	// stdout carries the protocol; logs go to stderr.
	return mcpserver.New(db, version()).Run(ctx, &mcp.StdioTransport{})
}

func version() string {
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" {
		return bi.Main.Version
	}
	return "dev"
}

// parseDay parses YYYY-MM-DD in local time; end makes it exclusive (the
// start of the following day).
func parseDay(s string, end bool) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	t, err := time.ParseInLocation(time.DateOnly, s, time.Local)
	if err != nil {
		return t, err
	}
	if end {
		t = t.AddDate(0, 0, 1)
	}
	return t, nil
}

func write(path string, fn func(*os.File) error) error {
	if path == "-" {
		return fn(os.Stdout)
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	if err := fn(f); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
