// Package obd defines the standard OBD-II PIDs carsight logs and decodes
// mode 01 (live data), mode 02 (freeze frame), mode 03/07/0A (DTCs), mode 06
// (monitor test results) and mode 09 (vehicle information) responses.
package obd

import (
	"fmt"
	"math"
)

// Rate says how often a PID is polled during a drive.
type Rate int

const (
	// Fast PIDs are polled every cycle: they change quickly and drive the
	// analysis (RPM, speed, load, airflow, short-term trims, O2).
	Fast Rate = iota
	// Slow PIDs (temperatures, long-term trims, voltage) are polled about
	// every SlowEvery cycles, a few per cycle in rotation.
	Slow
	// Extra PIDs (levels, pressures, counters, secondary sensors) are polled
	// in a slower rotation, about every ExtraEvery cycles.
	Extra
	// Once PIDs describe the car or change rarely (OBD standard, fuel type,
	// sensor layout). They are read into the session snapshot at the start
	// and end of a drive but not polled.
	Once
)

func (r Rate) String() string {
	return [...]string{"fast", "slow", "extra", "once"}[r]
}

func (r Rate) MarshalText() ([]byte, error) { return []byte(r.String()), nil }

func (r *Rate) UnmarshalText(b []byte) error {
	for i, n := range [...]string{"fast", "slow", "extra", "once"} {
		if string(b) == n {
			*r = Rate(i)
			return nil
		}
	}
	return fmt.Errorf("obd: unknown rate %q", b)
}

// PID describes one value carried by a mode 01 PID. A PID that carries
// several values (an O2 sensor's voltage and trim) has one entry per value,
// sharing Code; they are read with one request.
type PID struct {
	Code byte
	Name string // stable identifier used in storage, e.g. "rpm"
	Desc string
	Unit string
	// Bytes is the number of data bytes the PID returns.
	Bytes int
	Rate  Rate
	// Decode turns the data bytes into the value. NaN means the car
	// reports the value as not available (e.g. an unused O2 trim).
	Decode func(b []byte) float64
	// Labels names the values of enumerated PIDs, e.g. fuel system status.
	Labels map[int]string
}

// The PIDs the analysis refers to by name.
var (
	EngineLoad    = PID{Code: 0x04, Name: "engine_load", Desc: "Calculated engine load", Unit: "%", Bytes: 1, Decode: pct}
	CoolantTemp   = PID{Code: 0x05, Name: "coolant_temp", Desc: "Engine coolant temperature", Unit: "°C", Bytes: 1, Rate: Slow, Decode: temp}
	STFTBank1     = PID{Code: 0x06, Name: "stft_b1", Desc: "Short term fuel trim, bank 1", Unit: "%", Bytes: 1, Decode: trim}
	LTFTBank1     = PID{Code: 0x07, Name: "ltft_b1", Desc: "Long term fuel trim, bank 1", Unit: "%", Bytes: 1, Rate: Slow, Decode: trim}
	MAP           = PID{Code: 0x0B, Name: "map", Desc: "Intake manifold absolute pressure", Unit: "kPa", Bytes: 1, Decode: raw}
	RPM           = PID{Code: 0x0C, Name: "rpm", Desc: "Engine speed", Unit: "rpm", Bytes: 2, Decode: func(b []byte) float64 { return float64(u16(b)) / 4 }}
	Speed         = PID{Code: 0x0D, Name: "speed", Desc: "Vehicle speed", Unit: "km/h", Bytes: 1, Decode: raw}
	TimingAdvance = PID{Code: 0x0E, Name: "timing_advance", Desc: "Timing advance", Unit: "° before TDC", Bytes: 1, Decode: func(b []byte) float64 { return float64(b[0])/2 - 64 }}
	IntakeTemp    = PID{Code: 0x0F, Name: "intake_temp", Desc: "Intake air temperature", Unit: "°C", Bytes: 1, Rate: Slow, Decode: temp}
	MAF           = PID{Code: 0x10, Name: "maf", Desc: "Mass air flow", Unit: "g/s", Bytes: 2, Decode: func(b []byte) float64 { return float64(u16(b)) / 100 }}
	Throttle      = PID{Code: 0x11, Name: "throttle", Desc: "Throttle position", Unit: "%", Bytes: 1, Decode: pct}
	O2B1S1        = PID{Code: 0x14, Name: "o2_b1s1", Desc: "O2 sensor voltage, bank 1 sensor 1", Unit: "V", Bytes: 2, Decode: o2}
	O2B1S2        = PID{Code: 0x15, Name: "o2_b1s2", Desc: "O2 sensor voltage, bank 1 sensor 2", Unit: "V", Bytes: 2, Rate: Slow, Decode: o2}
	ModuleVoltage = PID{Code: 0x42, Name: "module_voltage", Desc: "Control module voltage", Unit: "V", Bytes: 2, Rate: Slow, Decode: func(b []byte) float64 { return float64(u16(b)) / 1000 }}
)

// FuelSystemLabels are the values of PID 03 (fuel system status).
var FuelSystemLabels = map[int]string{
	0: "off", 1: "open loop, warming up", 2: "closed loop", 4: "open loop, load or decel",
	8: "open loop, system fault", 16: "closed loop, feedback fault",
}

// All is every PID carsight can decode, in polling order: the analysis set
// first, then the rest of SAE J1979 mode 01 by code. A drive polls the ones
// the car supports. The O2 sensor PIDs 16-1B, 24-2B and 34-3B are numbered
// by sensor; which bank and position each one is depends on PID 13 or 1D.
var All = func() []PID {
	ps := []PID{
		RPM, Speed, EngineLoad, Throttle, MAF, MAP, STFTBank1, TimingAdvance, O2B1S1,
		CoolantTemp, IntakeTemp, LTFTBank1, O2B1S2, ModuleVoltage,

		{Code: 0x03, Name: "fuel_system", Desc: "Fuel system status (bank 1)", Unit: "code", Bytes: 2, Rate: Slow, Decode: raw, Labels: FuelSystemLabels},
		{Code: 0x08, Name: "stft_b2", Desc: "Short term fuel trim, bank 2", Unit: "%", Bytes: 1, Decode: trim},
		{Code: 0x09, Name: "ltft_b2", Desc: "Long term fuel trim, bank 2", Unit: "%", Bytes: 1, Rate: Slow, Decode: trim},
		{Code: 0x0A, Name: "fuel_pressure", Desc: "Fuel pressure (gauge)", Unit: "kPa", Bytes: 1, Rate: Extra, Decode: func(b []byte) float64 { return 3 * float64(b[0]) }},
		{Code: 0x12, Name: "secondary_air", Desc: "Commanded secondary air status", Unit: "code", Bytes: 1, Rate: Extra, Decode: raw,
			Labels: map[int]string{1: "upstream", 2: "downstream of catalyst", 4: "outside atmosphere or off", 8: "pump on for diagnostics"}},
		{Code: 0x13, Name: "o2_sensors_present", Desc: "O2 sensors present (2-bank layout, bit mask)", Unit: "mask", Bytes: 1, Rate: Once, Decode: raw},
		{Code: 0x14, Name: "o2_b1s1_trim", Desc: "Short term fuel trim from O2 sensor 1", Unit: "%", Bytes: 2, Decode: o2trim},
		{Code: 0x15, Name: "o2_b1s2_trim", Desc: "Short term fuel trim from O2 sensor 2", Unit: "%", Bytes: 2, Rate: Slow, Decode: o2trim},
		{Code: 0x1C, Name: "obd_standard", Desc: "OBD standard the vehicle conforms to", Unit: "code", Bytes: 1, Rate: Once, Decode: raw, Labels: OBDStandards},
		{Code: 0x1D, Name: "o2_sensors_present_4bank", Desc: "O2 sensors present (4-bank layout, bit mask)", Unit: "mask", Bytes: 1, Rate: Once, Decode: raw},
		{Code: 0x1E, Name: "pto_active", Desc: "Power take-off active", Unit: "bool", Bytes: 1, Rate: Extra, Decode: func(b []byte) float64 { return float64(b[0] & 1) }},
		{Code: 0x1F, Name: "run_time", Desc: "Run time since engine start", Unit: "s", Bytes: 2, Rate: Extra, Decode: u16f},
		{Code: 0x21, Name: "distance_mil_on", Desc: "Distance travelled with the MIL on", Unit: "km", Bytes: 2, Rate: Extra, Decode: u16f},
		{Code: 0x22, Name: "fuel_rail_pressure_rel", Desc: "Fuel rail pressure (relative to manifold vacuum)", Unit: "kPa", Bytes: 2, Rate: Extra, Decode: func(b []byte) float64 { return 0.079 * float64(u16(b)) }},
		{Code: 0x23, Name: "fuel_rail_pressure", Desc: "Fuel rail gauge pressure (diesel, direct injection)", Unit: "kPa", Bytes: 2, Rate: Extra, Decode: func(b []byte) float64 { return 10 * float64(u16(b)) }},
		{Code: 0x2C, Name: "egr_commanded", Desc: "Commanded EGR", Unit: "%", Bytes: 1, Rate: Extra, Decode: pct},
		{Code: 0x2D, Name: "egr_error", Desc: "EGR error", Unit: "%", Bytes: 1, Rate: Extra, Decode: trim},
		{Code: 0x2E, Name: "evap_purge", Desc: "Commanded evaporative purge", Unit: "%", Bytes: 1, Rate: Extra, Decode: pct},
		{Code: 0x2F, Name: "fuel_level", Desc: "Fuel tank level", Unit: "%", Bytes: 1, Rate: Extra, Decode: pct},
		{Code: 0x30, Name: "warmups_since_clear", Desc: "Warm-ups since codes cleared", Unit: "count", Bytes: 1, Rate: Extra, Decode: raw},
		{Code: 0x31, Name: "distance_since_clear", Desc: "Distance travelled since codes cleared", Unit: "km", Bytes: 2, Rate: Extra, Decode: u16f},
		{Code: 0x32, Name: "evap_vapor_pressure", Desc: "Evap system vapour pressure", Unit: "Pa", Bytes: 2, Rate: Extra, Decode: func(b []byte) float64 { return float64(int16(u16(b))) / 4 }},
		{Code: 0x33, Name: "baro", Desc: "Barometric pressure", Unit: "kPa", Bytes: 1, Rate: Extra, Decode: raw},
		{Code: 0x3C, Name: "cat_temp_b1s1", Desc: "Catalyst temperature, bank 1 sensor 1", Unit: "°C", Bytes: 2, Rate: Slow, Decode: catTemp},
		{Code: 0x3D, Name: "cat_temp_b2s1", Desc: "Catalyst temperature, bank 2 sensor 1", Unit: "°C", Bytes: 2, Rate: Slow, Decode: catTemp},
		{Code: 0x3E, Name: "cat_temp_b1s2", Desc: "Catalyst temperature, bank 1 sensor 2", Unit: "°C", Bytes: 2, Rate: Extra, Decode: catTemp},
		{Code: 0x3F, Name: "cat_temp_b2s2", Desc: "Catalyst temperature, bank 2 sensor 2", Unit: "°C", Bytes: 2, Rate: Extra, Decode: catTemp},
		{Code: 0x43, Name: "absolute_load", Desc: "Absolute load value", Unit: "%", Bytes: 2, Rate: Slow, Decode: func(b []byte) float64 { return float64(u16(b)) * 100 / 255 }},
		{Code: 0x44, Name: "commanded_lambda", Desc: "Commanded air-fuel equivalence ratio (lambda)", Unit: "λ", Bytes: 2, Decode: lambda},
		{Code: 0x45, Name: "relative_throttle", Desc: "Relative throttle position", Unit: "%", Bytes: 1, Rate: Extra, Decode: pct},
		{Code: 0x46, Name: "ambient_temp", Desc: "Ambient air temperature", Unit: "°C", Bytes: 1, Rate: Extra, Decode: temp},
		{Code: 0x47, Name: "throttle_b", Desc: "Absolute throttle position B", Unit: "%", Bytes: 1, Rate: Extra, Decode: pct},
		{Code: 0x48, Name: "throttle_c", Desc: "Absolute throttle position C", Unit: "%", Bytes: 1, Rate: Extra, Decode: pct},
		{Code: 0x49, Name: "pedal_d", Desc: "Accelerator pedal position D", Unit: "%", Bytes: 1, Rate: Slow, Decode: pct},
		{Code: 0x4A, Name: "pedal_e", Desc: "Accelerator pedal position E", Unit: "%", Bytes: 1, Rate: Extra, Decode: pct},
		{Code: 0x4B, Name: "pedal_f", Desc: "Accelerator pedal position F", Unit: "%", Bytes: 1, Rate: Extra, Decode: pct},
		{Code: 0x4C, Name: "throttle_commanded", Desc: "Commanded throttle actuator", Unit: "%", Bytes: 1, Rate: Slow, Decode: pct},
		{Code: 0x4D, Name: "time_mil_on", Desc: "Time run with the MIL on", Unit: "min", Bytes: 2, Rate: Extra, Decode: u16f},
		{Code: 0x4E, Name: "time_since_clear", Desc: "Time since codes cleared", Unit: "min", Bytes: 2, Rate: Extra, Decode: u16f},
		{Code: 0x51, Name: "fuel_type", Desc: "Fuel type", Unit: "code", Bytes: 1, Rate: Once, Decode: raw, Labels: FuelTypes},
		{Code: 0x52, Name: "ethanol_pct", Desc: "Ethanol fuel percentage", Unit: "%", Bytes: 1, Rate: Extra, Decode: pct},
		{Code: 0x53, Name: "evap_vapor_pressure_abs", Desc: "Absolute evap system vapour pressure", Unit: "kPa", Bytes: 2, Rate: Extra, Decode: func(b []byte) float64 { return float64(u16(b)) / 200 }},
		{Code: 0x54, Name: "evap_vapor_pressure_wide", Desc: "Evap system vapour pressure (wide range)", Unit: "Pa", Bytes: 2, Rate: Extra, Decode: func(b []byte) float64 { return float64(int16(u16(b))) }},
		{Code: 0x55, Name: "secondary_o2_stft_b1", Desc: "Short term secondary O2 trim, bank 1", Unit: "%", Bytes: 2, Rate: Extra, Decode: trim},
		{Code: 0x56, Name: "secondary_o2_ltft_b1", Desc: "Long term secondary O2 trim, bank 1", Unit: "%", Bytes: 2, Rate: Extra, Decode: trim},
		{Code: 0x57, Name: "secondary_o2_stft_b2", Desc: "Short term secondary O2 trim, bank 2", Unit: "%", Bytes: 2, Rate: Extra, Decode: trim},
		{Code: 0x58, Name: "secondary_o2_ltft_b2", Desc: "Long term secondary O2 trim, bank 2", Unit: "%", Bytes: 2, Rate: Extra, Decode: trim},
		{Code: 0x59, Name: "fuel_rail_pressure_abs", Desc: "Fuel rail absolute pressure", Unit: "kPa", Bytes: 2, Rate: Extra, Decode: func(b []byte) float64 { return 10 * float64(u16(b)) }},
		{Code: 0x5A, Name: "pedal_relative", Desc: "Relative accelerator pedal position", Unit: "%", Bytes: 1, Rate: Extra, Decode: pct},
		{Code: 0x5B, Name: "hybrid_battery", Desc: "Hybrid battery pack remaining life", Unit: "%", Bytes: 1, Rate: Extra, Decode: pct},
		{Code: 0x5C, Name: "oil_temp", Desc: "Engine oil temperature", Unit: "°C", Bytes: 1, Rate: Slow, Decode: temp},
		{Code: 0x5D, Name: "injection_timing", Desc: "Fuel injection timing", Unit: "°", Bytes: 2, Rate: Extra, Decode: func(b []byte) float64 { return float64(u16(b))/128 - 210 }},
		{Code: 0x5E, Name: "fuel_rate", Desc: "Engine fuel rate", Unit: "L/h", Bytes: 2, Rate: Slow, Decode: func(b []byte) float64 { return float64(u16(b)) / 20 }},
		{Code: 0x5F, Name: "emission_standard", Desc: "Emission requirements the vehicle was designed to", Unit: "code", Bytes: 1, Rate: Once, Decode: raw},
		{Code: 0x61, Name: "torque_demand", Desc: "Driver's demand engine torque", Unit: "%", Bytes: 1, Rate: Extra, Decode: torque},
		{Code: 0x62, Name: "torque_actual", Desc: "Actual engine torque", Unit: "%", Bytes: 1, Rate: Slow, Decode: torque},
		{Code: 0x63, Name: "torque_reference", Desc: "Engine reference torque", Unit: "Nm", Bytes: 2, Rate: Once, Decode: u16f},
		{Code: 0x67, Name: "coolant_temp_1", Desc: "Engine coolant temperature, sensor 1", Unit: "°C", Bytes: 3, Rate: Extra, Decode: masked(0x01, 1, temp)},
		{Code: 0x67, Name: "coolant_temp_2", Desc: "Engine coolant temperature, sensor 2", Unit: "°C", Bytes: 3, Rate: Extra, Decode: masked(0x02, 2, temp)},
		{Code: 0x68, Name: "intake_temp_1", Desc: "Intake air temperature, sensor 1", Unit: "°C", Bytes: 3, Rate: Extra, Decode: masked(0x01, 1, temp)},
		{Code: 0x68, Name: "intake_temp_2", Desc: "Intake air temperature, sensor 2", Unit: "°C", Bytes: 3, Rate: Extra, Decode: masked(0x02, 2, temp)},
		{Code: 0x8E, Name: "torque_friction", Desc: "Engine friction, percent torque", Unit: "%", Bytes: 1, Rate: Extra, Decode: torque},
		{Code: 0xA2, Name: "cylinder_fuel_rate", Desc: "Cylinder fuel rate", Unit: "mg/stroke", Bytes: 2, Rate: Extra, Decode: func(b []byte) float64 { return float64(u16(b)) / 32 }},
		{Code: 0xA4, Name: "gear", Desc: "Transmission actual gear", Unit: "gear", Bytes: 4, Rate: Slow, Decode: func(b []byte) float64 {
			if b[0]&0x02 == 0 {
				return math.NaN()
			}
			return float64(b[2] >> 4)
		}},
		{Code: 0xA6, Name: "odometer", Desc: "Odometer", Unit: "km", Bytes: 4, Rate: Extra, Decode: func(b []byte) float64 {
			return float64(uint32(b[0])<<24|uint32(b[1])<<16|uint32(b[2])<<8|uint32(b[3])) / 10
		}},
	}
	// O2 sensors beyond bank 1 sensors 1-2, numbered by sensor.
	for i := range 6 {
		code := byte(0x16 + i)
		ps = append(ps,
			PID{Code: code, Name: fmt.Sprintf("o2_s%d", i+3), Desc: fmt.Sprintf("O2 sensor %d voltage", i+3), Unit: "V", Bytes: 2, Rate: Extra, Decode: o2},
			PID{Code: code, Name: fmt.Sprintf("o2_s%d_trim", i+3), Desc: fmt.Sprintf("Short term fuel trim from O2 sensor %d", i+3), Unit: "%", Bytes: 2, Rate: Extra, Decode: o2trim})
	}
	// Wide-band O2 sensors: lambda with voltage (24-2B) or current (34-3B).
	for i := range 8 {
		rate := Extra
		if i == 0 {
			rate = Fast // the upstream sensor on cars that report wide-band only
		}
		ps = append(ps,
			PID{Code: byte(0x24 + i), Name: fmt.Sprintf("o2w_s%d_lambda", i+1), Desc: fmt.Sprintf("Wide-band O2 sensor %d, lambda", i+1), Unit: "λ", Bytes: 4, Rate: rate, Decode: lambda},
			PID{Code: byte(0x24 + i), Name: fmt.Sprintf("o2w_s%d_voltage", i+1), Desc: fmt.Sprintf("Wide-band O2 sensor %d, voltage", i+1), Unit: "V", Bytes: 4, Rate: Extra, Decode: func(b []byte) float64 { return float64(u16(b[2:])) * 8 / 65536 }},
			PID{Code: byte(0x34 + i), Name: fmt.Sprintf("o2c_s%d_lambda", i+1), Desc: fmt.Sprintf("Wide-band O2 sensor %d, lambda (current type)", i+1), Unit: "λ", Bytes: 4, Rate: rate, Decode: lambda},
			PID{Code: byte(0x34 + i), Name: fmt.Sprintf("o2c_s%d_current", i+1), Desc: fmt.Sprintf("Wide-band O2 sensor %d, current", i+1), Unit: "mA", Bytes: 4, Rate: Extra, Decode: func(b []byte) float64 { return float64(u16(b[2:]))/256 - 128 }})
	}
	return ps
}()

// OBDStandards are the values of PID 1C.
var OBDStandards = map[int]string{
	1: "OBD-II (CARB)", 2: "OBD (EPA)", 3: "OBD and OBD-II", 4: "OBD-I", 5: "not OBD compliant", 6: "EOBD (Europe)",
	7: "EOBD and OBD-II", 8: "EOBD and OBD", 9: "EOBD, OBD and OBD-II", 10: "JOBD (Japan)", 11: "JOBD and OBD-II",
	12: "JOBD and EOBD", 13: "JOBD, EOBD and OBD-II", 17: "EMD", 18: "EMD+", 19: "HD OBD-C", 20: "HD OBD",
	21: "WWH OBD", 23: "HD EOBD-I", 24: "HD EOBD-I N", 25: "HD EOBD-II", 26: "HD EOBD-II N", 28: "OBDBr-1",
	29: "OBDBr-2", 30: "KOBD (Korea)", 31: "IOBD I (India)", 32: "IOBD II (India)", 33: "HD EOBD-IV",
}

// FuelTypes are the values of PID 51.
var FuelTypes = map[int]string{
	0: "not available", 1: "gasoline", 2: "methanol", 3: "ethanol", 4: "diesel", 5: "LPG", 6: "CNG", 7: "propane",
	8: "electric", 9: "bifuel gasoline", 10: "bifuel methanol", 11: "bifuel ethanol", 12: "bifuel LPG", 13: "bifuel CNG",
	14: "bifuel propane", 15: "bifuel electric", 16: "bifuel electric/combustion", 17: "hybrid gasoline",
	18: "hybrid ethanol", 19: "hybrid diesel", 20: "hybrid electric", 21: "hybrid electric/combustion",
	22: "hybrid regenerative", 23: "bifuel diesel",
}

// Default is the set the analysis was built around: the first 14 PIDs of
// All.
var Default = All[:14]

// ByName looks up a PID by its storage name.
func ByName(name string) (PID, bool) {
	for _, p := range All {
		if p.Name == name {
			return p, true
		}
	}
	return PID{}, false
}

// Label returns the label of an enumerated PID's value, or "".
func (p PID) Label(v float64) string {
	return p.Labels[int(v)]
}

// Group is the PIDs answered by one mode 01 request.
type Group struct {
	Code  byte
	Bytes int
	Rate  Rate // the fastest rate of its PIDs
	PIDs  []PID
}

// Groups collects ps by code, keeping the order in which codes first appear.
func Groups(ps []PID) []Group {
	var out []Group
	idx := map[byte]int{}
	for _, p := range ps {
		i, ok := idx[p.Code]
		if !ok {
			i = len(out)
			idx[p.Code] = i
			out = append(out, Group{Code: p.Code, Rate: p.Rate})
		}
		g := &out[i]
		g.PIDs = append(g.PIDs, p)
		g.Bytes = max(g.Bytes, p.Bytes)
		g.Rate = min(g.Rate, p.Rate)
	}
	return out
}

func raw(b []byte) float64     { return float64(b[0]) }
func pct(b []byte) float64     { return float64(b[0]) * 100 / 255 }
func temp(b []byte) float64    { return float64(b[0]) - 40 }
func trim(b []byte) float64    { return float64(b[0])*100/128 - 100 }
func o2(b []byte) float64      { return float64(b[0]) / 200 }
func u16(b []byte) int         { return int(b[0])<<8 | int(b[1]) }
func u16f(b []byte) float64    { return float64(u16(b)) }
func lambda(b []byte) float64  { return float64(u16(b)) * 2 / 65536 }
func catTemp(b []byte) float64 { return float64(u16(b))/10 - 40 }
func torque(b []byte) float64  { return float64(b[0]) - 125 }

// o2trim decodes the fuel trim byte of PIDs 14-1B; FF means the sensor
// isn't used for trim.
func o2trim(b []byte) float64 {
	if b[1] == 0xFF {
		return math.NaN()
	}
	return float64(b[1])*100/128 - 100
}

// masked decodes the value at byte i of a PID whose first byte says which
// sensors are present.
func masked(bit byte, i int, dec func([]byte) float64) func([]byte) float64 {
	return func(b []byte) float64 {
		if b[0]&bit == 0 {
			return math.NaN()
		}
		return dec(b[i:])
	}
}

// DecodeMode01 extracts the value for p from a mode 01 response payload
// (starting with 0x41).
func DecodeMode01(p PID, payload []byte) (float64, error) {
	if len(payload) < 2+p.Bytes || payload[0] != 0x41 || payload[1] != p.Code {
		return 0, fmt.Errorf("obd: unexpected response % X for PID %02X", payload, p.Code)
	}
	return p.Decode(payload[2 : 2+p.Bytes]), nil
}
