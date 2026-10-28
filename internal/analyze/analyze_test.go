package analyze

import (
	"bytes"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/jnszkr/carsight/internal/obd"
	"github.com/jnszkr/carsight/internal/store"
)

type drive struct {
	ltft    float64 // added to all warm trims
	voltage float64
	idleRPM float64
	coolMax float64
}

// synth builds a 20 minute drive at 2 Hz: cold start, 3 minutes idle,
// then alternating cruise and stops.
func synth(start time.Time, d drive) []store.Sample {
	var out []store.Sample
	for i := 0; i < 20*60*2; i++ {
		t := start.Add(time.Duration(i) * 500 * time.Millisecond)
		sec := float64(i) / 2
		cool := d.coolMax - (d.coolMax-10)*math.Exp(-sec/200)
		speed := 0.0
		if sec > 180 && math.Mod(sec, 120) < 90 {
			speed = 50
		}
		rpm := d.idleRPM + 10*math.Sin(sec)
		if speed > 0 {
			rpm = 2000
		}
		add := func(p obd.PID, v float64) { out = append(out, store.Sample{T: t, PID: p.Name, Value: v}) }
		add(obd.RPM, rpm)
		add(obd.Speed, speed)
		add(obd.CoolantTemp, cool)
		add(obd.STFTBank1, 1*math.Sin(sec/3))
		add(obd.LTFTBank1, d.ltft)
		add(obd.ModuleVoltage, d.voltage)
		add(obd.O2B1S1, 0.45+0.4*math.Sin(sec*3))
		add(obd.MAF, 3)
	}
	return out
}

var healthy = drive{ltft: 2, voltage: 14.1, idleRPM: 750, coolMax: 90}

func summarize(id int64, start time.Time, d drive, dtcs ...store.DTC) Summary {
	sess := store.Session{ID: id, VIN: "VIN1", StartedAt: start, EndedAt: start.Add(20 * time.Minute)}
	return Summarize(sess, store.Vehicle{Name: "Test car"}, synth(start, d), dtcs)
}

func TestSummarizeHealthy(t *testing.T) {
	s := summarize(1, time.Unix(1_700_000_000, 0), healthy)
	if len(s.Events) != 0 {
		t.Errorf("unexpected events: %+v", s.Events)
	}
	if !s.WarmUp.ColdStart || !s.WarmUp.Reached75C || s.WarmUp.ToWarm == nil {
		t.Errorf("warm-up: %+v", s.WarmUp)
	}
	if s.Idle == nil || math.Abs(s.Idle.RPM.Mean-750) > 5 {
		t.Errorf("idle: %+v", s.Idle)
	}
	if s.FuelTrim == nil || math.Abs(s.FuelTrim.TotalIdle-2) > 1 {
		t.Errorf("fuel trim: %+v", s.FuelTrim)
	}
	// 50 km/h for 90 of every 120 s over ~17 minutes is about 10 km.
	if s.DistanceKm < 8 || s.DistanceKm > 12 {
		t.Errorf("distance %v", s.DistanceKm)
	}
	if math.Abs(s.RPMRateHz-2) > 0.01 {
		t.Errorf("rate %v", s.RPMRateHz)
	}
}

func TestSummarizeFaults(t *testing.T) {
	s := summarize(1, time.Unix(1_700_000_000, 0), drive{ltft: 14, voltage: 12.4, idleRPM: 750, coolMax: 70},
		store.DTC{Code: "P0171", Kind: "stored"})
	kinds := map[string]bool{}
	for _, e := range s.Events {
		kinds[e.Kind] = true
	}
	for _, k := range []string{"slow_warmup", "low_voltage", "dtc"} {
		if !kinds[k] {
			t.Errorf("missing %s event: %+v", k, s.Events)
		}
	}
	// Never warm, so no warm trims or warm idle.
	if s.FuelTrim != nil || s.Idle != nil {
		t.Error("warm-only figures computed for a cold engine")
	}

	s = summarize(1, time.Unix(1_700_000_000, 0), drive{ltft: 14, voltage: 14, idleRPM: 750, coolMax: 90})
	found := false
	for _, e := range s.Events {
		if e.Kind == "fuel_trim" && strings.Contains(e.Detail, "lean") {
			found = true
		}
	}
	if !found {
		t.Errorf("lean trim not flagged: %+v", s.Events)
	}
}

// A warm diesel standing still with a few free revs, polled slowly and with
// no throttle PID, as logged from a Citroën C4 over K-line.
func TestSummarizeIdleIgnoresRevDown(t *testing.T) {
	start := time.Unix(1_700_000_000, 0)
	rpms := []float64{744, 742, 744, 746, 743, 744, 744, 746, 744, 745, 1170, 2533, 1184, 738, 744, 742, 743, 744,
		742, 740, 1904, 2649, 2805, 1010, 735, 743, 743, 744, 745, 743, 1951, 3165, 842, 742, 744, 743, 743, 745, 744}
	var samples []store.Sample
	for i, r := range rpms {
		t := start.Add(time.Duration(i) * 1500 * time.Millisecond)
		samples = append(samples,
			store.Sample{T: t, PID: obd.RPM.Name, Value: r},
			store.Sample{T: t, PID: obd.Speed.Name, Value: 0},
			store.Sample{T: t, PID: obd.CoolantTemp.Name, Value: 80})
	}
	sess := store.Session{ID: 1, StartedAt: start, EndedAt: start.Add(time.Minute)}
	s := Summarize(sess, store.Vehicle{}, samples, nil)
	if s.Idle == nil || s.Idle.RPM.Max > 750 || s.Idle.RPM.Std > 5 {
		t.Fatalf("idle includes revving: %+v", s.Idle)
	}
	for _, e := range s.Events {
		if e.Kind == "unstable_idle" {
			t.Errorf("unexpected %+v", e)
		}
	}
}

// DTCs are read at the start and the end of a drive; the same code read twice
// is one code.
func TestSummarizeDedupesDTCs(t *testing.T) {
	s := summarize(1, time.Unix(1_700_000_000, 0), healthy,
		store.DTC{Code: "P1351", Kind: "stored"}, store.DTC{Code: "P1351", Kind: "stored"},
		store.DTC{Code: "P1351", Kind: "pending"}, store.DTC{Code: "P1351", Kind: "pending"})
	if len(s.DTCs) != 2 {
		t.Errorf("dtcs %+v", s.DTCs)
	}
	n := 0
	for _, e := range s.Events {
		if e.Kind == "dtc" {
			n++
		}
	}
	if n != 2 {
		t.Errorf("%d dtc events: %+v", n, s.Events)
	}
}

func TestCompareAndTrends(t *testing.T) {
	t0 := time.Unix(1_700_000_000, 0)
	var base []Summary
	for i := range 4 {
		d := healthy
		d.idleRPM += float64(i) * 3
		base = append(base, summarize(int64(i+1), t0.Add(time.Duration(i)*24*time.Hour), d))
	}
	bad := healthy
	bad.ltft = 12
	cur := summarize(5, t0.Add(5*24*time.Hour), bad)

	deltas := Compare(cur, base)
	flagged := map[string]bool{}
	for _, d := range deltas {
		if d.Flag {
			flagged[d.Metric] = true
		}
	}
	if !flagged["ltft_warm_mean"] || !flagged["trim_total_idle"] {
		t.Errorf("trim deviation not flagged: %+v", deltas)
	}
	if flagged["idle_rpm_mean"] || flagged["voltage_running_mean"] {
		t.Errorf("unexpected flags: %v", flagged)
	}

	trends := Trends(append(base, cur))
	for _, tl := range trends {
		if tl.Metric == "ltft_warm_mean" && (tl.SlopePerDrive <= 0 || tl.SlopePer30d == nil || *tl.SlopePer30d <= 0) {
			t.Errorf("ltft slope %+v", tl)
		}
	}

	var buf bytes.Buffer
	if err := WriteReport(&buf, cur, deltas); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"# Drive 5: Test car", "## Findings", "## Compared with baseline", "⚠"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("report missing %q:\n%s", want, buf.String())
		}
	}
	buf.Reset()
	if err := WriteTrendReport(&buf, "Test car", append(base, cur), trends); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "ltft_warm_mean") {
		t.Errorf("trend report:\n%s", buf.String())
	}
}
