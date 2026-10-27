// Package car talks to a car through an ELM327 adapter: it connects, scans
// the car (VIN, supported PIDs, DTCs, readiness), clears trouble codes and
// logs drives into the store. The CLI and the MCP server share it.
package car

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/jnszkr/carsight/internal/elm327"
	"github.com/jnszkr/carsight/internal/obd"
)

// Adapter selects and configures the ELM327 adapter.
type Adapter struct {
	// Port is a serial device path, "auto" (the first USB serial adapter),
	// or "sim:volvo" / "sim:citroen" for the built-in simulator.
	Port string
	// Baud is the serial baud rate; 0 auto-detects.
	Baud int
	// SimSpeedup runs simulated time this many times faster.
	SimSpeedup float64
}

// Car is an open connection to a car.
type Car struct {
	rw     io.ReadWriteCloser
	Conn   *elm327.Conn
	Client *obd.Client
	// Desc describes the adapter, e.g. "/dev/tty.usbserial-X @ 115200 baud".
	Desc string
}

// Connect opens the adapter and initialises the ELM327, which searches for
// the car's OBD protocol. The ignition must be on.
func Connect(ctx context.Context, a Adapter) (*Car, error) {
	if a.Port == "" {
		a.Port = "auto"
	}
	if a.SimSpeedup <= 0 {
		a.SimSpeedup = 1
	}
	rw, desc, err := openAdapter(a.Port, a.Baud, a.SimSpeedup)
	if err != nil {
		return nil, err
	}
	conn := elm327.New(rw)
	if err := conn.Init(ctx); err != nil {
		rw.Close()
		return nil, fmt.Errorf("initialise adapter on %s: %w", desc, err)
	}
	return &Car{rw: rw, Conn: conn, Client: &obd.Client{R: conn, CAN: conn.Protocol.IsCAN()}, Desc: desc}, nil
}

// Close closes the adapter.
func (c *Car) Close() error { return c.rw.Close() }

// Protocol is the OBD protocol the adapter found.
func (c *Car) Protocol() string { return c.Conn.Protocol.String() }

// PIDSupport says whether the car supports one of the logged PIDs.
type PIDSupport struct {
	Name      string `json:"name"`
	PID       string `json:"pid"`
	Desc      string `json:"description"`
	Supported bool   `json:"supported"`
}

// Scan is a snapshot of the car's identity and diagnostic state.
type Scan struct {
	Adapter     string         `json:"adapter"`
	ELMVersion  string         `json:"elm_version"`
	Protocol    string         `json:"protocol"`
	VIN         string         `json:"vin,omitempty"`
	VINError    string         `json:"vin_error,omitempty"`
	LoggedPIDs  []PIDSupport   `json:"logged_pids"`
	AllPIDs     []string       `json:"all_supported_mode01_pids"`
	StoredDTCs  []string       `json:"stored_dtcs"`
	PendingDTCs []string       `json:"pending_dtcs"`
	DTCErrors   []string       `json:"dtc_errors,omitempty"`
	Readiness   *obd.Readiness `json:"readiness,omitempty"`
}

// Scan reads the VIN, supported PIDs, stored and pending DTCs and monitor
// readiness.
func (c *Car) Scan(ctx context.Context) (Scan, error) {
	s := Scan{Adapter: c.Desc, ELMVersion: c.Conn.Version, Protocol: c.Protocol(),
		StoredDTCs: []string{}, PendingDTCs: []string{}, AllPIDs: []string{}}
	if vin, err := c.Client.VIN(ctx); err != nil {
		s.VINError = err.Error()
	} else {
		s.VIN = vin
	}
	sup, err := c.Client.SupportedPIDs(ctx)
	if err != nil {
		return s, fmt.Errorf("supported PIDs: %w", err)
	}
	for _, p := range obd.Default {
		s.LoggedPIDs = append(s.LoggedPIDs, PIDSupport{Name: p.Name, PID: fmt.Sprintf("01%02X", p.Code), Desc: p.Desc, Supported: sup[p.Code]})
	}
	for code := 1; code < 0xE0; code++ {
		if sup[byte(code)] && code%0x20 != 0 {
			s.AllPIDs = append(s.AllPIDs, fmt.Sprintf("%02X", code))
		}
	}
	for _, pending := range []bool{false, true} {
		codes, err := c.Client.DTCs(ctx, pending)
		switch {
		case err != nil:
			s.DTCErrors = append(s.DTCErrors, fmt.Sprintf("%s: %v", dtcKind(pending), err))
		case pending:
			s.PendingDTCs = append(s.PendingDTCs, codes...)
		default:
			s.StoredDTCs = append(s.StoredDTCs, codes...)
		}
	}
	if r, err := c.Client.ReadReadiness(ctx); err == nil {
		s.Readiness = &r
	}
	return s, nil
}

// ClearDTCs clears stored trouble codes, freeze frames and readiness
// monitors (mode 04). It returns the stored codes read just before.
func (c *Car) ClearDTCs(ctx context.Context) ([]string, error) {
	codes, err := c.Client.DTCs(ctx, false)
	if err != nil {
		return nil, fmt.Errorf("read DTCs: %w", err)
	}
	return codes, c.Client.ClearDTCs(ctx)
}

func dtcKind(pending bool) string {
	if pending {
		return "pending"
	}
	return "stored"
}

// NormalizeDriveType lower-cases a drive type and joins words with '_', so
// "Cold start" and "cold_start" match.
func NormalizeDriveType(s string) string {
	return strings.Join(strings.Fields(strings.ToLower(strings.ReplaceAll(s, "-", " "))), "_")
}

// DriveTypes are the suggested drive types. Others are allowed.
var DriveTypes = map[string]string{
	"cold_start": "engine started cold (sat for hours); shows warm-up, cold idle and closed-loop entry",
	"commute":    "the regular commute; the best baseline because conditions repeat",
	"city":       "stop-and-go urban driving",
	"highway":    "sustained motorway speed; shows cruise fuel trims and charging under load",
	"mixed":      "a mix of city and open road",
	"short_trip": "a few minutes; the engine may never warm up",
	"idle_test":  "parked, engine idling; for idle stability, idle trims and warm-up",
	"diagnostic": "a drive to reproduce or investigate a specific symptom (describe it in notes)",
}
