package car

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/jnszkr/carsight/internal/elm327"
	"github.com/jnszkr/carsight/internal/obd"
	"github.com/jnszkr/carsight/internal/store"
)

// DriveOptions describe a drive to log.
type DriveOptions struct {
	Name      string        // friendly vehicle name, e.g. "Volvo V70"; kept if empty
	DriveType string        // e.g. "commute", "highway" (see DriveTypes)
	Notes     string        // free-form notes
	SlowEvery int           // poll slow PIDs every N cycles (default 5)
	Duration  time.Duration // stop after this long; 0 runs until stopped
	// Logf, if set, receives progress messages.
	Logf func(format string, args ...any)
}

// Status is the live state of a drive being logged, or the final state of
// one that has stopped.
type Status struct {
	SessionID   int64              `json:"session_id"`
	VIN         string             `json:"vin"`
	Vehicle     string             `json:"vehicle,omitempty"`
	Protocol    string             `json:"protocol"`
	Adapter     string             `json:"adapter"`
	DriveType   string             `json:"drive_type,omitempty"`
	Notes       string             `json:"notes,omitempty"`
	PIDs        []string           `json:"pids"`
	StartedAt   time.Time          `json:"started_at"`
	Running     bool               `json:"running"`
	Elapsed     float64            `json:"elapsed_s"`
	Samples     int                `json:"samples"`
	CyclesPerS  float64            `json:"cycles_per_s"`
	ReadErrors  int                `json:"read_errors"`
	Latest      map[string]float64 `json:"latest"`
	StoredDTCs  []string           `json:"stored_dtcs"`
	PendingDTCs []string           `json:"pending_dtcs"`
	StopReason  string             `json:"stop_reason,omitempty"`
}

// Drive is a drive being logged in the background.
type Drive struct {
	car    *Car
	db     *store.Store
	opts   DriveOptions
	fast   []obd.PID
	slow   []obd.PID
	cancel context.CancelFunc
	done   chan struct{}

	mu     sync.Mutex
	st     Status
	cycles int
}

// StartDrive identifies the car, creates a session, records DTCs and
// readiness, and then logs samples in the background until Stop, the
// duration, ctx's cancellation or a lost connection. The Drive owns c and
// closes it when logging ends.
func StartDrive(ctx context.Context, c *Car, db *store.Store, o DriveOptions) (*Drive, error) {
	d, err := startDrive(ctx, c, db, o)
	if err != nil {
		c.Close()
		return nil, err
	}
	return d, nil
}

func startDrive(ctx context.Context, c *Car, db *store.Store, o DriveOptions) (*Drive, error) {
	if o.SlowEvery <= 0 {
		o.SlowEvery = 5
	}
	o.DriveType = NormalizeDriveType(o.DriveType)
	d := &Drive{car: c, db: db, opts: o, done: make(chan struct{})}
	vin, err := c.Client.VIN(ctx)
	if err != nil {
		// Some older ECUs don't support mode 09; fall back to a stable placeholder.
		d.logf("VIN unavailable (%v); logging under UNKNOWN", err)
		vin = "UNKNOWN"
	}
	if err := db.UpsertVehicle(ctx, store.Vehicle{VIN: vin, Name: o.Name, Protocol: c.Protocol()}); err != nil {
		return nil, err
	}
	veh, _ := db.Vehicle(ctx, vin)
	sup, err := c.Client.SupportedPIDs(ctx)
	if err != nil {
		return nil, fmt.Errorf("supported PIDs: %w", err)
	}
	pids := obd.Filter(obd.Default, sup)
	if len(pids) == 0 {
		return nil, errors.New("car supports none of the logged PIDs")
	}
	var names []string
	for _, p := range pids {
		names = append(names, p.Name)
		if p.Slow {
			d.slow = append(d.slow, p)
		} else {
			d.fast = append(d.fast, p)
		}
	}

	start := time.Now()
	id, err := db.StartSession(ctx, vin, c.Protocol(), start)
	if err != nil {
		return nil, err
	}
	if o.DriveType != "" || o.Notes != "" {
		if err := db.Describe(ctx, id, o.DriveType, o.Notes); err != nil {
			return nil, err
		}
	}
	d.st = Status{SessionID: id, VIN: vin, Vehicle: veh.Name, Protocol: c.Protocol(), Adapter: c.Desc,
		DriveType: o.DriveType, Notes: o.Notes, PIDs: names, StartedAt: start, Running: true,
		Latest: map[string]float64{}, StoredDTCs: []string{}, PendingDTCs: []string{}}
	d.logf("session %d for %s: logging %s", id, vin, strings.Join(names, ", "))

	d.readDTCs(ctx)
	if r, err := c.Client.ReadReadiness(ctx); err == nil {
		b, _ := json.Marshal(r)
		_ = db.SetReadiness(ctx, id, string(b))
	}

	// Logging outlives the setup call but still stops when ctx is cancelled.
	run, cancel := context.WithCancel(context.WithoutCancel(ctx))
	stop := context.AfterFunc(ctx, cancel)
	d.cancel = cancel
	go func() {
		defer stop()
		defer cancel()
		if o.Duration > 0 {
			var cancelTimeout context.CancelFunc
			run, cancelTimeout = context.WithTimeout(run, o.Duration)
			defer cancelTimeout()
		}
		d.run(run)
	}()
	return d, nil
}

func (d *Drive) logf(format string, args ...any) {
	if d.opts.Logf != nil {
		d.opts.Logf(format, args...)
	}
}

// readDTCs records stored and pending codes. They are read at the start and
// again at the end in case something set during the drive.
func (d *Drive) readDTCs(ctx context.Context) {
	for _, pending := range []bool{false, true} {
		kind := dtcKind(pending)
		codes, err := d.car.Client.DTCs(ctx, pending)
		if err != nil {
			d.logf("read %s DTCs: %v", kind, err)
			continue
		}
		if len(codes) > 0 {
			d.logf("%s DTCs: %s", kind, strings.Join(codes, ", "))
		}
		_ = d.db.AddDTCs(ctx, d.st.SessionID, kind, codes, time.Now())
		d.mu.Lock()
		if pending {
			d.st.PendingDTCs = mergeCodes(d.st.PendingDTCs, codes)
		} else {
			d.st.StoredDTCs = mergeCodes(d.st.StoredDTCs, codes)
		}
		d.mu.Unlock()
	}
}

func mergeCodes(have, add []string) []string {
	for _, c := range add {
		if !slices.Contains(have, c) {
			have = append(have, c)
		}
	}
	return have
}

func (d *Drive) run(ctx context.Context) {
	defer close(d.done)
	defer d.car.Close()
	var buf []store.Sample
	lastFlush, lastStatus := time.Now(), time.Now()
	flush := func(ctx context.Context) {
		if err := d.db.AddSamples(ctx, d.st.SessionID, buf); err != nil {
			d.logf("write samples: %v", err)
		}
		buf = buf[:0]
	}
	reason := "stopped"
	poll := func(p obd.PID) bool {
		v, err := d.car.Client.Read(ctx, p)
		if ctx.Err() != nil {
			return false
		}
		d.mu.Lock()
		defer d.mu.Unlock()
		if err != nil {
			d.st.ReadErrors++
			if errors.Is(err, elm327.ErrUnableConnect) {
				reason = "lost connection to the car: " + err.Error()
				d.logf("%s", reason)
				return false
			}
			return true
		}
		buf = append(buf, store.Sample{T: time.Now(), PID: p.Name, Value: v})
		d.st.Samples++
		d.st.Latest[p.Name] = math.Round(v*100) / 100
		return true
	}
loop:
	for {
		group := d.fast
		if d.cycles%d.opts.SlowEvery == 0 {
			group = append(append([]obd.PID{}, d.fast...), d.slow...)
		}
		for _, p := range group {
			if !poll(p) {
				break loop
			}
		}
		d.mu.Lock()
		d.cycles++
		d.mu.Unlock()
		if time.Since(lastFlush) > 2*time.Second {
			flush(ctx)
			lastFlush = time.Now()
		}
		if time.Since(lastStatus) > 30*time.Second {
			st := d.Status()
			d.logf("%s: %d samples, %.1f cycles/s, %d errors", time.Duration(st.Elapsed*float64(time.Second)).Round(time.Second),
				st.Samples, st.CyclesPerS, st.ReadErrors)
			lastStatus = time.Now()
		}
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		reason = "duration reached"
	}

	// The run context is done; finish up with a fresh one.
	end := time.Now()
	fin, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	flush(fin)
	d.readDTCs(fin)
	if err := d.db.EndSession(fin, d.st.SessionID, end); err != nil {
		reason += "; ending session: " + err.Error()
	}
	d.mu.Lock()
	d.st.Running = false
	d.st.StopReason = reason
	d.st.Elapsed = end.Sub(d.st.StartedAt).Seconds()
	d.mu.Unlock()
	st := d.Status()
	d.logf("session %d done (%s): %s, %d samples, %.1f cycles/s (fast group), %d read errors",
		st.SessionID, reason, end.Sub(st.StartedAt).Round(time.Second), st.Samples, st.CyclesPerS, st.ReadErrors)
}

// Status returns a snapshot of the drive.
func (d *Drive) Status() Status {
	d.mu.Lock()
	defer d.mu.Unlock()
	st := d.st
	if st.Running {
		st.Elapsed = time.Since(st.StartedAt).Seconds()
	}
	st.Elapsed = math.Round(st.Elapsed*10) / 10
	if st.Elapsed > 0 {
		st.CyclesPerS = math.Round(float64(d.cycles)/st.Elapsed*10) / 10
	}
	st.Latest = maps.Clone(st.Latest)
	st.PIDs = append([]string{}, st.PIDs...)
	st.StoredDTCs = append([]string{}, st.StoredDTCs...)
	st.PendingDTCs = append([]string{}, st.PendingDTCs...)
	return st
}

// Done is closed when logging has stopped and the session is saved.
func (d *Drive) Done() <-chan struct{} { return d.done }

// Stop stops logging, waits for the session to be saved and returns the
// final status.
func (d *Drive) Stop() Status {
	d.cancel()
	<-d.done
	return d.Status()
}
