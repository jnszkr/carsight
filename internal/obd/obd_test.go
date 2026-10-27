package obd

import (
	"context"
	"math"
	"reflect"
	"testing"

	"github.com/jnszkr/carsight/internal/elm327"
)

type fakeReq map[string][][]byte

func (f fakeReq) Request(_ context.Context, req string) ([][]byte, error) {
	r, ok := f[req]
	if !ok {
		return nil, elm327.ErrNoData
	}
	return r, nil
}

func TestDecodeMode01(t *testing.T) {
	tests := []struct {
		p       PID
		payload []byte
		want    float64
	}{
		{RPM, []byte{0x41, 0x0C, 0x1A, 0xF8}, 1726},
		{Speed, []byte{0x41, 0x0D, 0x32}, 50},
		{CoolantTemp, []byte{0x41, 0x05, 0x7B}, 83},
		{STFTBank1, []byte{0x41, 0x06, 0x80}, 0},
		{LTFTBank1, []byte{0x41, 0x07, 0x8A}, 7.8125},
		{TimingAdvance, []byte{0x41, 0x0E, 0x94}, 10},
		{MAF, []byte{0x41, 0x10, 0x01, 0xF4}, 5},
		{ModuleVoltage, []byte{0x41, 0x42, 0x37, 0x1E}, 14.11},
		{O2B1S1, []byte{0x41, 0x14, 0x5A, 0xFF}, 0.45},
	}
	for _, tt := range tests {
		got, err := DecodeMode01(tt.p, tt.payload)
		if err != nil {
			t.Errorf("%s: %v", tt.p.Name, err)
			continue
		}
		if math.Abs(got-tt.want) > 1e-9 {
			t.Errorf("%s = %v, want %v", tt.p.Name, got, tt.want)
		}
	}
	if _, err := DecodeMode01(RPM, []byte{0x41, 0x0D, 0x32}); err == nil {
		t.Error("expected error on PID mismatch")
	}
}

func TestSupportedPIDs(t *testing.T) {
	c := &Client{R: fakeReq{
		"0100": {{0x41, 0x00, 0xBE, 0x3F, 0xA8, 0x13}},
		"0120": {{0x41, 0x20, 0x80, 0x00, 0x00, 0x01}},
		"0140": {{0x41, 0x40, 0x40, 0x00, 0x00, 0x00}},
	}}
	got, err := c.SupportedPIDs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, code := range []byte{0x01, 0x04, 0x05, 0x0C, 0x0D, 0x10, 0x11, 0x13, 0x15, 0x20, 0x21, 0x40, 0x42} {
		if !got[code] {
			t.Errorf("PID %02X not reported supported", code)
		}
	}
	if got[0x02] || got[0x14] || got[0x41] {
		t.Error("unsupported PID reported")
	}
}

func TestParseDTCs(t *testing.T) {
	can := [][]byte{{0x43, 0x02, 0x01, 0x33, 0xC1, 0x71}}
	if got, want := ParseDTCs(can, true), []string{"P0133", "U0171"}; !reflect.DeepEqual(got, want) {
		t.Errorf("CAN: %v, want %v", got, want)
	}
	kline := [][]byte{{0x43, 0x01, 0x33, 0x03, 0x00, 0x00, 0x00}, {0x43, 0x01, 0x33, 0x00, 0x00, 0x00, 0x00}}
	if got, want := ParseDTCs(kline, false), []string{"P0133", "P0300"}; !reflect.DeepEqual(got, want) {
		t.Errorf("K-line: %v, want %v", got, want)
	}
	if got := ParseDTCs([][]byte{{0x43, 0x00}}, true); len(got) != 0 {
		t.Errorf("empty: %v", got)
	}
}

func TestParseVIN(t *testing.T) {
	can := [][]byte{append([]byte{0x49, 0x02, 0x01}, "YV1BW84B1B1234567"...)}
	if v, err := ParseVIN(can, true); err != nil || v != "YV1BW84B1B1234567" {
		t.Errorf("CAN: %q %v", v, err)
	}
	kline := [][]byte{
		{0x49, 0x02, 0x02, 'F', '7', 'N', '2'},
		{0x49, 0x02, 0x01, 0x00, 0x00, 0x00, 'V'},
		{0x49, 0x02, 0x03, '1', '2', '3', '4'},
		{0x49, 0x02, 0x04, '5', '6', '7', '8'},
		{0x49, 0x02, 0x05, '9', 'A', 'B', 'C'},
	}
	if v, err := ParseVIN(kline, false); err != nil || v != "VF7N2123456789ABC" {
		t.Errorf("K-line: %q %v", v, err)
	}
}

func TestDecodeReadiness(t *testing.T) {
	r := DecodeReadiness([]byte{0x81, 0x07, 0x65, 0x04})
	if !r.MIL || r.DTCCount != 1 {
		t.Errorf("MIL/count: %+v", r)
	}
	// C=0x65: catalyst, evap, o2_sensor, o2_sensor_heater supported; D=0x04: evap incomplete.
	want := map[string]string{
		"misfire": "complete", "fuel_system": "complete", "components": "complete",
		"catalyst": "complete", "evap_system": "incomplete", "o2_sensor": "complete", "o2_sensor_heater": "complete",
	}
	if !reflect.DeepEqual(r.Monitors, want) {
		t.Errorf("monitors = %v, want %v", r.Monitors, want)
	}
}

func TestFormatDTC(t *testing.T) {
	if got := FormatDTC(0x01, 0x33); got != "P0133" {
		t.Error(got)
	}
	if got := FormatDTC(0x92, 0x34); got != "B1234" {
		t.Error(got)
	}
}

func TestGroups(t *testing.T) {
	gs := Groups(All)
	seen := map[byte]bool{}
	for _, g := range gs {
		if seen[g.Code] {
			t.Errorf("PID %02X in two groups", g.Code)
		}
		seen[g.Code] = true
		for _, p := range g.PIDs {
			if p.Bytes > g.Bytes || g.Rate > p.Rate {
				t.Errorf("%s: group bytes %d rate %v", p.Name, g.Bytes, g.Rate)
			}
		}
	}
	names := map[string]bool{}
	for _, p := range All {
		if names[p.Name] {
			t.Errorf("duplicate name %s", p.Name)
		}
		names[p.Name] = true
	}
	// O2 voltage and trim come back from one request.
	c := &Client{R: fakeReq{"0114": {{0x41, 0x14, 0x5A, 0x80}}}}
	for _, g := range gs {
		if g.Code == 0x14 {
			vs, err := c.ReadGroup(context.Background(), g)
			if err != nil || len(vs) != 2 || vs[0] != 0.45 || vs[1] != 0 {
				t.Errorf("o2 group: %v %v", vs, err)
			}
		}
	}
	if v := o2trim([]byte{0x5A, 0xFF}); !math.IsNaN(v) {
		t.Errorf("unused O2 trim = %v, want NaN", v)
	}
}

func TestParseMonitorTests(t *testing.T) {
	// Cylinder 3 misfire monitor: TID 0C, counts, 7 misfires, limits 0..65535;
	// and a failing O2 test in mV.
	ts := ParseMonitorTests([]byte{0x46,
		0xA4, 0x0C, 0x24, 0x00, 0x07, 0x00, 0x00, 0xFF, 0xFF,
		0x01, 0x01, 0x0A, 0x10, 0x00, 0x00, 0x00, 0x0F, 0x00})
	if len(ts) != 2 {
		t.Fatalf("%+v", ts)
	}
	if m := ts[0]; m.Cylinder() != 3 || m.Value != 7 || !m.Pass || m.Unit != "count" || m.Monitor != "misfire cylinder 3" {
		t.Errorf("misfire: %+v", m)
	}
	if o := ts[1]; o.Pass || o.Unit != "mV" || math.Abs(o.Value-4096*0.122) > 1e-6 || o.Cylinder() != 0 {
		t.Errorf("o2: %+v", o)
	}
}

func TestVehicleInfo(t *testing.T) {
	calid := []byte("CAL123\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00")
	can := &Client{CAN: true, R: fakeReq{
		"0900": {{0x49, 0x00, 0x54, 0x40, 0x00, 0x00}}, // 02 04 06 0A
		"0904": {append([]byte{0x49, 0x04, 0x01}, calid...)},
		"0906": {{0x49, 0x06, 0x01, 0xDE, 0xAD, 0xBE, 0xEF}},
		"090A": {append([]byte{0x49, 0x0A, 0x01}, []byte("ECM\x00-EngineControl\x00")...)},
	}}
	vi, err := can.ReadVehicleInfo(context.Background())
	if err != nil || !reflect.DeepEqual(vi.CalibrationIDs, []string{"CAL123"}) || !reflect.DeepEqual(vi.CVNs, []string{"DEADBEEF"}) ||
		!reflect.DeepEqual(vi.ECUNames, []string{"ECM-EngineControl"}) {
		t.Errorf("CAN: %+v %v", vi, err)
	}
	// K-line splits answers into numbered 4-byte messages and puts a count
	// before the bitmap.
	kline := &Client{R: fakeReq{
		"0900": {{0x49, 0x00, 0x01, 0x14, 0x00, 0x00, 0x00}}, // 04 06
		"0904": {{0x49, 0x04, 0x02, 0x34, 0x00, 0x00, 0x00}, {0x49, 0x04, 0x01, 0x41, 0x42, 0x43, 0x31},
			{0x49, 0x04, 0x03, 0, 0, 0, 0}, {0x49, 0x04, 0x04, 0, 0, 0, 0}},
		"0906": {{0x49, 0x06, 0x01, 0x01, 0x02, 0x03, 0x04}},
	}}
	vi, err = kline.ReadVehicleInfo(context.Background())
	if err != nil || !reflect.DeepEqual(vi.CalibrationIDs, []string{"ABC14"}) || !reflect.DeepEqual(vi.CVNs, []string{"01020304"}) {
		t.Errorf("K-line: %+v %v", vi, err)
	}
}

func TestFreezeFrame(t *testing.T) {
	c := &Client{R: fakeReq{
		"020200": {{0x42, 0x02, 0x00, 0x04, 0x20}},
		"020000": {{0x42, 0x00, 0x00, 0x48, 0x18, 0x00, 0x00}}, // 02 05 0C 0D
		"020500": {{0x42, 0x05, 0x00, 0x7B}},
		"020C00": {{0x42, 0x0C, 0x00, 0x1A, 0xF8}},
		"020D00": {{0x42, 0x0D, 0x00, 0x32}},
	}}
	ff, err := c.ReadFreezeFrame(context.Background())
	if err != nil || ff == nil || ff.DTC != "P0420" || ff.Values["coolant_temp"] != 83 || ff.Values["rpm"] != 1726 || ff.Values["speed"] != 50 {
		t.Errorf("%+v %v", ff, err)
	}
	none := &Client{R: fakeReq{"020200": {{0x42, 0x02, 0x00, 0x00, 0x00}}}}
	if ff, err := none.ReadFreezeFrame(context.Background()); ff != nil || err != nil {
		t.Errorf("no frame: %+v %v", ff, err)
	}
}
