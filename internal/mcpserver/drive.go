package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/jnszkr/carsight/internal/car"
	"github.com/jnszkr/carsight/internal/store"
)

type adapterIn struct {
	Port     string `json:"port,omitempty" jsonschema:"adapter serial device; default: the server's (usually auto-detected). 'sim:volvo' or 'sim:citroen' use the simulator"`
	Protocol int    `json:"protocol,omitempty" jsonschema:"OBD protocol as an ELM327 ATSP number, to skip the protocol search: 3 = ISO 9141-2, 4 = KWP2000 5-baud init, 5 = KWP2000 fast init, 6-9 = CAN (6 = 11 bit 500 kbaud). Default: the server's (usually 0 = search). Older European petrol and diesel cars (roughly pre-2008) are often 3, 4 or 5"`
}

func (s *Server) adapterFor(in adapterIn) car.Adapter {
	a := s.adapter
	if in.Port != "" {
		a.Port = in.Port
		a.Baud = 0
	}
	if in.Protocol != 0 {
		a.Protocol = in.Protocol
	}
	return a
}

// running reports the drive being logged, if any. carMu must be held.
func (s *Server) running() *car.Drive {
	if s.drive == nil {
		return nil
	}
	select {
	case <-s.drive.Done():
		return nil
	default:
		return s.drive
	}
}

var errDriving = errors.New("a drive is being logged and has the adapter; use drive_status, or stop_drive first")

func (s *Server) scanCar(ctx context.Context, _ *mcp.CallToolRequest, in adapterIn) (*mcp.CallToolResult, any, error) {
	s.carMu.Lock()
	defer s.carMu.Unlock()
	if s.running() != nil {
		return nil, nil, errDriving
	}
	c, err := car.Connect(ctx, s.adapterFor(in))
	if err != nil {
		return nil, nil, err
	}
	defer c.Close()
	sc, err := c.Scan(ctx)
	if err != nil {
		return nil, nil, err
	}
	return nil, sc, nil
}

type startDriveIn struct {
	adapterIn
	DriveType   string  `json:"drive_type,omitempty" jsonschema:"kind of drive: cold_start, commute, city, highway, mixed, short_trip, idle_test, diagnostic, or your own"`
	Notes       string  `json:"notes,omitempty" jsonschema:"free-form notes, e.g. the symptom being investigated, weather, load"`
	VehicleName string  `json:"vehicle_name,omitempty" jsonschema:"friendly name for the car, e.g. Volvo V70 (kept for later drives)"`
	DurationMin float64 `json:"duration_min,omitempty" jsonschema:"stop automatically after this many minutes (default: run until stop_drive)"`
}

func (s *Server) startDrive(ctx context.Context, _ *mcp.CallToolRequest, in startDriveIn) (*mcp.CallToolResult, any, error) {
	s.carMu.Lock()
	defer s.carMu.Unlock()
	if d := s.running(); d != nil {
		return nil, nil, fmt.Errorf("drive %d is already being logged; stop_drive first", d.Status().SessionID)
	}
	c, err := car.Connect(ctx, s.adapterFor(in.adapterIn))
	if err != nil {
		return nil, nil, err
	}
	// The drive runs under the server's context, not this call's.
	d, err := car.StartDrive(s.base, c, s.db, car.DriveOptions{
		Name: in.VehicleName, DriveType: in.DriveType, Notes: in.Notes,
		Duration: time.Duration(in.DurationMin * float64(time.Minute)),
	})
	if err != nil {
		return nil, nil, err
	}
	s.drive = d
	return nil, s.statusOut(d.Status(), ""), nil
}

type driveStatusOut struct {
	*car.Status
	DashboardURL string `json:"dashboard_url,omitempty"`
	Message      string `json:"message,omitempty"`
}

func (s *Server) statusOut(st car.Status, msg string) driveStatusOut {
	out := driveStatusOut{Status: &st, Message: msg}
	if u, err := s.dashboardURL(fmt.Sprintf("/drive/%d", st.SessionID)); err == nil {
		out.DashboardURL = u
	}
	return out
}

func (s *Server) driveStatus(_ context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
	s.carMu.Lock()
	d := s.drive
	s.carMu.Unlock()
	if d == nil {
		return nil, driveStatusOut{Message: "no drive has been logged since this server started; use start_drive"}, nil
	}
	return nil, s.statusOut(d.Status(), ""), nil
}

func (s *Server) stopDrive(_ context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
	s.carMu.Lock()
	defer s.carMu.Unlock()
	d := s.running()
	if d == nil {
		if s.drive != nil {
			return nil, s.statusOut(s.drive.Status(), "this drive had already stopped"), nil
		}
		return nil, nil, errors.New("no drive is being logged")
	}
	return nil, s.statusOut(d.Stop(), ""), nil
}

type labelDriveIn struct {
	SessionID int64   `json:"session_id" jsonschema:"drive to label"`
	DriveType *string `json:"drive_type,omitempty" jsonschema:"new drive type (empty string clears it)"`
	Notes     *string `json:"notes,omitempty" jsonschema:"new notes (replaces the old ones; empty string clears them)"`
}

func (s *Server) labelDrive(ctx context.Context, _ *mcp.CallToolRequest, in labelDriveIn) (*mcp.CallToolResult, any, error) {
	sess, err := s.db.Session(ctx, in.SessionID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil, fmt.Errorf("no session %d", in.SessionID)
	}
	if err != nil {
		return nil, nil, err
	}
	if in.DriveType != nil {
		sess.DriveType = car.NormalizeDriveType(*in.DriveType)
	}
	if in.Notes != nil {
		sess.Notes = *in.Notes
	}
	if err := s.db.Describe(ctx, sess.ID, sess.DriveType, sess.Notes); err != nil {
		return nil, nil, err
	}
	return nil, map[string]any{"session_id": sess.ID, "drive_type": sess.DriveType, "notes": sess.Notes}, nil
}

type clearDTCsIn struct {
	adapterIn
	Confirm bool `json:"confirm" jsonschema:"must be true; set it only after the user explicitly asked to clear the codes"`
}

func (s *Server) clearDTCs(ctx context.Context, _ *mcp.CallToolRequest, in clearDTCsIn) (*mcp.CallToolResult, any, error) {
	if !in.Confirm {
		return nil, nil, errors.New("not cleared: confirm must be true, and only when the user explicitly asked")
	}
	s.carMu.Lock()
	defer s.carMu.Unlock()
	if s.running() != nil {
		return nil, nil, errDriving
	}
	c, err := car.Connect(ctx, s.adapterFor(in.adapterIn))
	if err != nil {
		return nil, nil, err
	}
	defer c.Close()
	codes, err := c.ClearDTCs(ctx)
	if err != nil {
		return nil, nil, err
	}
	if codes == nil {
		codes = []string{}
	}
	return nil, map[string]any{"cleared": true, "stored_dtcs_before": codes,
		"note": "readiness monitors are reset and need a few drive cycles to complete"}, nil
}
