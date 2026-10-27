// Package obd defines the standard OBD-II PIDs carsight logs and decodes
// mode 01 (live data), mode 03/07 (DTCs) and mode 09 (VIN) responses.
package obd

import "fmt"

// PID describes a mode 01 parameter.
type PID struct {
	Code  byte
	Name  string // stable identifier used in storage, e.g. "rpm"
	Desc  string
	Unit  string
	Bytes int
	// Slow PIDs change slowly (temperatures, voltage) and are polled less
	// often to keep the sample rate of the fast group high.
	Slow   bool
	Decode func(b []byte) float64
}

// The v1 PID set from the README.
var (
	EngineLoad    = PID{0x04, "engine_load", "Calculated engine load", "%", 1, false, pct}
	CoolantTemp   = PID{0x05, "coolant_temp", "Engine coolant temperature", "°C", 1, true, temp}
	STFTBank1     = PID{0x06, "stft_b1", "Short term fuel trim, bank 1", "%", 1, false, trim}
	LTFTBank1     = PID{0x07, "ltft_b1", "Long term fuel trim, bank 1", "%", 1, true, trim}
	MAP           = PID{0x0B, "map", "Intake manifold absolute pressure", "kPa", 1, false, raw}
	RPM           = PID{0x0C, "rpm", "Engine speed", "rpm", 2, false, func(b []byte) float64 { return float64(u16(b)) / 4 }}
	Speed         = PID{0x0D, "speed", "Vehicle speed", "km/h", 1, false, raw}
	TimingAdvance = PID{0x0E, "timing_advance", "Timing advance", "° before TDC", 1, false, func(b []byte) float64 { return float64(b[0])/2 - 64 }}
	IntakeTemp    = PID{0x0F, "intake_temp", "Intake air temperature", "°C", 1, true, temp}
	MAF           = PID{0x10, "maf", "Mass air flow", "g/s", 2, false, func(b []byte) float64 { return float64(u16(b)) / 100 }}
	Throttle      = PID{0x11, "throttle", "Throttle position", "%", 1, false, pct}
	O2B1S1        = PID{0x14, "o2_b1s1", "O2 sensor voltage, bank 1 sensor 1", "V", 2, false, o2}
	O2B1S2        = PID{0x15, "o2_b1s2", "O2 sensor voltage, bank 1 sensor 2", "V", 2, true, o2}
	ModuleVoltage = PID{0x42, "module_voltage", "Control module voltage", "V", 2, true, func(b []byte) float64 { return float64(u16(b)) / 1000 }}
)

// Default is the v1 PID set, in polling order.
var Default = []PID{
	RPM, Speed, EngineLoad, Throttle, MAF, MAP, STFTBank1, TimingAdvance, O2B1S1,
	CoolantTemp, IntakeTemp, LTFTBank1, O2B1S2, ModuleVoltage,
}

// ByName looks up a PID from the default set by its storage name.
func ByName(name string) (PID, bool) {
	for _, p := range Default {
		if p.Name == name {
			return p, true
		}
	}
	return PID{}, false
}

func raw(b []byte) float64  { return float64(b[0]) }
func pct(b []byte) float64  { return float64(b[0]) * 100 / 255 }
func temp(b []byte) float64 { return float64(b[0]) - 40 }
func trim(b []byte) float64 { return float64(b[0])*100/128 - 100 }
func o2(b []byte) float64   { return float64(b[0]) / 200 }
func u16(b []byte) int      { return int(b[0])<<8 | int(b[1]) }

// DecodeMode01 extracts the value for p from a mode 01 response payload
// (starting with 0x41).
func DecodeMode01(p PID, payload []byte) (float64, error) {
	if len(payload) < 2+p.Bytes || payload[0] != 0x41 || payload[1] != p.Code {
		return 0, fmt.Errorf("obd: unexpected response % X for PID %02X", payload, p.Code)
	}
	return p.Decode(payload[2 : 2+p.Bytes]), nil
}
