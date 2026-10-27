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
	"github.com/jnszkr/carsight/internal/car"
	"github.com/jnszkr/carsight/internal/mcpserver"
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
  mcp         serve carsight to an AI assistant over MCP (stdio): analysis,
              raw data, and starting/stopping drives

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

// adapterFlags adds the flags that select the adapter.
func adapterFlags(fs *flag.FlagSet) *car.Adapter {
	a := &car.Adapter{}
	fs.StringVar(&a.Port, "port", "auto", "serial device, 'auto', or 'sim:volvo' / 'sim:citroen' for the simulator")
	fs.IntVar(&a.Baud, "baud", 0, "baud rate (0 = auto-detect)")
	fs.Float64Var(&a.SimSpeedup, "sim-speedup", 1, "simulator: run simulated time this many times faster")
	return a
}

func carCommand(ctx context.Context, cmd string, args []string) error {
	fs, dbPath := newFlags(cmd, "")
	adapter := adapterFlags(fs)
	var opts car.DriveOptions
	var yes *bool
	switch cmd {
	case "log":
		fs.StringVar(&opts.Name, "name", "", "friendly vehicle name, e.g. 'Volvo V70'")
		fs.StringVar(&opts.DriveType, "type", "", "drive type: "+strings.Join(slices.Sorted(maps.Keys(car.DriveTypes)), ", ")+" (or your own)")
		fs.StringVar(&opts.Notes, "notes", "", "notes for this session, e.g. 'rough idle after refuelling'")
		fs.IntVar(&opts.SlowEvery, "slow-every", 5, "poll slow PIDs (temperatures, voltage, LTFT) every N cycles")
		fs.DurationVar(&opts.Duration, "duration", 0, "stop logging after this long (0 = until Ctrl-C)")
	case "clear-dtcs":
		yes = fs.Bool("yes", false, "don't ask for confirmation")
	}
	fs.Parse(args)

	c, err := car.Connect(ctx, *adapter)
	if err != nil {
		return err
	}
	log.Printf("connected: %s, %s, %s", c.Desc, c.Conn.Version, c.Protocol())

	switch cmd {
	case "scan":
		defer c.Close()
		return scan(ctx, c)
	case "clear-dtcs":
		defer c.Close()
		return clearDTCs(ctx, c, *yes)
	default:
		db, err := openDB(*dbPath)
		if err != nil {
			c.Close()
			return err
		}
		defer db.Close()
		opts.Logf = log.Printf
		d, err := car.StartDrive(ctx, c, db, opts)
		if err != nil {
			return err
		}
		log.Print("logging; press Ctrl-C to stop")
		<-d.Done()
		fmt.Println(d.Status().SessionID)
		return nil
	}
}

func scan(ctx context.Context, c *car.Car) error {
	s, err := c.Scan(ctx)
	if s.VIN != "" {
		fmt.Printf("VIN: %s\n", s.VIN)
	} else {
		fmt.Printf("VIN: unavailable (%s)\n", s.VINError)
	}
	if err != nil {
		return err
	}
	fmt.Println("\nLogged PIDs:")
	for _, p := range s.LoggedPIDs {
		mark := "no "
		if p.Supported {
			mark = "yes"
		}
		fmt.Printf("  %s  %s %-16s %s\n", mark, p.PID[2:], p.Name, p.Desc)
	}
	fmt.Printf("\nAll supported mode 01 PIDs: %s\n", strings.Join(s.AllPIDs, " "))
	for _, e := range s.DTCErrors {
		fmt.Printf("\nDTC read error: %s\n", e)
	}
	for _, kc := range []struct {
		kind  string
		codes []string
	}{{"Stored", s.StoredDTCs}, {"Pending", s.PendingDTCs}} {
		kind, codes := kc.kind, kc.codes
		if len(codes) == 0 {
			fmt.Printf("\n%s DTCs: none\n", kind)
		} else {
			fmt.Printf("\n%s DTCs: %s\n", kind, strings.Join(codes, ", "))
		}
	}
	if r := s.Readiness; r != nil {
		fmt.Printf("\nMIL: %v, DTC count: %d\nMonitors:\n", r.MIL, r.DTCCount)
		for _, k := range slices.Sorted(maps.Keys(r.Monitors)) {
			fmt.Printf("  %-18s %s\n", k, r.Monitors[k])
		}
	}
	return nil
}

func clearDTCs(ctx context.Context, c *car.Car, yes bool) error {
	codes, err := c.Client.DTCs(ctx, false)
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
	if _, err := c.ClearDTCs(ctx); err != nil {
		return err
	}
	fmt.Println("DTCs cleared.")
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
			fmt.Printf("  #%-5d %s  %-10s %-11s %s\n", s.ID, s.StartedAt.Local().Format("2006-01-02 15:04"), dur, s.DriveType, s.Notes)
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
	driveType := fs.String("type", "", "only drives of this type, e.g. commute")
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
	t, err := analyze.AnalyzeTrends(ctx, db, analyze.TrendFilter{VIN: *vin, Since: s, Until: u, MinDuration: *minDur,
		DriveType: car.NormalizeDriveType(*driveType)})
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
	adapter := adapterFlags(fs)
	fs.Parse(args)
	db, err := openDB(*dbPath)
	if err != nil {
		return err
	}
	defer db.Close()
	srv := mcpserver.New(db, mcpserver.Config{Version: version(), Adapter: *adapter})
	// Ending the session (the client exits) stops and saves a drive in progress.
	defer srv.Close()
	// stdout carries the protocol; logs go to stderr.
	return srv.MCP.Run(ctx, &mcp.StdioTransport{})
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
