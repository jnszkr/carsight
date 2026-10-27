package obd

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/jnszkr/carsight/internal/elm327"
)

// FreezeFrame is the snapshot the ECU stored when it set a trouble code
// (mode 02, frame 0).
type FreezeFrame struct {
	DTC    string             `json:"dtc"`
	Values map[string]float64 `json:"values"`
	Labels map[string]string  `json:"labels,omitempty"`
}

// ReadFreezeFrame reads freeze frame 0. It returns nil when no frame is
// stored.
func (c *Client) ReadFreezeFrame(ctx context.Context) (*FreezeFrame, error) {
	resps, err := c.R.Request(ctx, "020200")
	if errors.Is(err, elm327.ErrNoData) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var dtc string
	for _, r := range resps {
		if len(r) >= 5 && r[0] == 0x42 && r[1] == 0x02 {
			if r[3] != 0 || r[4] != 0 {
				dtc = FormatDTC(r[3], r[4])
			}
			break
		}
	}
	if dtc == "" {
		return nil, nil
	}
	ff := &FreezeFrame{DTC: dtc, Values: map[string]float64{}, Labels: map[string]string{}}
	sup, err := c.supported(ctx, "02%02X00", 0x42, 1)
	if err != nil {
		return ff, nil // the DTC alone is still worth having
	}
	for _, g := range Groups(All) {
		if !sup[g.Code] {
			continue
		}
		resps, err := c.R.Request(ctx, fmt.Sprintf("02%02X00", g.Code))
		if err != nil {
			continue
		}
		for _, r := range resps {
			if len(r) < 3+g.Bytes || r[0] != 0x42 || r[1] != g.Code {
				continue
			}
			for _, p := range g.PIDs {
				if v := p.Decode(r[3 : 3+p.Bytes]); !math.IsNaN(v) {
					ff.Values[p.Name] = round3(v)
					if l := p.Label(v); l != "" {
						ff.Labels[p.Name] = l
					}
				}
			}
			break
		}
	}
	return ff, nil
}

// VehicleInfo is what mode 09 reports besides the VIN.
type VehicleInfo struct {
	// CalibrationIDs identify the software in each ECU; CVNs are their
	// checksums, so a change reveals a reflash or tune.
	CalibrationIDs []string `json:"calibration_ids,omitempty"`
	CVNs           []string `json:"cvns,omitempty"`
	ECUNames       []string `json:"ecu_names,omitempty"`
	// InUsePerformance holds the monitor completion counters (IPT): how
	// often each monitor ran against how often conditions allowed it.
	InUsePerformance map[string]int `json:"in_use_performance,omitempty"`
}

// iptNames name the counters of mode 09 PID 08 (spark ignition), in order.
var iptNames = []string{
	"obd_monitoring_conditions", "ignition_cycles",
	"catalyst_b1_completed", "catalyst_b1_conditions", "catalyst_b2_completed", "catalyst_b2_conditions",
	"o2_b1_completed", "o2_b1_conditions", "o2_b2_completed", "o2_b2_conditions",
	"egr_vvt_completed", "egr_vvt_conditions", "secondary_air_completed", "secondary_air_conditions",
	"evap_completed", "evap_conditions", "secondary_o2_b1_completed", "secondary_o2_b1_conditions",
	"secondary_o2_b2_completed", "secondary_o2_b2_conditions",
}

// ReadVehicleInfo reads calibration IDs, CVNs, ECU names and in-use
// performance counters, whichever the car supports.
func (c *Client) ReadVehicleInfo(ctx context.Context) (*VehicleInfo, error) {
	sup, err := c.supported(ctx, "09%02X", 0x49, 0)
	if err != nil {
		return nil, err
	}
	vi := &VehicleInfo{}
	read := func(pid byte) [][]byte {
		if !sup[pid] {
			return nil
		}
		resps, err := c.R.Request(ctx, fmt.Sprintf("09%02X", pid))
		if err != nil {
			return nil
		}
		return mode09Data(resps, pid, c.CAN)
	}
	for _, d := range read(0x04) {
		for i := 0; i+16 <= len(d); i += 16 {
			if s := ascii(d[i : i+16]); s != "" {
				vi.CalibrationIDs = append(vi.CalibrationIDs, s)
			}
		}
	}
	for _, d := range read(0x06) {
		for i := 0; i+4 <= len(d); i += 4 {
			vi.CVNs = append(vi.CVNs, fmt.Sprintf("%02X%02X%02X%02X", d[i], d[i+1], d[i+2], d[i+3]))
		}
	}
	for _, d := range read(0x0A) {
		if s := ascii(d); s != "" {
			vi.ECUNames = append(vi.ECUNames, s)
		}
	}
	if ds := read(0x08); len(ds) > 0 {
		vi.InUsePerformance = map[string]int{}
		d := ds[0]
		for i := 0; i+1 < len(d) && i/2 < len(iptNames); i += 2 {
			vi.InUsePerformance[iptNames[i/2]] = u16(d[i:])
		}
	}
	return vi, nil
}

// mode09Data returns the data of each ECU's answer to mode 09 pid. On CAN
// each response is one ECU's reassembled message "49 pid count data...";
// older protocols split one answer into messages "49 pid seq data..." that
// are joined in sequence order.
func mode09Data(resps [][]byte, pid byte, can bool) [][]byte {
	var out [][]byte
	if can {
		for _, r := range resps {
			if len(r) >= 3 && r[0] == 0x49 && r[1] == pid {
				out = append(out, r[3:])
			}
		}
		return out
	}
	parts := map[byte][]byte{}
	last := byte(0)
	for _, r := range resps {
		if len(r) >= 3 && r[0] == 0x49 && r[1] == pid {
			parts[r[2]] = r[3:]
			last = max(last, r[2])
		}
	}
	var d []byte
	for seq := byte(1); seq <= last; seq++ {
		d = append(d, parts[seq]...)
	}
	if d != nil {
		out = append(out, d)
	}
	return out
}

func ascii(b []byte) string {
	return strings.TrimSpace(strings.Map(func(r rune) rune {
		if r >= 0x20 && r < 0x7F {
			return r
		}
		return -1
	}, string(b)))
}

// MonitorTest is one on-board monitor test result (mode 06): what the ECU
// measured the last time the monitor ran, and the limits it must stay in.
type MonitorTest struct {
	MID     string  `json:"mid"` // monitor ID, hex
	TID     string  `json:"tid"` // test ID, hex
	Monitor string  `json:"monitor"`
	Test    string  `json:"test,omitempty"`
	Value   float64 `json:"value"`
	Min     float64 `json:"min"`
	Max     float64 `json:"max"`
	// Unit is empty when the scaling ID isn't one carsight knows; then the
	// numbers are raw and only Pass is meaningful.
	Unit    string `json:"unit,omitempty"`
	ScaleID string `json:"scale_id"`
	Pass    bool   `json:"pass"`
}

// Cylinder returns the cylinder number of a per-cylinder misfire monitor,
// or 0.
func (t MonitorTest) Cylinder() int {
	var mid int
	fmt.Sscanf(t.MID, "%X", &mid)
	if mid >= 0xA2 && mid <= 0xAD {
		return mid - 0xA1
	}
	return 0
}

type scaling struct {
	unit   string
	factor float64
	offset float64
	signed bool
}

// scalings are the unit and scaling IDs (SAE J1979 appendix E) carsight
// converts; others are reported raw.
var scalings = map[byte]scaling{
	0x01: {"", 1, 0, false},
	0x0A: {"mV", 0.122, 0, false},
	0x0B: {"V", 0.001, 0, false},
	0x10: {"ms", 1, 0, false},
	0x14: {"Ω", 1, 0, false},
	0x16: {"°C", 0.1, -40, false},
	0x24: {"count", 1, 0, false},
	0x81: {"", 1, 0, true},
	0x8A: {"mV", 0.122, 0, true},
}

// monitorName names a monitor ID.
func monitorName(mid byte) string {
	o2 := func(base byte) string {
		i := mid - base
		return fmt.Sprintf("bank %d sensor %d", i/4+1, i%4+1)
	}
	switch {
	case mid >= 0x01 && mid <= 0x10:
		return "O2 sensor " + o2(0x01)
	case mid >= 0x21 && mid <= 0x24:
		return fmt.Sprintf("catalyst bank %d", mid-0x20)
	case mid >= 0x31 && mid <= 0x34:
		return fmt.Sprintf("EGR bank %d", mid-0x30)
	case mid >= 0x35 && mid <= 0x38:
		return fmt.Sprintf("VVT bank %d", mid-0x34)
	case mid == 0x39:
		return "evap (cap off)"
	case mid == 0x3A:
		return "evap (0.090\" leak)"
	case mid == 0x3B:
		return "evap (0.040\" leak)"
	case mid == 0x3C:
		return "evap (0.020\" leak)"
	case mid == 0x3D:
		return "purge flow"
	case mid >= 0x41 && mid <= 0x50:
		return "O2 sensor heater " + o2(0x41)
	case mid >= 0x61 && mid <= 0x64:
		return fmt.Sprintf("heated catalyst bank %d", mid-0x60)
	case mid >= 0x71 && mid <= 0x74:
		return fmt.Sprintf("secondary air %d", mid-0x70)
	case mid >= 0x81 && mid <= 0x84:
		return fmt.Sprintf("fuel system bank %d", mid-0x80)
	case mid >= 0x85 && mid <= 0x88:
		return fmt.Sprintf("boost pressure bank %d", mid-0x84)
	case mid == 0x90 || mid == 0x91:
		return fmt.Sprintf("NOx adsorber bank %d", mid-0x8F)
	case mid == 0x98 || mid == 0x99:
		return fmt.Sprintf("NOx catalyst bank %d", mid-0x97)
	case mid == 0xA1:
		return "misfire, general"
	case mid >= 0xA2 && mid <= 0xAD:
		return fmt.Sprintf("misfire cylinder %d", mid-0xA1)
	case mid == 0xB0 || mid == 0xB1:
		return fmt.Sprintf("PM filter bank %d", mid-0xAF)
	}
	return fmt.Sprintf("monitor %02X", mid)
}

func testName(mid, tid byte) string {
	if mid >= 0xA1 && mid <= 0xAD {
		switch tid {
		case 0x0B:
			return "misfires, average of the last 10 drive cycles"
		case 0x0C:
			return "misfires, last or current drive cycle"
		}
	}
	if tid >= 0x01 && tid <= 0x0A && (mid <= 0x10) {
		return [...]string{"", "rich to lean threshold voltage", "lean to rich threshold voltage",
			"low voltage for switch time", "high voltage for switch time", "rich to lean switch time",
			"lean to rich switch time", "minimum voltage", "maximum voltage", "time between transitions",
			"sensor period"}[tid]
	}
	return ""
}

// ReadMonitorTests reads all mode 06 test results. Only the CAN format is
// supported; older protocols return nil.
func (c *Client) ReadMonitorTests(ctx context.Context) ([]MonitorTest, error) {
	if !c.CAN {
		return nil, nil
	}
	sup, err := c.supported(ctx, "06%02X", 0x46, 0)
	if err != nil {
		return nil, err
	}
	var out []MonitorTest
	for mid := 1; mid < 0xE0; mid++ {
		if !sup[byte(mid)] || mid%0x20 == 0 {
			continue
		}
		if ts, err := c.readMonitor(ctx, byte(mid)); err == nil {
			out = append(out, ts...)
		}
	}
	return out, nil
}

func (c *Client) readMonitor(ctx context.Context, mid byte) ([]MonitorTest, error) {
	resps, err := c.R.Request(ctx, fmt.Sprintf("06%02X", mid))
	if err != nil {
		return nil, err
	}
	var out []MonitorTest
	for _, r := range resps {
		out = append(out, ParseMonitorTests(r)...)
	}
	return out, nil
}

// ParseMonitorTests decodes a CAN mode 06 payload: 46 followed by 9-byte
// records MID TID UASID value(2) min(2) max(2).
func ParseMonitorTests(r []byte) []MonitorTest {
	if len(r) < 1 || r[0] != 0x46 {
		return nil
	}
	var out []MonitorTest
	for i := 1; i+9 <= len(r); i += 9 {
		mid, tid, uas := r[i], r[i+1], r[i+2]
		raw := func(j int) int { return int(r[j])<<8 | int(r[j+1]) }
		v, lo, hi := raw(i+3), raw(i+5), raw(i+7)
		sc, known := scalings[uas]
		conv := func(n int) float64 {
			if sc.signed {
				n = int(int16(n))
			}
			if !known {
				return float64(n)
			}
			return round3(float64(n)*sc.factor + sc.offset)
		}
		pass := v >= lo && v <= hi
		if sc.signed {
			pass = int16(v) >= int16(lo) && int16(v) <= int16(hi)
		}
		out = append(out, MonitorTest{
			MID: fmt.Sprintf("%02X", mid), TID: fmt.Sprintf("%02X", tid), Monitor: monitorName(mid), Test: testName(mid, tid),
			Value: conv(v), Min: conv(lo), Max: conv(hi), Unit: sc.unit, ScaleID: fmt.Sprintf("%02X", uas), Pass: pass,
		})
	}
	return out
}

func round3(v float64) float64 { return math.Round(v*1000) / 1000 }
