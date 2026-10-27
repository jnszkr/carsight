// Command carsight-analyze summarises logged drives, compares them against
// the car's baseline, optionally asks Claude to interpret them, and writes
// Markdown reports.
//
//	carsight-analyze -list                         list vehicles and sessions
//	carsight-analyze [-session N] [-ai]            report on one drive (default: latest)
//	carsight-analyze -trend [-vin V] [-since D]    trend report across drives
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/jnszkr/carsight/internal/analyze"
	"github.com/jnszkr/carsight/internal/store"
)

func main() {
	var (
		dbPath    = flag.String("db", "carsight.db", "SQLite database path")
		sessionID = flag.Int64("session", 0, "session to analyse (default: latest)")
		vin       = flag.String("vin", "", "restrict to this vehicle")
		since     = flag.String("since", "", "trend: first day to include (YYYY-MM-DD)")
		until     = flag.String("until", "", "trend: last day to include (YYYY-MM-DD)")
		trend     = flag.Bool("trend", false, "write a trend report across drives instead of a single-drive report")
		list      = flag.Bool("list", false, "list vehicles and sessions")
		useAI     = flag.Bool("ai", false, "ask Claude to analyse the drive (needs ANTHROPIC_API_KEY)")
		model     = flag.String("model", analyze.DefaultModel, "Claude model")
		baselineN = flag.Int("baseline", 10, "number of earlier drives used as the baseline")
		minDur    = flag.Duration("min-duration", 5*time.Minute, "ignore drives shorter than this for baselines and trends")
		outDir    = flag.String("out", "reports", "directory for Markdown reports ('-' for stdout)")
		asJSON    = flag.Bool("json", false, "print the drive summary as JSON instead of writing a report")
	)
	flag.Parse()
	ctx := context.Background()

	db, err := store.Open(*dbPath)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	var ai *analyze.Client
	if *useAI {
		ai = analyze.NewClient()
		ai.Model = *model
	}

	switch {
	case *list:
		err = listAll(ctx, db, *vin)
	case *trend:
		err = trendReport(ctx, db, ai, *vin, *since, *until, *minDur, *outDir)
	default:
		err = driveReport(ctx, db, ai, *sessionID, *vin, *baselineN, *minDur, *outDir, *asJSON)
	}
	if err != nil {
		log.Fatal(err)
	}
}

func listAll(ctx context.Context, db *store.Store, vin string) error {
	vs, err := db.Vehicles(ctx)
	if err != nil {
		return err
	}
	for _, v := range vs {
		if vin != "" && v.VIN != vin {
			continue
		}
		fmt.Printf("%s  %s  (%s)\n", v.VIN, v.Name, v.Protocol)
		ss, err := db.Sessions(ctx, v.VIN, time.Time{}, time.Time{})
		if err != nil {
			return err
		}
		for _, s := range ss {
			dur := "running"
			if !s.EndedAt.IsZero() {
				dur = s.EndedAt.Sub(s.StartedAt).Round(time.Second).String()
			}
			fmt.Printf("  #%-5d %s  %-10s %s\n", s.ID, s.StartedAt.Local().Format("2006-01-02 15:04"), dur, s.Notes)
		}
	}
	return nil
}

func load(ctx context.Context, db *store.Store, sess store.Session) (analyze.Summary, error) {
	veh, err := db.Vehicle(ctx, sess.VIN)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return analyze.Summary{}, err
	}
	samples, err := db.Samples(ctx, sess.ID)
	if err != nil {
		return analyze.Summary{}, err
	}
	dtcs, err := db.DTCs(ctx, sess.ID)
	if err != nil {
		return analyze.Summary{}, err
	}
	return analyze.Summarize(sess, veh, samples, dtcs), nil
}

func driveReport(ctx context.Context, db *store.Store, ai *analyze.Client, id int64, vin string, baselineN int, minDur time.Duration, outDir string, asJSON bool) error {
	var sess store.Session
	var err error
	if id != 0 {
		sess, err = db.Session(ctx, id)
	} else {
		var ss []store.Session
		ss, err = db.Sessions(ctx, vin, time.Time{}, time.Time{})
		if err == nil && len(ss) == 0 {
			err = errors.New("no sessions logged yet")
		}
		if err == nil {
			sess = ss[0]
		}
	}
	if err != nil {
		return err
	}
	sum, err := load(ctx, db, sess)
	if err != nil {
		return err
	}

	// Baseline: the most recent earlier drives of the same car.
	earlier, err := db.Sessions(ctx, sess.VIN, time.Time{}, sess.StartedAt)
	if err != nil {
		return err
	}
	var base []analyze.Summary
	for _, s := range earlier {
		if len(base) >= baselineN {
			break
		}
		if s.ID == sess.ID || s.EndedAt.IsZero() || s.EndedAt.Sub(s.StartedAt) < minDur {
			continue
		}
		b, err := load(ctx, db, s)
		if err != nil {
			return err
		}
		base = append(base, b)
	}
	deltas := analyze.Compare(sum, base)

	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(analyze.DriveInput{Drive: sum, Baseline: deltas})
	}

	var text string
	if ai != nil {
		log.Printf("asking %s to analyse session %d (%d baseline drives)...", ai.Model, sess.ID, len(base))
		if text, err = ai.AnalyzeDrive(ctx, sum, deltas); err != nil {
			return err
		}
	}
	return write(outDir, fmt.Sprintf("%s-session-%d.md", sess.VIN, sess.ID), func(f *os.File) error {
		return analyze.WriteReport(f, sum, deltas, text)
	})
}

func trendReport(ctx context.Context, db *store.Store, ai *analyze.Client, vin, sinceS, untilS string, minDur time.Duration, outDir string) error {
	since, err := parseDay(sinceS)
	if err != nil {
		return err
	}
	until, err := parseDay(untilS)
	if err != nil {
		return err
	}
	if !until.IsZero() {
		until = until.AddDate(0, 0, 1)
	}
	vs, err := db.Vehicles(ctx)
	if err != nil {
		return err
	}
	for _, v := range vs {
		if vin != "" && v.VIN != vin {
			continue
		}
		ss, err := db.Sessions(ctx, v.VIN, since, until)
		if err != nil {
			return err
		}
		slices.Reverse(ss) // chronological
		var drives []analyze.Summary
		for _, s := range ss {
			if s.EndedAt.IsZero() || s.EndedAt.Sub(s.StartedAt) < minDur {
				continue
			}
			d, err := load(ctx, db, s)
			if err != nil {
				return err
			}
			drives = append(drives, d)
		}
		if len(drives) < 2 {
			log.Printf("%s: %d usable drives in range; need at least 2 for trends", v.VIN, len(drives))
			continue
		}
		trends := analyze.Trends(drives)
		name := v.Name
		if name == "" {
			name = v.VIN
		}
		var text string
		if ai != nil {
			log.Printf("asking %s to analyse trends for %s (%d drives)...", ai.Model, name, len(drives))
			if text, err = ai.AnalyzeTrends(ctx, name, drives, trends); err != nil {
				return err
			}
		}
		if err := write(outDir, v.VIN+"-trends.md", func(f *os.File) error {
			return analyze.WriteTrendReport(f, name, drives, trends, text)
		}); err != nil {
			return err
		}
	}
	return nil
}

func parseDay(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	return time.ParseInLocation(time.DateOnly, s, time.Local)
}

func write(dir, name string, fn func(*os.File) error) error {
	if dir == "-" {
		return fn(os.Stdout)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	path := filepath.Join(dir, name)
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	if err := fn(f); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	fmt.Println(path)
	return nil
}
