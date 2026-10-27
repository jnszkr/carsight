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
	Name       string        // friendly vehicle name, e.g. "Volvo V70"; kept if empty
	DriveType  string        // e.g. "commute", "highway" (see DriveTypes)
	Notes      string        // free-form notes
	SlowEvery  int           // poll each slow PID about every N cycles (default 5)
	ExtraEvery int           // poll each extra PID about every N cycles (default 30)
	Duration   time.Duration // stop after this long; 0 runs until stopped
	// Logf, if set, receives progress messages.
	Logf func(format string, args ...any)
}

// Status is the live state of a drive being logged, or the final state of
// one that has stopped.
type Status struct {
	SessionID int64    `json:"session_id"`
	VIN       string   `json:"vin"`
	Vehicle   string   `json:"vehicle,omitempty"`
	Protocol  string   `json:"protocol"`
	Adapter   string   `json:"adapter"`
	DriveType string   `json:"drive_type,omitempty"`
	Notes     string   `json:"notes,omitempty"`
	PIDs      []string `json:"pids"`
	// Phase is "logging", "final snapshot" while the full state is read
	// at the end, then "done".
	Phase string `json:"phase"`
	// SnapshotDone says the start snapshot (every value, freeze frame,
	// vehicle info, monitor tests) is complete; it is read a request per
	// cycle alongside the polling.
	SnapshotDone bool               `json:"snapshot_done"`
	StartedAt    time.Time          `json:"started_at"`
	Running      bool               `json:"running"`
	Elapsed      float64            `json:"elapsed_s"`
	Samples      int                `json:"samples"`
	CyclesPerS   float64            `json:"cycles_per_s"`
	ReadErrors   int                `json:"read_errors"`
	Latest       map[string]float64 `json:"latest"`
	StoredDTCs   []string           `json:"stored_dtcs"`
	PendingDTCs  []string           `json:"pending_dtcs"`
	// PermanentDTCs can't be cleared; the ECU drops them once the fault
	// is verified fixed.
	PermanentDTCs []string `json:"permanent_dtcs"`
	StopReason    string   `json:"stop_reason,omitempty"`
}

// Drive is a drive being logged in the background.
type Drive struct {
	car    *Car
	db     *store.Store
	opts   DriveOptions
	sup    map[byte]bool
	groups [3][]obd.Group // fast, slow, extra
	cancel context.CancelFunc
	done   chan struct{}

	mu       sync.Mutex
	st       Status
	cycles   int
	logStart time.Time // when polling began, after the start snapshot
	subs     map[chan []store.Sample]struct{}
	buf      []store.Sample // read but not yet written to the store
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
	if o.ExtraEvery <= 0 {
		o.ExtraEvery = 30
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
	d.sup = sup
	var names []string
	for _, g := range obd.Groups(obd.Filter(obd.All, sup)) {
		if g.Rate == obd.Once {
			continue
		}
		d.groups[g.Rate] = append(d.groups[g.Rate], g)
		for _, p := range g.PIDs {
			names = append(names, p.Name)
		}
	}
	if len(names) == 0 {
		return nil, errors.New("car supports none of the logged PIDs")
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
		DriveType: o.DriveType, Notes: o.Notes, PIDs: names, Phase: "logging", StartedAt: start, Running: true,
		Latest: map[string]float64{}, StoredDTCs: []string{}, PendingDTCs: []string{}, PermanentDTCs: []string{}}
	d.logf("session %d for %s: logging %s", id, vin, strings.Join(names, ", "))

	d.readDTCs(ctx)

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

// saveSnapshot stores a finished snapshot in the session info.
func (d *Drive) saveSnapshot(ctx context.Context, info *obd.SessionInfo, snap *obd.Snapshot, end bool) {
	for _, e := range snap.Errors {
		d.logf("snapshot: %s", e)
	}
	if end {
		info.End = snap
	} else {
		info.Start = snap
		if snap.Readiness != nil {
			b, _ := json.Marshal(snap.Readiness)
			_ = d.db.SetReadiness(ctx, d.st.SessionID, string(b))
		}
	}
	if len(snap.PermanentDTCs) > 0 {
		_ = d.db.AddDTCs(ctx, d.st.SessionID, "permanent", snap.PermanentDTCs, snap.At)
		d.mu.Lock()
		d.st.PermanentDTCs = mergeCodes(d.st.PermanentDTCs, snap.PermanentDTCs)
		d.mu.Unlock()
	}
	b, err := json.Marshal(info)
	if err == nil {
		err = d.db.SetInfo(ctx, d.st.SessionID, string(b))
	}
	if err != nil {
		d.logf("store snapshot: %v", err)
	}
}

func (d *Drive) setPhase(p string) {
	d.mu.Lock()
	d.st.Phase = p
	d.mu.Unlock()
}

// maxGroupFailures is how many reads in a row of one PID may fail before it
// is dropped: a PID the car claims but never answers would otherwise cost
// a timeout every time.
const maxGroupFailures = 5

func (d *Drive) run(ctx context.Context) {
	defer close(d.done)
	defer d.car.Close()
	var info obd.SessionInfo
	snap := d.car.Client.NewSnapshot(d.sup, true)
	finishSnapshot := func(ctx context.Context) {
		d.saveSnapshot(ctx, &info, snap.Snapshot, false)
		d.mu.Lock()
		d.st.SnapshotDone = true
		d.mu.Unlock()
	}
	d.mu.Lock()
	d.logStart = time.Now()
	d.mu.Unlock()

	lastFlush, lastStatus := time.Now(), time.Now()
	flush := func(ctx context.Context) {
		d.mu.Lock()
		buf := d.buf
		d.buf = nil
		d.mu.Unlock()
		if err := d.db.AddSamples(ctx, d.st.SessionID, buf); err != nil {
			d.logf("write samples: %v", err)
		}
	}
	reason := "stopped"
	lost := false
	var cycle []store.Sample // this cycle's readings, for subscribers
	failures := map[byte]int{}
	dropped := map[byte]bool{}
	poll := func(g obd.Group) bool {
		vs, err := d.car.Client.ReadGroup(ctx, g)
		if ctx.Err() != nil {
			return false
		}
		d.mu.Lock()
		defer d.mu.Unlock()
		if err != nil {
			d.st.ReadErrors++
			if errors.Is(err, elm327.ErrUnableConnect) {
				reason = "lost connection to the car: " + err.Error()
				lost = true
				d.logf("%s", reason)
				return false
			}
			if failures[g.Code]++; failures[g.Code] >= maxGroupFailures {
				dropped[g.Code] = true
				d.logf("PID %02X failed %d times in a row; no longer polled (%v)", g.Code, maxGroupFailures, err)
			}
			return true
		}
		failures[g.Code] = 0
		now := time.Now()
		for i, p := range g.PIDs {
			if math.IsNaN(vs[i]) {
				continue
			}
			sm := store.Sample{T: now, PID: p.Name, Value: vs[i]}
			d.buf = append(d.buf, sm)
			cycle = append(cycle, sm)
			d.st.Samples++
			d.st.Latest[p.Name] = math.Round(vs[i]*100) / 100
		}
		return true
	}
	// Slow and extra PIDs are spread over the cycles in rotation, a few per
	// cycle, so no cycle stalls on a long batch.
	var next [3]int
	every := [3]int{1, d.opts.SlowEvery, d.opts.ExtraEvery}
	due := func(rate obd.Rate) []obd.Group {
		gs := d.groups[rate]
		if rate == obd.Fast {
			return gs
		}
		n := min(len(gs), (len(gs)+every[rate]-1)/every[rate])
		var out []obd.Group
		for range n {
			out = append(out, gs[next[rate]%len(gs)])
			next[rate]++
		}
		return out
	}
loop:
	for {
		cycle = nil
		for _, rate := range []obd.Rate{obd.Fast, obd.Slow, obd.Extra} {
			for _, g := range due(rate) {
				if dropped[g.Code] {
					continue
				}
				if !poll(g) {
					break loop
				}
			}
		}
		d.mu.Lock()
		d.cycles++
		d.publish(cycle)
		d.mu.Unlock()
		if !snap.Done() {
			if !snap.Step(ctx) && ctx.Err() == nil {
				finishSnapshot(ctx)
			}
		}
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
	fin, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	flush(fin)
	if !lost {
		d.setPhase("final snapshot")
		if !snap.Done() || info.Start == nil {
			for snap.Step(fin) {
			}
			finishSnapshot(fin)
		}
		d.readDTCs(fin)
		d.saveSnapshot(fin, &info, d.car.Client.TakeSnapshot(fin, d.sup, false), true)
	}
	if err := d.db.EndSession(fin, d.st.SessionID, end); err != nil {
		reason += "; ending session: " + err.Error()
	}
	d.mu.Lock()
	d.st.Running = false
	d.st.Phase = "done"
	d.st.StopReason = reason
	d.st.Elapsed = end.Sub(d.st.StartedAt).Seconds()
	d.mu.Unlock()
	st := d.Status()
	d.mu.Lock()
	for ch := range d.subs {
		close(ch)
	}
	d.subs = nil
	d.mu.Unlock()
	d.logf("session %d done (%s): %s, %d samples, %.1f cycles/s, %d read errors",
		st.SessionID, reason, end.Sub(st.StartedAt).Round(time.Second), st.Samples, st.CyclesPerS, st.ReadErrors)
}

// publish hands one cycle's readings to the subscribers. A subscriber that
// isn't keeping up misses the cycle rather than slowing the logger down.
// d.mu must be held.
func (d *Drive) publish(cycle []store.Sample) {
	for ch := range d.subs {
		select {
		case ch <- cycle:
		default:
		}
	}
}

// Subscribe returns a channel that receives the readings of each polling
// cycle from now on, and a function to unsubscribe. The first delivery also
// holds the readings not yet written to the store, so a subscriber that then
// loads the stored samples misses nothing. The channel is closed when
// logging stops (at once if it already has).
func (d *Drive) Subscribe() (<-chan []store.Sample, func()) {
	ch := make(chan []store.Sample, 64)
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.st.Running {
		close(ch)
		return ch, func() {}
	}
	if d.subs == nil {
		d.subs = map[chan []store.Sample]struct{}{}
	}
	d.subs[ch] = struct{}{}
	if len(d.buf) > 0 {
		ch <- slices.Clone(d.buf)
	}
	return ch, func() {
		d.mu.Lock()
		defer d.mu.Unlock()
		if _, ok := d.subs[ch]; ok {
			delete(d.subs, ch)
			close(ch)
		}
	}
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
	if !d.logStart.IsZero() {
		logEnd := time.Now()
		if !st.Running {
			logEnd = st.StartedAt.Add(time.Duration(st.Elapsed * float64(time.Second)))
		}
		if secs := logEnd.Sub(d.logStart).Seconds(); secs > 0 {
			st.CyclesPerS = math.Round(float64(d.cycles)/secs*10) / 10
		}
	}
	st.Latest = maps.Clone(st.Latest)
	st.PIDs = append([]string{}, st.PIDs...)
	st.StoredDTCs = append([]string{}, st.StoredDTCs...)
	st.PendingDTCs = append([]string{}, st.PendingDTCs...)
	st.PermanentDTCs = append([]string{}, st.PermanentDTCs...)
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
