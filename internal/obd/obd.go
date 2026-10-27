package obd

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/jnszkr/carsight/internal/elm327"
)

// Requester is the subset of elm327.Conn used here.
type Requester interface {
	Request(ctx context.Context, req string) ([][]byte, error)
}

// Client issues OBD requests through an adapter.
type Client struct {
	R   Requester
	CAN bool // the vehicle protocol is CAN; enables single-response shortcuts
}

// Read reads a single mode 01 PID. When several ECUs answer, the first
// valid response wins.
func (c *Client) Read(ctx context.Context, p PID) (float64, error) {
	req := fmt.Sprintf("01%02X", p.Code)
	if c.CAN {
		// Telling the adapter to expect one response lets it return as soon
		// as the engine ECU answers instead of waiting out the timeout.
		req += "1"
	}
	resps, err := c.R.Request(ctx, req)
	if err != nil {
		return 0, err
	}
	var lastErr error
	for _, r := range resps {
		v, err := DecodeMode01(p, r)
		if err == nil {
			return v, nil
		}
		lastErr = err
	}
	return 0, lastErr
}

// SupportedPIDs queries the 0x00/0x20/0x40/... bitmaps and returns the set
// of supported mode 01 PIDs.
func (c *Client) SupportedPIDs(ctx context.Context) (map[byte]bool, error) {
	supported := map[byte]bool{}
	for base := 0; base < 0xE0; base += 0x20 {
		resps, err := c.R.Request(ctx, fmt.Sprintf("01%02X", base))
		if errors.Is(err, elm327.ErrNoData) {
			break
		}
		if err != nil {
			return supported, err
		}
		more := false
		for _, r := range resps {
			if len(r) < 6 || r[0] != 0x41 || int(r[1]) != base {
				continue
			}
			for i := range 32 {
				if r[2+i/8]&(0x80>>(i%8)) != 0 {
					supported[byte(base+i+1)] = true
				}
			}
			if r[5]&1 != 0 {
				more = true
			}
		}
		if !more {
			break
		}
	}
	if len(supported) == 0 {
		return nil, errors.New("obd: no supported PIDs reported")
	}
	return supported, nil
}

// Filter returns the PIDs from ps that are in supported.
func Filter(ps []PID, supported map[byte]bool) []PID {
	var out []PID
	for _, p := range ps {
		if supported[p.Code] {
			out = append(out, p)
		}
	}
	return out
}

// DTCs reads stored (mode 03) or pending (mode 07) trouble codes.
func (c *Client) DTCs(ctx context.Context, pending bool) ([]string, error) {
	mode := "03"
	if pending {
		mode = "07"
	}
	resps, err := c.R.Request(ctx, mode)
	if errors.Is(err, elm327.ErrNoData) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return ParseDTCs(resps, c.CAN), nil
}

// ParseDTCs decodes mode 03/07/0A response payloads. On CAN, the byte
// after the service ID is the DTC count; on older protocols each response
// carries up to three codes padded with zeros.
func ParseDTCs(resps [][]byte, can bool) []string {
	seen := map[string]bool{}
	var out []string
	for _, r := range resps {
		if len(r) < 1 {
			continue
		}
		data := r[1:]
		if can && len(data) > 0 {
			n := int(data[0])
			data = data[1:]
			if 2*n < len(data) {
				data = data[:2*n]
			}
		}
		for i := 0; i+1 < len(data); i += 2 {
			if data[i] == 0 && data[i+1] == 0 {
				continue
			}
			code := FormatDTC(data[i], data[i+1])
			if !seen[code] {
				seen[code] = true
				out = append(out, code)
			}
		}
	}
	sort.Strings(out)
	return out
}

// FormatDTC renders two DTC bytes as e.g. "P0133".
func FormatDTC(a, b byte) string {
	sys := "PCBU"[a>>6]
	return fmt.Sprintf("%c%d%X%02X", sys, (a>>4)&0x3, a&0xF, b)
}

// ClearDTCs sends mode 04, which clears stored codes and freeze frames and
// resets the readiness monitors. Only call this on explicit user request.
func (c *Client) ClearDTCs(ctx context.Context) error {
	resps, err := c.R.Request(ctx, "04")
	if err != nil {
		return err
	}
	for _, r := range resps {
		if len(r) > 0 && r[0] == 0x44 {
			return nil
		}
	}
	return fmt.Errorf("obd: clear DTCs not acknowledged: % X", resps)
}

// VIN reads the vehicle identification number (mode 09 PID 02).
func (c *Client) VIN(ctx context.Context) (string, error) {
	resps, err := c.R.Request(ctx, "0902")
	if err != nil {
		return "", err
	}
	return ParseVIN(resps, c.CAN)
}

// ParseVIN decodes mode 09 PID 02 payloads. CAN returns one reassembled
// payload "49 02 01 <17 chars>"; older protocols return five messages
// "49 02 <seq> <4 bytes>" which are concatenated in sequence order.
func ParseVIN(resps [][]byte, can bool) (string, error) {
	var b []byte
	if can {
		for _, r := range resps {
			if len(r) >= 3 && r[0] == 0x49 && r[1] == 0x02 {
				b = r[3:]
				break
			}
		}
	} else {
		parts := map[byte][]byte{}
		for _, r := range resps {
			if len(r) >= 3 && r[0] == 0x49 && r[1] == 0x02 {
				parts[r[2]] = r[3:]
			}
		}
		for seq := byte(1); seq <= 5; seq++ {
			b = append(b, parts[seq]...)
		}
	}
	vin := strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' || r >= 'A' && r <= 'Z' {
			return r
		}
		return -1
	}, string(b))
	if len(vin) > 17 {
		vin = vin[len(vin)-17:]
	}
	if len(vin) != 17 {
		return "", fmt.Errorf("obd: malformed VIN %q", vin)
	}
	return vin, nil
}

// Readiness is the decoded monitor status (mode 01 PID 01).
type Readiness struct {
	MIL      bool              `json:"mil"`
	DTCCount int               `json:"dtc_count"`
	Monitors map[string]string `json:"monitors"` // name -> "complete" | "incomplete"
}

// ReadReadiness reads and decodes mode 01 PID 01.
func (c *Client) ReadReadiness(ctx context.Context) (Readiness, error) {
	resps, err := c.R.Request(ctx, "0101")
	if err != nil {
		return Readiness{}, err
	}
	for _, r := range resps {
		if len(r) >= 6 && r[0] == 0x41 && r[1] == 0x01 {
			return DecodeReadiness(r[2:6]), nil
		}
	}
	return Readiness{}, fmt.Errorf("obd: unexpected readiness response % X", resps)
}

// DecodeReadiness decodes the four data bytes A..D of PID 01.
func DecodeReadiness(d []byte) Readiness {
	a, b, c, dd := d[0], d[1], d[2], d[3]
	r := Readiness{MIL: a&0x80 != 0, DTCCount: int(a & 0x7F), Monitors: map[string]string{}}
	set := func(name string, supported, incomplete bool) {
		if !supported {
			return
		}
		if incomplete {
			r.Monitors[name] = "incomplete"
		} else {
			r.Monitors[name] = "complete"
		}
	}
	set("misfire", b&0x01 != 0, b&0x10 != 0)
	set("fuel_system", b&0x02 != 0, b&0x20 != 0)
	set("components", b&0x04 != 0, b&0x40 != 0)
	if b&0x08 == 0 { // spark ignition
		names := []string{"catalyst", "heated_catalyst", "evap_system", "secondary_air", "ac_refrigerant", "o2_sensor", "o2_sensor_heater", "egr_system"}
		for i, n := range names {
			set(n, c&(1<<i) != 0, dd&(1<<i) != 0)
		}
	} else {
		names := []string{"nmhc_catalyst", "nox_scr", "", "boost_pressure", "", "exhaust_gas_sensor", "pm_filter", "egr_vvt_system"}
		for i, n := range names {
			if n != "" {
				set(n, c&(1<<i) != 0, dd&(1<<i) != 0)
			}
		}
	}
	return r
}
