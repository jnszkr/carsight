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
