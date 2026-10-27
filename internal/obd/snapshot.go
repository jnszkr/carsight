package obd

import (
	"context"
	"fmt"
	"math"
	"time"
)

// Snapshot is everything the car reports at one moment besides the live
// samples: every supported mode 01 value (including the ones not polled
// during a drive), monitor status, freeze frame, permanent codes, vehicle
// information and monitor test results.
type Snapshot struct {
	At     time.Time          `json:"at"`
	Values map[string]float64 `json:"values"`
	// Labels names enumerated values, e.g. fuel_type: "gasoline".
	Labels map[string]string `json:"labels,omitempty"`
	// Undecoded holds supported mode 01 PIDs carsight can't decode, as
	// raw hex data bytes keyed by PID.
	Undecoded     map[string]string `json:"undecoded_pids,omitempty"`
	Readiness     *Readiness        `json:"readiness,omitempty"`
	ThisCycle     *Readiness        `json:"monitors_this_drive_cycle,omitempty"`
	FreezeFrame   *FreezeFrame      `json:"freeze_frame,omitempty"`
	PermanentDTCs []string          `json:"permanent_dtcs"`
	VehicleInfo   *VehicleInfo      `json:"vehicle_info,omitempty"`
	MonitorTests  []MonitorTest     `json:"monitor_tests,omitempty"`
	// Errors lists the parts that couldn't be read.
	Errors []string `json:"errors,omitempty"`
}

// SessionInfo is what a session stores besides samples and DTCs.
type SessionInfo struct {
	Start *Snapshot `json:"start,omitempty"`
	End   *Snapshot `json:"end,omitempty"`
}

// TakeSnapshot reads a snapshot. supported is the car's mode 01 support
// set. vehicle also reads mode 09 vehicle information, which doesn't
// change during a drive.
func (c *Client) TakeSnapshot(ctx context.Context, supported map[byte]bool, vehicle bool) *Snapshot {
	j := c.NewSnapshot(supported, vehicle)
	for j.Step(ctx) {
	}
	return j.Snapshot
}

// SnapshotJob reads a snapshot one request at a time, so a drive can take
// it between polling cycles instead of pausing logging for seconds.
type SnapshotJob struct {
	Snapshot *Snapshot
	c        *Client
	queue    []func(context.Context)
}

// NewSnapshot prepares a snapshot; Step reads it.
func (c *Client) NewSnapshot(supported map[byte]bool, vehicle bool) *SnapshotJob {
	s := &Snapshot{At: time.Now(), Values: map[string]float64{}, Labels: map[string]string{}, PermanentDTCs: []string{}}
	j := &SnapshotJob{Snapshot: s, c: c}
	fail := func(what string, err error) { s.Errors = append(s.Errors, fmt.Sprintf("%s: %v", what, err)) }
	add := func(f func(context.Context)) { j.queue = append(j.queue, f) }

	add(func(ctx context.Context) {
		if r, err := c.ReadReadiness(ctx); err != nil {
			fail("readiness", err)
		} else {
			s.Readiness = &r
		}
	})
	add(func(ctx context.Context) {
		if codes, err := c.PermanentDTCs(ctx); err != nil {
			fail("permanent DTCs", err)
		} else if codes != nil {
			s.PermanentDTCs = codes
		}
	})
	if supported[0x41] {
		add(func(ctx context.Context) {
			if r, err := c.readMonitorStatus(ctx, 0x41); err == nil {
				r.DTCCount, r.MIL = 0, false // byte A is reserved in PID 41
				s.ThisCycle = &r
			}
		})
	}
	known := map[byte]bool{0x01: true, 0x02: true, 0x41: true}
	for _, g := range Groups(All) {
		known[g.Code] = true
		if !supported[g.Code] {
			continue
		}
		add(func(ctx context.Context) {
			vs, err := c.ReadGroup(ctx, g)
			if err != nil {
				return
			}
			for i, p := range g.PIDs {
				if !math.IsNaN(vs[i]) {
					s.Values[p.Name] = round3(vs[i])
					if l := p.Label(vs[i]); l != "" {
						s.Labels[p.Name] = l
					}
				}
			}
		})
	}
	for code := 1; code < 0xE0; code++ {
		if !supported[byte(code)] || known[byte(code)] || code%0x20 == 0 {
			continue
		}
		add(func(ctx context.Context) {
			resps, err := c.R.Request(ctx, fmt.Sprintf("01%02X", code))
			if err != nil {
				return
			}
			for _, r := range resps {
				if len(r) > 2 && r[0] == 0x41 && int(r[1]) == code {
					if s.Undecoded == nil {
						s.Undecoded = map[string]string{}
					}
					s.Undecoded[fmt.Sprintf("%02X", code)] = fmt.Sprintf("%X", r[2:])
					return
				}
			}
		})
	}
	add(func(ctx context.Context) {
		ff, err := c.ReadFreezeFrame(ctx)
		if err != nil {
			fail("freeze frame", err)
		}
		s.FreezeFrame = ff
	})
	if vehicle {
		add(func(ctx context.Context) {
			vi, err := c.ReadVehicleInfo(ctx)
			if err != nil {
				fail("vehicle info", err)
			}
			s.VehicleInfo = vi
		})
	}
	if c.CAN {
		// Mode 06: find the supported monitors, then read one per step.
		add(func(ctx context.Context) {
			sup, err := c.supported(ctx, "06%02X", 0x46, 0)
			if err != nil {
				fail("monitor tests", err)
				return
			}
			for mid := 1; mid < 0xE0; mid++ {
				if !sup[byte(mid)] || mid%0x20 == 0 {
					continue
				}
				add(func(ctx context.Context) {
					ts, err := c.readMonitor(ctx, byte(mid))
					if err == nil {
						s.MonitorTests = append(s.MonitorTests, ts...)
					}
				})
			}
		})
	}
	return j
}

// Step runs the next request (or small batch) and reports whether any
// remain.
func (j *SnapshotJob) Step(ctx context.Context) bool {
	if len(j.queue) == 0 {
		return false
	}
	f := j.queue[0]
	j.queue = j.queue[1:]
	f(ctx)
	return len(j.queue) > 0
}

// Done reports whether the snapshot is complete.
func (j *SnapshotJob) Done() bool { return len(j.queue) == 0 }
