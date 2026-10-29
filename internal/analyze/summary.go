// Package analyze reduces a logged drive to a compact summary (statistics,
// warm-up behaviour, notable events, DTCs), compares it against the car's
// baseline, renders Markdown reports and asks Claude to interpret them.
package analyze

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/jnszkr/carsight/internal/obd"
	"github.com/jnszkr/carsight/internal/store"
)

// Thresholds used to flag events. Engine "warm" means coolant ≥ WarmC.
const (
	WarmC              = 75.0
	OverheatC          = 105.0
	LowVoltageRunning  = 13.0
	HighVoltage        = 15.0
	TrimWarnPct        = 10.0
	TrimSeverePct      = 20.0
	IdleUnstableStdRPM = 50.0
	MaxIdleRPM         = 1300.0
	SlowWarmup         = 15 * time.Minute
	minEventDuration   = 10 * time.Second
	idleSettle         = 5 * time.Second
)

// Summary is the compact description of one drive. It is what gets sent to
// the model; raw samples never are.
type Summary struct {
	SessionID   int64              `json:"session_id"`
	VIN         string             `json:"vin"`
	Vehicle     string             `json:"vehicle,omitempty"`
	Protocol    string             `json:"protocol,omitempty"`
	DriveType   string             `json:"drive_type,omitempty"`
	Notes       string             `json:"notes,omitempty"`
	Software    string             `json:"logged_by,omitempty"` // carsight version that logged the drive
	Adapter     string             `json:"adapter,omitempty"`
	Start       time.Time          `json:"start"`
	Duration    Duration           `json:"duration"`
	DistanceKm  float64            `json:"distance_km"`
	SampleCount int                `json:"sample_count"`
	RPMRateHz   float64            `json:"rpm_sample_rate_hz"`
	PIDs        map[string]Stats   `json:"pids"`
	WarmUp      WarmUp             `json:"warm_up"`
	Idle        *Idle              `json:"idle,omitempty"`
	FuelTrim    *FuelTrim          `json:"fuel_trim,omitempty"`
	O2B1S1Warm  *Stats             `json:"o2_b1s1_warm,omitempty"`
	VoltageRun  *Stats             `json:"voltage_running,omitempty"`
	Events      []Event            `json:"events"`
	DTCs        []DTCEntry         `json:"dtcs"`
	Readiness   *obd.Readiness     `json:"readiness,omitempty"`
	Metrics     map[string]float64 `json:"metrics"`
	// Snapshots are the full state read at the start and end of the drive.
	// Monitor tests are trimmed to failures and misfire counts.
	Snapshots *obd.SessionInfo `json:"snapshots,omitempty"`
	// Misfires is the misfire count per cylinder in the drive cycle, from
	// the misfire monitor (mode 06), when the car reports it.
	Misfires map[string]float64 `json:"misfires_by_cylinder,omitempty"`
}

// Duration marshals as seconds.
type Duration time.Duration

func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(math.Round(time.Duration(d).Seconds()))
}

func (d Duration) String() string { return time.Duration(d).Round(time.Second).String() }

// WarmUp describes how the coolant warmed.
type WarmUp struct {
	StartC     float64   `json:"start_c"`
	MaxC       float64   `json:"max_c"`
	ColdStart  bool      `json:"cold_start"` // started below 40 °C
	ToWarm     *Duration `json:"time_to_75c,omitempty"`
	Reached75C bool      `json:"reached_75c"`
}

// Idle describes warm idle behaviour (stationary, engine running).
type Idle struct {
	Seconds float64 `json:"seconds"`
	RPM     Stats   `json:"rpm"`
	MAF     *Stats  `json:"maf,omitempty"`
	Timing  *Stats  `json:"timing_advance,omitempty"`
}

// FuelTrim describes warm fuel trims, bank 1.
type FuelTrim struct {
	STFT        Stats   `json:"stft"`
	LTFT        Stats   `json:"ltft"`
	TotalIdle   float64 `json:"total_idle"`
	TotalCruise float64 `json:"total_cruise"`
}

// Event is a notable interval in the drive.
type Event struct {
	Kind     string   `json:"kind"`
	Severity string   `json:"severity"` // info | warn | high
	At       Duration `json:"at"`       // offset from session start
	Lasted   Duration `json:"lasted"`
	Detail   string   `json:"detail"`
}

// DTCEntry is a trouble code with its kind.
type DTCEntry struct {
	Code string `json:"code"`
	Kind string `json:"kind"`
}

// Summarize computes the summary of a session.
func Summarize(sess store.Session, veh store.Vehicle, samples []store.Sample, dtcs []store.DTC) Summary {
	sum := Summary{
		SessionID: sess.ID, VIN: sess.VIN, Vehicle: veh.Name, Protocol: sess.Protocol, DriveType: sess.DriveType, Notes: sess.Notes,
		Software: sess.Software, Adapter: sess.Adapter, Start: sess.StartedAt, SampleCount: len(samples), PIDs: map[string]Stats{}, Events: []Event{},
		DTCs: []DTCEntry{}, Metrics: map[string]float64{},
	}
	end := sess.EndedAt
	if len(samples) > 0 && (end.IsZero() || samples[len(samples)-1].T.After(end)) {
		end = samples[len(samples)-1].T
	}
	if !end.IsZero() {
		sum.Duration = Duration(end.Sub(sess.StartedAt))
	}
	// Codes are read at the start and the end of a drive; list each once.
	seen := map[DTCEntry]bool{}
	for _, d := range dtcs {
		e := DTCEntry{d.Code, d.Kind}
		if !seen[e] {
			seen[e] = true
			sum.DTCs = append(sum.DTCs, e)
		}
	}
	if sess.Readiness != "" {
		var r obd.Readiness
		if json.Unmarshal([]byte(sess.Readiness), &r) == nil {
			sum.Readiness = &r
		}
	}

	if sess.Info != "" {
		var info obd.SessionInfo
		if json.Unmarshal([]byte(sess.Info), &info) == nil {
			sum.Snapshots = &info
		}
	}

	g := group(samples)
	for name, s := range g {
		sum.PIDs[name] = stats(s.v)
	}
	rpm, speed, coolant := g[obd.RPM.Name], g[obd.Speed.Name], g[obd.CoolantTemp.Name]
	if rpm != nil && len(rpm.t) > 1 {
		span := rpm.t[len(rpm.t)-1].Sub(rpm.t[0]).Seconds()
		if span > 0 {
			sum.RPMRateHz = round(float64(len(rpm.t)-1)/span, 2)
		}
	}

	running := func(t time.Time) bool { v, ok := rpm.at(t); return ok && v > 400 }
	warm := func(t time.Time) bool { v, ok := coolant.at(t); return ok && v >= WarmC }
	stopped := func(t time.Time) bool { v, ok := speed.at(t); return ok && v < 1 }
	moving := func(t time.Time) bool { v, ok := speed.at(t); return ok && v >= 20 }

	// Distance: integrate speed (km/h) over time.
	if speed != nil {
		for i := 1; i < len(speed.t); i++ {
			dt := speed.t[i].Sub(speed.t[i-1])
			if dt <= maxStale {
				sum.DistanceKm += speed.v[i-1] * dt.Hours()
			}
		}
		sum.DistanceKm = round(sum.DistanceKm, 2)
	}

	// Warm-up.
	if coolant != nil && len(coolant.v) > 0 {
		w := WarmUp{StartC: coolant.v[0], MaxC: sum.PIDs[obd.CoolantTemp.Name].Max, ColdStart: coolant.v[0] < 40}
		for i, v := range coolant.v {
			if v >= WarmC {
				w.Reached75C = true
				d := Duration(coolant.t[i].Sub(coolant.t[0]))
				w.ToWarm = &d
				break
			}
		}
		sum.WarmUp = w
		if w.ToWarm != nil && w.ColdStart {
			sum.Metrics["warmup_to_75c_s"] = round(time.Duration(*w.ToWarm).Seconds(), 0)
		}
		sum.Metrics["coolant_max_c"] = w.MaxC
		if w.ColdStart && time.Duration(sum.Duration) > SlowWarmup {
			if !w.Reached75C {
				sum.Events = append(sum.Events, Event{Kind: "slow_warmup", Severity: "warn", At: 0, Lasted: sum.Duration,
					Detail: fmt.Sprintf("coolant never reached %.0f °C in %s (max %.0f °C); possible thermostat stuck open", WarmC, sum.Duration, w.MaxC)})
			} else if time.Duration(*w.ToWarm) > SlowWarmup {
				sum.Events = append(sum.Events, Event{Kind: "slow_warmup", Severity: "warn", At: 0, Lasted: *w.ToWarm,
					Detail: fmt.Sprintf("took %s to reach %.0f °C from a cold start", *w.ToWarm, WarmC)})
			}
		}
	}

	// Warm idle: stationary, foot off the throttle, and RPM in idle range so
	// rolling to a stop with the clutch in doesn't count. RPM must also stay in
	// idle range for idleSettle either side, so a blip of the throttle and the
	// RPM falling back from it don't count either (diesels often report no
	// throttle position).
	throttle := g[obd.Throttle.Name]
	idleCond := func(t time.Time) bool {
		if !running(t) || !warm(t) || !stopped(t) {
			return false
		}
		if v, ok := rpm.at(t); !ok || v > MaxIdleRPM || rpm.anyWithin(t, idleSettle, func(v float64) bool { return v > MaxIdleRPM }) {
			return false
		}
		v, ok := throttle.at(t)
		return !ok || v < 5
	}
	if r := rpm.where(idleCond); len(r) >= 5 {
		idle := &Idle{RPM: stats(r), Seconds: round(span(rpm, idleCond), 0)}
		if v := g[obd.MAF.Name].where(idleCond); len(v) > 0 {
			s := stats(v)
			idle.MAF = &s
			sum.Metrics["idle_maf_gs"] = round(s.Mean, 2)
		}
		if v := g[obd.TimingAdvance.Name].where(idleCond); len(v) > 0 {
			s := stats(v)
			idle.Timing = &s
			sum.Metrics["idle_timing_deg"] = round(s.Mean, 1)
		}
		sum.Idle = idle
		sum.Metrics["idle_rpm_mean"] = round(idle.RPM.Mean, 0)
		sum.Metrics["idle_rpm_std"] = round(idle.RPM.Std, 1)
		if idle.RPM.Std > IdleUnstableStdRPM {
			sum.Events = append(sum.Events, Event{Kind: "unstable_idle", Severity: "warn", Lasted: Duration(time.Duration(idle.Seconds) * time.Second),
				Detail: fmt.Sprintf("warm idle RPM std dev %.0f rpm (mean %.0f)", idle.RPM.Std, idle.RPM.Mean)})
		}
	}

	// Fuel trims (warm, closed loop approximated by warm + running).
	stft, ltft := g[obd.STFTBank1.Name], g[obd.LTFTBank1.Name]
	warmRunning := func(t time.Time) bool { return running(t) && warm(t) }
	if s := stft.where(warmRunning); len(s) >= 5 {
		ft := &FuelTrim{STFT: stats(s), LTFT: stats(ltft.where(warmRunning))}
		ft.TotalIdle = round(totalTrim(stft, ltft, idleCond), 1)
		ft.TotalCruise = round(totalTrim(stft, ltft, func(t time.Time) bool { return warmRunning(t) && moving(t) }), 1)
		sum.FuelTrim = ft
		sum.Metrics["stft_warm_mean"] = round(ft.STFT.Mean, 1)
		sum.Metrics["ltft_warm_mean"] = round(ft.LTFT.Mean, 1)
		sum.Metrics["trim_total_idle"] = ft.TotalIdle
		sum.Metrics["trim_total_cruise"] = ft.TotalCruise
		for _, c := range []struct {
			where string
			v     float64
		}{{"idle", ft.TotalIdle}, {"cruise", ft.TotalCruise}} {
			if math.Abs(c.v) >= TrimWarnPct {
				sev, dir := "warn", "lean (adding fuel)"
				if math.Abs(c.v) >= TrimSeverePct {
					sev = "high"
				}
				if c.v < 0 {
					dir = "rich (removing fuel)"
				}
				sum.Events = append(sum.Events, Event{Kind: "fuel_trim", Severity: sev,
					Detail: fmt.Sprintf("total fuel trim %+.1f%% at %s: running %s", c.v, c.where, dir)})
			}
		}
	}

	// O2 sensor activity when warm.
	if v := g[obd.O2B1S1.Name].where(warmRunning); len(v) >= 10 {
		s := stats(v)
		sum.O2B1S1Warm = &s
		sum.Metrics["o2_b1s1_std"] = round(s.Std, 3)
		if s.Max-s.Min < 0.3 {
			sum.Events = append(sum.Events, Event{Kind: "o2_inactive", Severity: "warn",
				Detail: fmt.Sprintf("upstream O2 sensor barely switching when warm (%.2f–%.2f V)", s.Min, s.Max)})
		}
	}

	// Charging voltage.
	volt := g[obd.ModuleVoltage.Name]
	if v := volt.where(running); len(v) > 0 {
		s := stats(v)
		sum.VoltageRun = &s
		sum.Metrics["voltage_running_mean"] = round(s.Mean, 2)
	}
	sum.Events = append(sum.Events, intervals(volt, sess.StartedAt, func(t time.Time, v float64) bool { return running(t) && v < LowVoltageRunning },
		"low_voltage", "warn", "control module voltage below %.1f V with engine running (min %.2f V)", LowVoltageRunning, true)...)
	sum.Events = append(sum.Events, intervals(volt, sess.StartedAt, func(_ time.Time, v float64) bool { return v > HighVoltage },
		"high_voltage", "warn", "control module voltage above %.1f V (max %.2f V)", HighVoltage, false)...)
	sum.Events = append(sum.Events, intervals(coolant, sess.StartedAt, func(_ time.Time, v float64) bool { return v > OverheatC },
		"overheat", "high", "coolant above %.0f °C (max %.0f °C)", OverheatC, false)...)

	for _, d := range sum.DTCs {
		sev := "warn"
		if d.Kind == "pending" {
			sev = "info"
		}
		sum.Events = append(sum.Events, Event{Kind: "dtc", Severity: sev, Detail: fmt.Sprintf("%s trouble code %s", d.Kind, d.Code)})
	}
	if sum.Readiness != nil && sum.Readiness.MIL {
		sum.Events = append(sum.Events, Event{Kind: "mil_on", Severity: "high", Detail: "check engine light (MIL) is on"})
	}
	if fs := g["fuel_system"]; fs != nil {
		for i, v := range fs.v {
			if v == 8 || v == 16 {
				sum.Events = append(sum.Events, Event{Kind: "fuel_system_fault", Severity: "warn", At: Duration(fs.t[i].Sub(sess.StartedAt)),
					Detail: "fuel system reported " + obd.FuelSystemLabels[int(v)]})
				break
			}
		}
	}
	sum.snapshotEvents()
	sort.SliceStable(sum.Events, func(i, j int) bool { return sevRank(sum.Events[i].Severity) > sevRank(sum.Events[j].Severity) })
	return sum
}

// ClearedRecentlyKm and ClearedRecentlyWarmups: below either, trouble
// codes were cleared too recently for readiness and stored codes to tell
// the whole story.
const (
	ClearedRecentlyKm      = 100
	ClearedRecentlyWarmups = 5
)

// snapshotEvents flags what the start and end snapshots show and trims
// their monitor tests to the ones worth reading.
func (sum *Summary) snapshotEvents() {
	info := sum.Snapshots
	if info == nil {
		return
	}
	if s := info.Start; s != nil {
		km, okKm := s.Values["distance_since_clear"]
		wu, okWu := s.Values["warmups_since_clear"]
		if okKm && km < ClearedRecentlyKm || okWu && wu < ClearedRecentlyWarmups {
			sum.Events = append(sum.Events, Event{Kind: "codes_cleared_recently", Severity: "info",
				Detail: fmt.Sprintf("trouble codes were cleared recently (%.0f km, %.0f warm-ups ago); readiness and stored codes may not show the whole picture", km, wu)})
		}
		if ff := s.FreezeFrame; ff != nil {
			var parts []string
			for _, k := range []string{"rpm", "speed", "coolant_temp", "engine_load", "stft_b1", "ltft_b1"} {
				if v, ok := ff.Values[k]; ok {
					parts = append(parts, fmt.Sprintf("%s %.4g", k, v))
				}
			}
			sum.Events = append(sum.Events, Event{Kind: "freeze_frame", Severity: "info",
				Detail: fmt.Sprintf("freeze frame stored with %s: %s", ff.DTC, strings.Join(parts, ", "))})
		}
	}
	// Misfire counts for the drive cycle: the end snapshot has the most.
	for _, s := range []*obd.Snapshot{info.Start, info.End} {
		if s == nil {
			continue
		}
		for _, t := range s.MonitorTests {
			if c := t.Cylinder(); c > 0 && t.TID == "0C" {
				if sum.Misfires == nil {
					sum.Misfires = map[string]float64{}
				}
				sum.Misfires[fmt.Sprint(c)] = t.Value
			}
		}
	}
	for c, n := range sum.Misfires {
		if n > 0 {
			sum.Events = append(sum.Events, Event{Kind: "misfire", Severity: "warn",
				Detail: fmt.Sprintf("cylinder %s: %.0f misfires counted this drive cycle", c, n)})
		}
	}
	seen := map[string]bool{}
	for _, s := range []*obd.Snapshot{info.Start, info.End} {
		if s == nil {
			continue
		}
		var keep []obd.MonitorTest
		for _, t := range s.MonitorTests {
			if !t.Pass {
				if k := t.MID + t.TID; !seen[k] {
					seen[k] = true
					sum.Events = append(sum.Events, Event{Kind: "monitor_test_failed", Severity: "warn",
						Detail: fmt.Sprintf("on-board test failed: %s %s %g outside %g–%g %s", t.Monitor, t.Test, t.Value, t.Min, t.Max, t.Unit)})
				}
			}
			if !t.Pass || t.Cylinder() > 0 && t.Value > 0 {
				keep = append(keep, t)
			}
		}
		s.MonitorTests = keep
	}
}

func sevRank(s string) int {
	switch s {
	case "high":
		return 2
	case "warn":
		return 1
	}
	return 0
}

// span estimates how long cond held, from the sample spacing of s.
func span(s *series, cond func(time.Time) bool) float64 {
	var total float64
	for i := 1; i < len(s.t); i++ {
		dt := s.t[i].Sub(s.t[i-1])
		if dt <= maxStale && cond(s.t[i-1]) {
			total += dt.Seconds()
		}
	}
	return total
}

func totalTrim(stft, ltft *series, cond func(time.Time) bool) float64 {
	if stft == nil {
		return 0
	}
	var vs []float64
	for i, t := range stft.t {
		if !cond(t) {
			continue
		}
		l, _ := ltft.at(t)
		vs = append(vs, stft.v[i]+l)
	}
	return stats(vs).Mean
}

// intervals finds stretches of at least minEventDuration where cond holds and
// reports each as an event. The detail format takes the threshold and the
// extreme value (min if useMin, else max) within the interval.
func intervals(s *series, start time.Time, cond func(time.Time, float64) bool, kind, sev, format string, threshold float64, useMin bool) []Event {
	if s == nil {
		return nil
	}
	var out []Event
	from := -1
	flush := func(to int) {
		if from < 0 {
			return
		}
		d := s.t[to].Sub(s.t[from])
		if d >= minEventDuration {
			ext := s.v[from]
			for _, v := range s.v[from : to+1] {
				if useMin {
					ext = math.Min(ext, v)
				} else {
					ext = math.Max(ext, v)
				}
			}
			out = append(out, Event{Kind: kind, Severity: sev, At: Duration(s.t[from].Sub(start)), Lasted: Duration(d),
				Detail: fmt.Sprintf(format, threshold, ext)})
		}
		from = -1
	}
	for i, t := range s.t {
		if cond(t, s.v[i]) {
			if from < 0 {
				from = i
			}
		} else if from >= 0 {
			flush(i - 1)
		}
	}
	if from >= 0 {
		flush(len(s.t) - 1)
	}
	return out
}

func round(v float64, places int) float64 {
	p := math.Pow(10, float64(places))
	return math.Round(v*p) / p
}
