package analyze

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/jnszkr/carsight/internal/store"
)

// DriveAnalysis is a drive summary plus its comparison against the car's
// baseline of earlier drives.
type DriveAnalysis struct {
	Drive         Summary `json:"drive"`
	BaselineDrive []int64 `json:"baseline_sessions"`
	Baseline      []Delta `json:"baseline_deltas"`
}

// Load summarises one session.
func Load(ctx context.Context, db *store.Store, sess store.Session) (Summary, error) {
	veh, err := db.Vehicle(ctx, sess.VIN)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return Summary{}, err
	}
	samples, err := db.Samples(ctx, sess.ID)
	if err != nil {
		return Summary{}, err
	}
	dtcs, err := db.DTCs(ctx, sess.ID)
	if err != nil {
		return Summary{}, err
	}
	return Summarize(sess, veh, samples, dtcs), nil
}

// LatestSession returns the most recent session, optionally for one vehicle.
func LatestSession(ctx context.Context, db *store.Store, vin string) (store.Session, error) {
	ss, err := db.Sessions(ctx, vin, time.Time{}, time.Time{})
	if err != nil {
		return store.Session{}, err
	}
	if len(ss) == 0 {
		return store.Session{}, store.ErrNotFound
	}
	return ss[0], nil
}

// usable reports whether a session is finished and long enough to count
// towards baselines and trends.
func usable(s store.Session, minDur time.Duration) bool {
	return !s.EndedAt.IsZero() && s.EndedAt.Sub(s.StartedAt) >= minDur
}

// AnalyzeDrive summarises a session and compares it with up to baselineN
// earlier drives of the same car that lasted at least minDur.
func AnalyzeDrive(ctx context.Context, db *store.Store, sess store.Session, baselineN int, minDur time.Duration) (DriveAnalysis, error) {
	sum, err := Load(ctx, db, sess)
	if err != nil {
		return DriveAnalysis{}, err
	}
	earlier, err := db.Sessions(ctx, sess.VIN, time.Time{}, sess.StartedAt)
	if err != nil {
		return DriveAnalysis{}, err
	}
	out := DriveAnalysis{Drive: sum, BaselineDrive: []int64{}}
	var base []Summary
	for _, s := range earlier {
		if len(base) >= baselineN {
			break
		}
		if s.ID == sess.ID || !usable(s, minDur) {
			continue
		}
		b, err := Load(ctx, db, s)
		if err != nil {
			return DriveAnalysis{}, err
		}
		base = append(base, b)
		out.BaselineDrive = append(out.BaselineDrive, s.ID)
	}
	out.Baseline = Compare(sum, base)
	if out.Baseline == nil {
		out.Baseline = []Delta{}
	}
	return out, nil
}

// TrendDrive is the per-drive slice of a trend analysis.
type TrendDrive struct {
	SessionID  int64              `json:"session_id"`
	Start      time.Time          `json:"start"`
	Duration   Duration           `json:"duration_s"`
	DistanceKm float64            `json:"distance_km"`
	DriveType  string             `json:"drive_type,omitempty"`
	ColdStart  bool               `json:"cold_start"`
	Metrics    map[string]float64 `json:"metrics"`
	Events     []string           `json:"events"`
}

// TrendAnalysis is a car's key metrics across drives.
type TrendAnalysis struct {
	VIN     string       `json:"vin"`
	Vehicle string       `json:"vehicle,omitempty"`
	Drives  []TrendDrive `json:"drives"` // chronological
	Trends  []TrendLine  `json:"trends"`

	summaries []Summary
}

// Summaries returns the full drive summaries behind the analysis.
func (t TrendAnalysis) Summaries() []Summary { return t.summaries }

// TrendFilter selects the drives of one car for a trend analysis.
type TrendFilter struct {
	VIN          string
	Since, Until time.Time     // [Since, Until); zero is unbounded
	MinDuration  time.Duration // ignore shorter drives
	DriveType    string        // only drives of this type; empty matches all
}

// AnalyzeTrends loads the usable drives matching f and fits trends through
// their key metrics.
func AnalyzeTrends(ctx context.Context, db *store.Store, f TrendFilter) (TrendAnalysis, error) {
	vin := f.VIN
	out := TrendAnalysis{VIN: vin, Drives: []TrendDrive{}, Trends: []TrendLine{}}
	if v, err := db.Vehicle(ctx, vin); err == nil {
		out.Vehicle = v.Name
	} else if !errors.Is(err, store.ErrNotFound) {
		return out, err
	}
	ss, err := db.Sessions(ctx, vin, f.Since, f.Until)
	if err != nil {
		return out, err
	}
	slices.Reverse(ss) // chronological
	for _, s := range ss {
		if !usable(s, f.MinDuration) || f.DriveType != "" && s.DriveType != f.DriveType {
			continue
		}
		d, err := Load(ctx, db, s)
		if err != nil {
			return out, err
		}
		out.summaries = append(out.summaries, d)
		td := TrendDrive{SessionID: d.SessionID, Start: d.Start, Duration: d.Duration, DistanceKm: d.DistanceKm,
			DriveType: d.DriveType, ColdStart: d.WarmUp.ColdStart, Metrics: d.Metrics, Events: []string{}}
		for _, e := range d.Events {
			td.Events = append(td.Events, e.Severity+": "+e.Detail)
		}
		out.Drives = append(out.Drives, td)
	}
	if t := Trends(out.summaries); t != nil {
		out.Trends = t
	}
	return out, nil
}
