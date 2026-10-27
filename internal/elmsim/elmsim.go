// Package elmsim simulates an ELM327 adapter plugged into a car, so the
// logger and analyser can be developed and tested without hardware.
package elmsim

import (
	"bytes"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"slices"
	"strings"
	"sync"
	"time"
)

// Car describes the simulated vehicle.
type Car struct {
	VIN         string
	CAN         bool
	PIDs        []byte   // supported mode 01 PIDs (01..0x5F)
	DTCs        []string // stored codes, e.g. "P0133"
	Pending     []string
	AmbientC    float64
	LTFT        float64 // long-term fuel trim offset in %, to simulate a lean/rich engine
	CoolantMaxC float64 // steady-state coolant temperature; ~90 for a healthy thermostat
	// Speedup makes simulated time run faster than wall-clock time.
	Speedup float64
	// Latency is how long each OBD request takes to answer, like a real
	// adapter waiting for the ECU.
	Latency time.Duration
}

// Volvo is a 2011 Volvo V70-like car on CAN.
func Volvo() Car {
	return Car{
		VIN: "YV1BW84B1B1234567", CAN: true,
		PIDs:     []byte{0x01, 0x04, 0x05, 0x06, 0x07, 0x0B, 0x0C, 0x0D, 0x0E, 0x0F, 0x10, 0x11, 0x13, 0x14, 0x15, 0x1C, 0x20, 0x21, 0x40, 0x42},
		AmbientC: 12, CoolantMaxC: 90, Speedup: 1, Latency: 30 * time.Millisecond,
	}
}

// Citroen is a 2006 Citroën C4-like car on KWP2000.
func Citroen() Car {
	return Car{
		VIN: "VF7LC9HZC12345678", CAN: false,
		PIDs:     []byte{0x01, 0x04, 0x05, 0x06, 0x07, 0x0B, 0x0C, 0x0D, 0x0E, 0x0F, 0x11, 0x13, 0x14, 0x15, 0x1C, 0x20},
		AmbientC: 12, CoolantMaxC: 88, Speedup: 1, Latency: 60 * time.Millisecond,
	}
}

// Adapter is an io.ReadWriter that behaves like an ELM327 attached to Car.
type Adapter struct {
	mu      sync.Mutex
	car     Car
	start   time.Time
	now     func() time.Time
	in      strings.Builder
	out     bytes.Buffer
	echo    bool
	spaces  bool
	rng     *rand.Rand
	cleared bool
}

// New returns a simulated adapter. Simulated time starts at the first call.
func New(car Car) *Adapter {
	if car.Speedup == 0 {
		car.Speedup = 1
	}
	return &Adapter{car: car, now: time.Now, echo: true, spaces: true, rng: rand.New(rand.NewPCG(1, 2))}
}

func (a *Adapter) Write(p []byte) (int, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.in.Write(p)
	for {
		buf := a.in.String()
		i := strings.IndexByte(buf, '\r')
		if i < 0 {
			break
		}
		a.in.Reset()
		a.in.WriteString(buf[i+1:])
		cmd := strings.ToUpper(strings.ReplaceAll(buf[:i], " ", ""))
		if a.echo {
			a.out.WriteString(buf[:i] + "\r")
		}
		if !strings.HasPrefix(cmd, "AT") && cmd != "" && a.car.Latency > 0 {
			time.Sleep(a.car.Latency)
		}
		a.out.WriteString(a.handle(cmd) + "\r\r>")
	}
	return len(p), nil
}

// Read returns buffered output, or io.EOF when there is none (like a serial
// port read timing out).
func (a *Adapter) Read(p []byte) (int, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.out.Len() == 0 {
		return 0, io.EOF
	}
	return a.out.Read(p)
}

func (a *Adapter) handle(cmd string) string {
	switch {
	case cmd == "":
		return ""
	case cmd == "ATZ":
		a.echo, a.spaces = true, true
		return "\rELM327 v1.5"
	case cmd == "ATE0":
		a.echo = false
		return "OK"
	case cmd == "ATS0":
		a.spaces = false
		return "OK"
	case cmd == "ATDPN":
		if a.car.CAN {
			return "A6"
		}
		return "A5"
	case strings.HasPrefix(cmd, "AT"):
		return "OK"
	}
	if a.start.IsZero() {
		a.start = a.now()
	}
	if len(cmd)%2 == 1 && a.car.CAN {
		cmd = cmd[:len(cmd)-1] // drop the response-count hint
	}
	switch {
	case cmd == "03":
		return a.dtcResponse(0x43, a.car.DTCs)
	case cmd == "07":
		return a.dtcResponse(0x47, a.car.Pending)
	case cmd == "04":
		a.car.DTCs, a.car.Pending, a.cleared = nil, nil, true
		return a.hex([]byte{0x44})
	case cmd == "0902":
		return a.vinResponse()
	case strings.HasPrefix(cmd, "01") && len(cmd) == 4:
		var pid byte
		if _, err := fmt.Sscanf(cmd[2:], "%02X", &pid); err != nil {
			return "?"
		}
		data, ok := a.mode01(pid)
		if !ok {
			return "NO DATA"
		}
		return a.hex(append([]byte{0x41, pid}, data...))
	}
	return "?"
}

func (a *Adapter) hex(b []byte) string {
	var sb strings.Builder
	for i, c := range b {
		if i > 0 && a.spaces {
			sb.WriteByte(' ')
		}
		fmt.Fprintf(&sb, "%02X", c)
	}
	return sb.String()
}

func (a *Adapter) supported(pid byte) bool { return slices.Contains(a.car.PIDs, pid) }

func (a *Adapter) mode01(pid byte) ([]byte, bool) {
	if pid%0x20 == 0 { // supported-PID bitmap
		var bits uint32
		any := false
		for _, p := range a.car.PIDs {
			if p > pid && p <= pid+0x20 {
				bits |= 1 << (32 - int(p-pid))
				any = true
			}
		}
		if !any && pid != 0 {
			return nil, false
		}
		return []byte{byte(bits >> 24), byte(bits >> 16), byte(bits >> 8), byte(bits)}, true
	}
	if !a.supported(pid) {
		return nil, false
	}
	s := a.state()
	u8 := func(v float64) byte { return byte(math.Max(0, math.Min(255, math.Round(v)))) }
	u16 := func(v float64) []byte {
		n := uint16(math.Max(0, math.Min(65535, math.Round(v))))
		return []byte{byte(n >> 8), byte(n)}
	}
	switch pid {
	case 0x01:
		a0 := byte(len(a.car.DTCs))
		if len(a.car.DTCs) > 0 {
			a0 |= 0x80
		}
		d := byte(0)
		if a.cleared {
			d = 0x65 // monitors not yet re-run after a clear
		}
		return []byte{a0, 0x07, 0x65, d}, true
	case 0x04:
		return []byte{u8(s.load * 255 / 100)}, true
	case 0x05:
		return []byte{u8(s.coolant + 40)}, true
	case 0x06:
		return []byte{u8((s.stft + 100) * 128 / 100)}, true
	case 0x07:
		return []byte{u8((a.car.LTFT + 100) * 128 / 100)}, true
	case 0x0B:
		return []byte{u8(s.mapKPa)}, true
	case 0x0C:
		return u16(s.rpm * 4), true
	case 0x0D:
		return []byte{u8(s.speed)}, true
	case 0x0E:
		return []byte{u8((s.timing + 64) * 2)}, true
	case 0x0F:
		return []byte{u8(s.intake + 40)}, true
	case 0x10:
		return u16(s.maf * 100), true
	case 0x11:
		return []byte{u8(s.throttle * 255 / 100)}, true
	case 0x13:
		return []byte{0x03}, true
	case 0x14:
		return []byte{u8(s.o2 * 200), 0xFF}, true
	case 0x15:
		return []byte{u8(0.7 * 200), 0xFF}, true
	case 0x1C:
		return []byte{0x06}, true // EOBD
	case 0x42:
		return u16(s.voltage * 1000), true
	}
	return []byte{0x00}, true
}

type state struct {
	rpm, speed, load, throttle, maf, mapKPa, coolant, intake, stft, timing, o2, voltage float64
}

// state models a short urban drive: idle while warming up, then a repeating
// cycle of accelerate / cruise / brake / stop.
func (a *Adapter) state() state {
	t := a.now().Sub(a.start).Seconds() * a.car.Speedup
	n := func(sd float64) float64 { return a.rng.NormFloat64() * sd }
	var s state

	cycle := math.Mod(math.Max(0, t-60), 120)
	switch {
	case t < 60 || cycle >= 100: // idle
		s.speed = 0
	case cycle < 15: // accelerate to 50
		s.speed = cycle / 15 * 50
		s.throttle = 35
	case cycle < 80: // cruise
		s.speed = 50 + 5*math.Sin(cycle/5)
		s.throttle = 18
	default: // brake
		s.speed = 50 * (100 - cycle) / 20
		s.throttle = 0
	}
	s.coolant = a.car.CoolantMaxC - (a.car.CoolantMaxC-a.car.AmbientC)*math.Exp(-t/240)
	idle := 750.0
	if s.coolant < 60 {
		idle = 750 + (60-s.coolant)*8 // fast idle when cold
	}
	if s.speed < 1 {
		s.rpm = idle + n(15)
		s.throttle = 0
		s.load = 22 + n(1)
	} else {
		s.rpm = math.Max(idle, s.speed*45) + s.throttle*10 + n(30)
		s.load = 25 + s.throttle*1.5 + n(2)
	}
	s.throttle = math.Max(0, s.throttle+n(0.5))
	s.speed = math.Max(0, s.speed+n(0.3))
	s.maf = s.rpm * s.load / 6000
	s.mapKPa = 25 + s.load*0.75
	s.intake = a.car.AmbientC + 8*(1-math.Exp(-t/300)) + n(0.3)
	s.stft = n(2.5)
	s.timing = 8 + (s.rpm-750)/200 - s.load/10 + n(1)
	if s.coolant < 40 {
		s.o2 = 0.45 // sensor not warm yet
	} else {
		s.o2 = 0.45 + 0.4*math.Sin(t*6)
	}
	s.voltage = 14.1 + n(0.05)
	return s
}

func (a *Adapter) dtcResponse(sid byte, codes []string) string {
	var data []byte
	for _, c := range codes {
		data = append(data, encodeDTC(c)...)
	}
	if a.car.CAN {
		payload := append([]byte{sid, byte(len(codes))}, data...)
		if len(payload) <= 7 {
			return a.hex(payload)
		}
		return a.multiFrame(payload)
	}
	if len(codes) == 0 {
		return a.hex([]byte{sid, 0, 0, 0, 0, 0, 0})
	}
	var lines []string
	for i := 0; i < len(data); i += 6 {
		chunk := make([]byte, 6)
		copy(chunk, data[i:])
		lines = append(lines, a.hex(append([]byte{sid}, chunk...)))
	}
	return strings.Join(lines, "\r")
}

func (a *Adapter) vinResponse() string {
	if a.car.CAN {
		return a.multiFrame(append([]byte{0x49, 0x02, 0x01}, a.car.VIN...))
	}
	padded := append([]byte{0, 0, 0}, a.car.VIN...)
	var lines []string
	for seq := range 5 {
		lines = append(lines, a.hex(append([]byte{0x49, 0x02, byte(seq + 1)}, padded[seq*4:seq*4+4]...)))
	}
	return strings.Join(lines, "\r")
}

// multiFrame formats a payload the way the ELM327 prints an ISO-TP
// multi-frame response with headers off.
func (a *Adapter) multiFrame(payload []byte) string {
	lines := []string{fmt.Sprintf("%03X", len(payload))}
	idx := 0
	for i := 0; i < len(payload); {
		size := 7
		if i == 0 {
			size = 6
		}
		end := min(i+size, len(payload))
		chunk := make([]byte, size)
		copy(chunk, payload[i:end])
		lines = append(lines, fmt.Sprintf("%X:%s", idx%16, a.hex(chunk)))
		idx++
		i = end
	}
	return strings.Join(lines, "\r")
}

func encodeDTC(code string) []byte {
	sys := strings.IndexByte("PCBU", code[0])
	var rest uint16
	fmt.Sscanf(code[1:], "%04X", &rest)
	return []byte{byte(sys<<6) | byte(rest>>8), byte(rest)}
}
