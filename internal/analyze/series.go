package analyze

import (
	"math"
	"sort"
	"time"

	"github.com/jnszkr/carsight/internal/store"
)

// series is one PID's samples in time order.
type series struct {
	t []time.Time
	v []float64
}

func group(samples []store.Sample) map[string]*series {
	out := map[string]*series{}
	for _, s := range samples {
		ser, ok := out[s.PID]
		if !ok {
			ser = &series{}
			out[s.PID] = ser
		}
		ser.t = append(ser.t, s.T)
		ser.v = append(ser.v, s.Value)
	}
	for _, ser := range out {
		if !sort.SliceIsSorted(ser.t, func(i, j int) bool { return ser.t[i].Before(ser.t[j]) }) {
			sort.Sort(ser)
		}
	}
	return out
}

func (s *series) Len() int           { return len(s.t) }
func (s *series) Less(i, j int) bool { return s.t[i].Before(s.t[j]) }
func (s *series) Swap(i, j int) {
	s.t[i], s.t[j] = s.t[j], s.t[i]
	s.v[i], s.v[j] = s.v[j], s.v[i]
}

// maxStale is how old a sample may be and still count as "the current value"
// when correlating PIDs that were polled at different times.
const maxStale = 5 * time.Second

// at returns the most recent value at or before t.
func (s *series) at(t time.Time) (float64, bool) {
	if s == nil {
		return 0, false
	}
	i := sort.Search(len(s.t), func(i int) bool { return s.t[i].After(t) }) - 1
	if i < 0 || t.Sub(s.t[i]) > maxStale {
		return 0, false
	}
	return s.v[i], true
}

// anyWithin reports whether any sample within d of t satisfies cond.
func (s *series) anyWithin(t time.Time, d time.Duration, cond func(v float64) bool) bool {
	if s == nil {
		return false
	}
	for i := sort.Search(len(s.t), func(i int) bool { return !s.t[i].Before(t.Add(-d)) }); i < len(s.t) && !s.t[i].After(t.Add(d)); i++ {
		if cond(s.v[i]) {
			return true
		}
	}
	return false
}

// Stats are simple descriptive statistics.
type Stats struct {
	N    int     `json:"n"`
	Min  float64 `json:"min"`
	Max  float64 `json:"max"`
	Mean float64 `json:"mean"`
	Std  float64 `json:"std"`
}

func stats(vs []float64) Stats {
	if len(vs) == 0 {
		return Stats{}
	}
	s := Stats{N: len(vs), Min: math.Inf(1), Max: math.Inf(-1)}
	var sum float64
	for _, v := range vs {
		sum += v
		s.Min = math.Min(s.Min, v)
		s.Max = math.Max(s.Max, v)
	}
	s.Mean = sum / float64(len(vs))
	var sq float64
	for _, v := range vs {
		sq += (v - s.Mean) * (v - s.Mean)
	}
	if len(vs) > 1 {
		s.Std = math.Sqrt(sq / float64(len(vs)-1))
	}
	return s
}

// where collects values of s at times where cond holds.
func (s *series) where(cond func(t time.Time) bool) []float64 {
	if s == nil {
		return nil
	}
	var out []float64
	for i, t := range s.t {
		if cond(t) {
			out = append(out, s.v[i])
		}
	}
	return out
}
