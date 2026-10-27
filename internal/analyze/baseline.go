package analyze

import (
	"math"
	"sort"
)

// MinBaseline is the number of earlier drives needed before deviations are
// flagged.
const MinBaseline = 3

// MinTrendDays is the time span drives must cover before a per-30-day slope
// is reported.
const MinTrendDays = 3.0

// Delta compares one metric of a drive to the car's baseline.
type Delta struct {
	Metric string  `json:"metric"`
	Value  float64 `json:"value"`
	Mean   float64 `json:"baseline_mean"`
	Std    float64 `json:"baseline_std"`
	N      int     `json:"baseline_n"`
	Z      float64 `json:"z"`
	Flag   bool    `json:"flag"`
}

// minSpread keeps tiny baseline std devs (very consistent drives) from
// turning trivial differences into large z-scores.
var minSpread = map[string]float64{
	"warmup_to_75c_s":      30,
	"coolant_max_c":        2,
	"idle_rpm_mean":        15,
	"idle_rpm_std":         5,
	"idle_maf_gs":          0.2,
	"idle_timing_deg":      1,
	"stft_warm_mean":       1.5,
	"ltft_warm_mean":       1.5,
	"trim_total_idle":      2,
	"trim_total_cruise":    2,
	"o2_b1s1_std":          0.03,
	"voltage_running_mean": 0.1,
}

// Compare compares cur against baseline drives of the same car.
func Compare(cur Summary, baseline []Summary) []Delta {
	var out []Delta
	for _, m := range sortedKeys(cur.Metrics) {
		var vs []float64
		for _, b := range baseline {
			if v, ok := b.Metrics[m]; ok {
				vs = append(vs, v)
			}
		}
		if len(vs) == 0 {
			continue
		}
		s := stats(vs)
		d := Delta{Metric: m, Value: cur.Metrics[m], Mean: round(s.Mean, 2), Std: round(s.Std, 2), N: s.N}
		spread := math.Max(s.Std, minSpread[m])
		if spread > 0 {
			d.Z = round((d.Value-s.Mean)/spread, 1)
		}
		d.Flag = d.N >= MinBaseline && math.Abs(d.Z) >= 2
		out = append(out, d)
	}
	return out
}

// TrendLine is a metric's linear trend across drives.
type TrendLine struct {
	Metric        string  `json:"metric"`
	N             int     `json:"n"`
	First         float64 `json:"first"`
	Last          float64 `json:"last"`
	Mean          float64 `json:"mean"`
	SlopePerDrive float64 `json:"slope_per_drive"`
	// SlopePer30d is only set when the drives span at least MinTrendDays.
	SlopePer30d *float64  `json:"slope_per_30d,omitempty"`
	Values      []float64 `json:"values"` // chronological
}

// Trends fits a least-squares line through each metric over time. drives must
// be in chronological order.
func Trends(drives []Summary) []TrendLine {
	metrics := map[string]bool{}
	for _, d := range drives {
		for m := range d.Metrics {
			metrics[m] = true
		}
	}
	var out []TrendLine
	for _, m := range sortedKeys(metrics) {
		var xs, idx, ys []float64
		for _, d := range drives {
			if v, ok := d.Metrics[m]; ok {
				xs = append(xs, d.Start.Sub(drives[0].Start).Hours()/24)
				idx = append(idx, float64(len(idx)))
				ys = append(ys, v)
			}
		}
		if len(ys) < 2 {
			continue
		}
		tl := TrendLine{Metric: m, N: len(ys), First: ys[0], Last: ys[len(ys)-1], Mean: round(stats(ys).Mean, 2), Values: ys}
		tl.SlopePerDrive = round(slope(idx, ys), 3)
		if xs[len(xs)-1]-xs[0] >= MinTrendDays {
			v := round(slope(xs, ys)*30, 3)
			tl.SlopePer30d = &v
		}
		out = append(out, tl)
	}
	return out
}

func slope(xs, ys []float64) float64 {
	mx, my := stats(xs).Mean, stats(ys).Mean
	var num, den float64
	for i := range xs {
		num += (xs[i] - mx) * (ys[i] - my)
		den += (xs[i] - mx) * (xs[i] - mx)
	}
	if den == 0 {
		return 0
	}
	return num / den
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
