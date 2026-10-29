// Package mcpserver exposes carsight to AI assistants over the Model Context
// Protocol. Read-only tools list vehicles and drives, return drive summaries,
// baseline comparisons, trends and Markdown reports, and give access to the
// raw samples. Car tools scan the car through the adapter, start, monitor and
// stop drive logging, label drives and clear trouble codes.
package mcpserver

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/jnszkr/carsight/internal/analyze"
	"github.com/jnszkr/carsight/internal/car"
	"github.com/jnszkr/carsight/internal/dashboard"
	"github.com/jnszkr/carsight/internal/obd"
	"github.com/jnszkr/carsight/internal/store"
)

const instructions = `carsight logs OBD-II data from the user's cars through a USB ELM327 adapter and holds what it logged: one session per drive, with a drive type, time-stamped samples of every standard PID the car supports, trouble codes (stored, pending, permanent), and a snapshot at the start and end of the drive (every value, monitor readiness, freeze frame, calibration IDs, monitor test results such as per-cylinder misfire counts).

Logging a drive: the adapter must be plugged into the car's OBD port and the laptop, with the ignition on. scan_car checks the connection and reads the VIN, supported PIDs, DTCs and readiness without logging. start_drive starts logging in the background and returns at once; ask the user what kind of drive it is (drive_type, see describe_pids) unless they said. start_drive returns a dashboard_url: a local web page charting the drive live; give it to the user (or open it in a browser). open_dashboard gives the page for a stored drive or the list of drives. drive_status shows live values while logging; stop_drive ends the drive and saves it, after which get_drive_summary analyses it. Logging also stops when the AI client exits. label_drive corrects a drive's type or notes afterwards. clear_dtcs erases trouble codes and readiness; only call it when the user explicitly asks.

Start with list_vehicles / list_sessions. get_drive_summary gives the compact per-drive analysis (statistics, warm-up, warm idle, fuel trims, rule-based events, DTCs, readiness, and z-scores against that car's earlier drives) and is usually all you need. get_trends shows how key metrics move across drives. Use get_samples when you need the raw time series (for example to look at a specific event); it downsamples, so ask for the PIDs and time window you care about. describe_pids lists PIDs, units, metrics and thresholds. The analyze_drive and analyze_trends prompts describe how to write up a diagnosis.`

// Config configures a Server.
type Config struct {
	Version string
	// Adapter is the default adapter for the car tools; a tool call may
	// name another port.
	Adapter car.Adapter
	// Dashboard is the address of the local dashboard ("" for
	// dashboard.DefaultAddr, "off" to disable it). It starts on first use.
	Dashboard string
}

// Server serves carsight over MCP.
type Server struct {
	MCP *mcp.Server

	db      *store.Store
	adapter car.Adapter
	// Defaults applied when a tool call doesn't specify them.
	BaselineDrives int
	MinDuration    time.Duration

	// base outlives tool calls: drives run under it until Close.
	base   context.Context
	cancel context.CancelFunc

	// carMu serialises use of the adapter; drive is the drive being
	// logged (or the last one).
	carMu sync.Mutex
	drive *car.Drive

	dash     *dashboard.Server
	dashAddr string
}

// New returns an MCP server for db. Call Close when the session ends to
// stop and save a drive in progress.
func New(db *store.Store, cfg Config) *Server {
	s := &Server{db: db, adapter: cfg.Adapter, BaselineDrives: 10, MinDuration: 5 * time.Minute, dashAddr: cfg.Dashboard}
	if cfg.Dashboard != "off" {
		s.dash = dashboard.New(db, func() *car.Drive {
			s.carMu.Lock()
			defer s.carMu.Unlock()
			return s.drive
		})
	}
	s.base, s.cancel = context.WithCancel(context.Background())
	srv := mcp.NewServer(&mcp.Implementation{Name: "carsight", Version: cfg.Version}, &mcp.ServerOptions{Instructions: instructions})
	s.MCP = srv

	ro := &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: new(bool)}
	add := func(name, title, desc string) *mcp.Tool {
		return &mcp.Tool{Name: name, Title: title, Description: desc, Annotations: ro}
	}
	mcp.AddTool(srv, add("list_vehicles", "List vehicles", "List the cars in the database (VIN, name, OBD protocol)."), s.listVehicles)
	mcp.AddTool(srv, add("list_sessions", "List drives",
		"List logged drives (sessions), newest first, optionally for one vehicle and a date range."), s.listSessions)
	mcp.AddTool(srv, add("get_drive_summary", "Drive summary",
		"Compact analysis of one drive: per-PID statistics, warm-up, warm idle, fuel trims, O2 activity, charging voltage, "+
			"rule-based events ranked by severity, DTCs, monitor readiness, key metrics, and each metric compared with the "+
			"car's earlier drives (z-scores; flagged at |z| >= 2 with 3+ baseline drives). Defaults to the latest drive."), s.driveSummary)
	mcp.AddTool(srv, add("get_drive_report", "Drive report",
		"The same analysis as get_drive_summary rendered as a human-readable Markdown report."), s.driveReport)
	mcp.AddTool(srv, add("get_trends", "Trends across drives",
		"Key metrics of each drive of one car in chronological order, with a least-squares slope per drive "+
			"(and per 30 days when the drives span 3+ days)."), s.trends)
	mcp.AddTool(srv, add("get_samples", "Raw samples",
		"Raw time series for one drive. Times are seconds from the start of the drive. Each PID is downsampled "+
			"evenly to at most max_points; narrow the window with from_s/to_s to see full resolution."), s.samples)
	mcp.AddTool(srv, add("open_dashboard", "Dashboard link",
		"Link to carsight's local web dashboard, which charts every PID of a drive (live while it is being logged, "+
			"with zoom and a synced crosshair for stored drives). Give the link to the user. Without session_id it links "+
			"the list of drives."), s.openDashboard)
	mcp.AddTool(srv, add("describe_pids", "Describe PIDs and metrics",
		"Every PID carsight can log (name, unit, description, polling rate: fast every cycle, slow every few cycles, extra "+
			"every ~10 s, once only in the start/end snapshot; a drive logs the ones the car supports), the key metrics used for baselines and trends, the event thresholds, "+
			"and the suggested drive types."), s.describe)

	// Car tools talk to the adapter.
	no := new(bool)
	mcp.AddTool(srv, &mcp.Tool{Name: "scan_car", Title: "Scan the car",
		Description: "Connect to the car through the adapter and read the OBD protocol, VIN, which PIDs it supports, " +
			"stored and pending DTCs and monitor readiness. Nothing is logged. Needs the adapter plugged in and the ignition on; " +
			"not available while a drive is being logged (use drive_status).",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}}, s.scanCar)
	mcp.AddTool(srv, &mcp.Tool{Name: "start_drive", Title: "Start logging a drive",
		Description: "Start logging a drive in the background and return immediately with the new session. Reads the VIN, " +
			"supported PIDs, DTCs and readiness, then polls the PIDs (several times a second) until stop_drive, duration_min, " +
			"a lost connection, or the AI client exiting. One drive at a time.",
		Annotations: &mcp.ToolAnnotations{DestructiveHint: no}}, s.startDrive)
	mcp.AddTool(srv, add("drive_status", "Drive status",
		"State of the drive being logged (or the last one this server logged): session, elapsed time, samples, polling rate, "+
			"read errors, the latest value of each PID and DTCs seen."), s.driveStatus)
	mcp.AddTool(srv, &mcp.Tool{Name: "stop_drive", Title: "Stop logging",
		Description: "Stop the drive being logged, save it and return its final status. Then use get_drive_summary with its session_id.",
		Annotations: &mcp.ToolAnnotations{DestructiveHint: no, IdempotentHint: true}}, s.stopDrive)
	mcp.AddTool(srv, &mcp.Tool{Name: "label_drive", Title: "Label a drive",
		Description: "Set a drive's type and notes, e.g. to correct them after the fact. Omitted fields keep their value.",
		Annotations: &mcp.ToolAnnotations{DestructiveHint: no, IdempotentHint: true, OpenWorldHint: no}}, s.labelDrive)
	mcp.AddTool(srv, &mcp.Tool{Name: "clear_dtcs", Title: "Clear trouble codes",
		Description: "Clear the car's stored DTCs, freeze frames and readiness monitors (OBD mode 04); also turns the MIL off. " +
			"The car may fail an emissions inspection until the monitors re-run, and the codes are lost as evidence. " +
			"Only call this when the user explicitly asks, with confirm=true. Engine off, ignition on.",
		Annotations: &mcp.ToolAnnotations{DestructiveHint: new(true)}}, s.clearDTCs)

	srv.AddPrompt(&mcp.Prompt{
		Name:        "analyze_drive",
		Title:       "Diagnose a drive",
		Description: "Write a health report for one drive from its summary, baseline comparison and, if needed, raw samples.",
		Arguments:   []*mcp.PromptArgument{{Name: "session_id", Description: "drive to analyse (default: latest)"}},
	}, func(_ context.Context, req *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
		target := "the latest drive"
		if id := req.Params.Arguments["session_id"]; id != "" {
			target = "drive " + id
		}
		return prompt("Diagnose a drive", fmt.Sprintf(drivePrompt, target)), nil
	})
	srv.AddPrompt(&mcp.Prompt{
		Name:        "analyze_trends",
		Title:       "Review trends",
		Description: "Review how a car's key metrics have changed across drives.",
		Arguments:   []*mcp.PromptArgument{{Name: "vin", Description: "vehicle (default: the only one, or ask)"}},
	}, func(_ context.Context, req *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
		target := "the car"
		if vin := req.Params.Arguments["vin"]; vin != "" {
			target = "vehicle " + vin
		}
		return prompt("Review trends", fmt.Sprintf(trendPrompt, target)), nil
	})
	return s
}

// Close stops and saves a drive in progress.
func (s *Server) Close() {
	s.cancel()
	s.carMu.Lock()
	d := s.drive
	s.carMu.Unlock()
	if d != nil {
		<-d.Done()
	}
	if s.dash != nil {
		s.dash.Close()
	}
}

// dashboardURL starts the dashboard if needed and returns the link to path,
// or "" if the dashboard is off or can't listen.
func (s *Server) dashboardURL(path string) (string, error) {
	if s.dash == nil {
		return "", errors.New("the dashboard is disabled (carsight mcp -dashboard off)")
	}
	base, err := s.dash.Start(s.dashAddr)
	if err != nil {
		return "", fmt.Errorf("start dashboard: %w", err)
	}
	return base + path, nil
}

type openDashboardIn struct {
	SessionID int64 `json:"session_id,omitempty" jsonschema:"drive to show; omit for the list of drives"`
}

func (s *Server) openDashboard(ctx context.Context, _ *mcp.CallToolRequest, in openDashboardIn) (*mcp.CallToolResult, any, error) {
	path := "/"
	if in.SessionID != 0 {
		if _, err := s.session(ctx, in.SessionID, ""); err != nil {
			return nil, nil, err
		}
		path = fmt.Sprintf("/drive/%d", in.SessionID)
	}
	u, err := s.dashboardURL(path)
	if err != nil {
		return nil, nil, err
	}
	return nil, map[string]string{"dashboard_url": u}, nil
}

func prompt(desc, text string) *mcp.GetPromptResult {
	return &mcp.GetPromptResult{Description: desc, Messages: []*mcp.PromptMessage{{Role: "user", Content: &mcp.TextContent{Text: text}}}}
}

type listVehiclesOut struct {
	Vehicles []store.Vehicle `json:"vehicles"`
}

func (s *Server) listVehicles(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
	vs, err := s.db.Vehicles(ctx)
	if vs == nil {
		vs = []store.Vehicle{}
	}
	return nil, listVehiclesOut{vs}, err
}

type listSessionsIn struct {
	VIN       string `json:"vin,omitempty" jsonschema:"only this vehicle"`
	Since     string `json:"since,omitempty" jsonschema:"first day to include, YYYY-MM-DD"`
	Until     string `json:"until,omitempty" jsonschema:"last day to include, YYYY-MM-DD"`
	Limit     int    `json:"limit,omitempty" jsonschema:"maximum number of sessions (default 50)"`
	DriveType string `json:"drive_type,omitempty" jsonschema:"only drives of this type, e.g. commute"`
}

type sessionInfo struct {
	ID        int64     `json:"id"`
	VIN       string    `json:"vin"`
	StartedAt time.Time `json:"started_at"`
	Duration  float64   `json:"duration_s"` // 0 while still logging
	Running   bool      `json:"still_logging,omitempty"`
	Protocol  string    `json:"protocol,omitempty"`
	DriveType string    `json:"drive_type,omitempty"`
	Notes     string    `json:"notes,omitempty"`
	Software  string    `json:"logged_by,omitempty"` // carsight version that logged it
	Adapter   string    `json:"adapter,omitempty"`
}

type listSessionsOut struct {
	Sessions []sessionInfo `json:"sessions"`
	More     bool          `json:"more"`
}

func (s *Server) listSessions(ctx context.Context, _ *mcp.CallToolRequest, in listSessionsIn) (*mcp.CallToolResult, any, error) {
	since, until, err := dayRange(in.Since, in.Until)
	if err != nil {
		return nil, nil, err
	}
	ss, err := s.db.Sessions(ctx, in.VIN, since, until)
	if err != nil {
		return nil, nil, err
	}
	limit := in.Limit
	if limit <= 0 {
		limit = 50
	}
	out := listSessionsOut{Sessions: []sessionInfo{}}
	dt := car.NormalizeDriveType(in.DriveType)
	for _, ss := range ss {
		if dt != "" && ss.DriveType != dt {
			continue
		}
		if len(out.Sessions) == limit {
			out.More = true
			break
		}
		info := sessionInfo{ID: ss.ID, VIN: ss.VIN, StartedAt: ss.StartedAt, Protocol: ss.Protocol,
			DriveType: ss.DriveType, Notes: ss.Notes, Software: ss.Software, Adapter: ss.Adapter, Running: ss.EndedAt.IsZero()}
		if !ss.EndedAt.IsZero() {
			info.Duration = math.Round(ss.EndedAt.Sub(ss.StartedAt).Seconds())
		}
		out.Sessions = append(out.Sessions, info)
	}
	return nil, out, nil
}

type driveIn struct {
	SessionID      int64  `json:"session_id,omitempty" jsonschema:"session to analyse (default: latest)"`
	VIN            string `json:"vin,omitempty" jsonschema:"with no session_id: latest drive of this vehicle"`
	BaselineDrives int    `json:"baseline_drives,omitempty" jsonschema:"earlier drives to compare against (default 10)"`
}

func (s *Server) session(ctx context.Context, id int64, vin string) (store.Session, error) {
	if id != 0 {
		ss, err := s.db.Session(ctx, id)
		if errors.Is(err, store.ErrNotFound) {
			return ss, fmt.Errorf("no session %d", id)
		}
		return ss, err
	}
	ss, err := analyze.LatestSession(ctx, s.db, vin)
	if errors.Is(err, store.ErrNotFound) {
		return ss, errors.New("no sessions logged yet")
	}
	return ss, err
}

func (s *Server) analysis(ctx context.Context, in driveIn) (analyze.DriveAnalysis, error) {
	sess, err := s.session(ctx, in.SessionID, in.VIN)
	if err != nil {
		return analyze.DriveAnalysis{}, err
	}
	n := in.BaselineDrives
	if n <= 0 {
		n = s.BaselineDrives
	}
	return analyze.AnalyzeDrive(ctx, s.db, sess, n, s.MinDuration)
}

func (s *Server) driveSummary(ctx context.Context, _ *mcp.CallToolRequest, in driveIn) (*mcp.CallToolResult, any, error) {
	a, err := s.analysis(ctx, in)
	if err != nil {
		return nil, nil, err
	}
	return nil, a, nil
}

func (s *Server) driveReport(ctx context.Context, _ *mcp.CallToolRequest, in driveIn) (*mcp.CallToolResult, any, error) {
	a, err := s.analysis(ctx, in)
	if err != nil {
		return nil, nil, err
	}
	var buf bytes.Buffer
	if err := analyze.WriteReport(&buf, a.Drive, a.Baseline); err != nil {
		return nil, nil, err
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: buf.String()}}}, nil, nil
}

type trendsIn struct {
	VIN         string  `json:"vin,omitempty" jsonschema:"vehicle (required when there is more than one)"`
	Since       string  `json:"since,omitempty" jsonschema:"first day to include, YYYY-MM-DD"`
	Until       string  `json:"until,omitempty" jsonschema:"last day to include, YYYY-MM-DD"`
	MinDuration float64 `json:"min_duration_min,omitempty" jsonschema:"ignore drives shorter than this many minutes (default 5)"`
	DriveType   string  `json:"drive_type,omitempty" jsonschema:"only drives of this type, e.g. commute, so like is compared with like"`
}

func (s *Server) trends(ctx context.Context, _ *mcp.CallToolRequest, in trendsIn) (*mcp.CallToolResult, any, error) {
	vin := in.VIN
	if vin == "" {
		vs, err := s.db.Vehicles(ctx)
		if err != nil {
			return nil, nil, err
		}
		if len(vs) != 1 {
			return nil, nil, fmt.Errorf("%d vehicles in the database; pass vin (see list_vehicles)", len(vs))
		}
		vin = vs[0].VIN
	}
	since, until, err := dayRange(in.Since, in.Until)
	if err != nil {
		return nil, nil, err
	}
	minDur := s.MinDuration
	if in.MinDuration > 0 {
		minDur = time.Duration(in.MinDuration * float64(time.Minute))
	}
	t, err := analyze.AnalyzeTrends(ctx, s.db, analyze.TrendFilter{VIN: vin, Since: since, Until: until,
		MinDuration: minDur, DriveType: car.NormalizeDriveType(in.DriveType)})
	if err != nil {
		return nil, nil, err
	}
	return nil, t, nil
}

const (
	defaultMaxPoints = 1000
	maxMaxPoints     = 20000
)

type samplesIn struct {
	SessionID int64    `json:"session_id" jsonschema:"session to read"`
	PIDs      []string `json:"pids,omitempty" jsonschema:"PID names, e.g. rpm, coolant_temp (default: all; see describe_pids)"`
	From      *float64 `json:"from_s,omitempty" jsonschema:"start of window, seconds from start of drive"`
	To        *float64 `json:"to_s,omitempty" jsonschema:"end of window, seconds from start of drive"`
	MaxPoints int      `json:"max_points,omitempty" jsonschema:"maximum points per PID (default 1000, max 20000)"`
}

type pidSeries struct {
	Unit      string    `json:"unit,omitempty"`
	Total     int       `json:"total_in_window"`
	Returned  int       `json:"returned"`
	T         []float64 `json:"t_s"`
	V         []float64 `json:"v"`
	Truncated bool      `json:"downsampled,omitempty"`
}

type samplesOut struct {
	SessionID int64                 `json:"session_id"`
	Start     time.Time             `json:"start"`
	Series    map[string]*pidSeries `json:"series"`
}

func (s *Server) samples(ctx context.Context, _ *mcp.CallToolRequest, in samplesIn) (*mcp.CallToolResult, any, error) {
	sess, err := s.session(ctx, in.SessionID, "")
	if err != nil {
		return nil, nil, err
	}
	all, err := s.db.Samples(ctx, sess.ID)
	if err != nil {
		return nil, nil, err
	}
	maxPts := in.MaxPoints
	if maxPts <= 0 {
		maxPts = defaultMaxPoints
	}
	maxPts = min(maxPts, maxMaxPoints)
	want := map[string]bool{}
	for _, p := range in.PIDs {
		want[p] = true
	}

	// Collect the window per PID, then thin each evenly.
	type pt struct{ t, v float64 }
	byPID := map[string][]pt{}
	for _, sm := range all {
		if len(want) > 0 && !want[sm.PID] {
			continue
		}
		t := sm.T.Sub(sess.StartedAt).Seconds()
		if in.From != nil && t < *in.From || in.To != nil && t > *in.To {
			continue
		}
		byPID[sm.PID] = append(byPID[sm.PID], pt{math.Round(t*1000) / 1000, sm.Value})
	}
	out := samplesOut{SessionID: sess.ID, Start: sess.StartedAt, Series: map[string]*pidSeries{}}
	for name := range want {
		if _, ok := byPID[name]; !ok {
			if _, known := obd.ByName(name); !known {
				return nil, nil, fmt.Errorf("unknown PID %q; see describe_pids", name)
			}
		}
	}
	for name, pts := range byPID {
		ser := &pidSeries{Total: len(pts), T: []float64{}, V: []float64{}}
		if p, ok := obd.ByName(name); ok {
			ser.Unit = p.Unit
		}
		step := 1.0
		if len(pts) > maxPts {
			step = float64(len(pts)) / float64(maxPts)
			ser.Truncated = true
		}
		for f := 0.0; int(f) < len(pts); f += step {
			p := pts[int(f)]
			ser.T = append(ser.T, p.t)
			ser.V = append(ser.V, p.v)
		}
		ser.Returned = len(ser.T)
		out.Series[name] = ser
	}
	return nil, out, nil
}

type pidInfo struct {
	Name string   `json:"name"`
	PID  string   `json:"pid"`
	Unit string   `json:"unit"`
	Desc string   `json:"description"`
	Rate obd.Rate `json:"rate"`
	// Labels names the values of enumerated PIDs.
	Labels map[int]string `json:"labels,omitempty"`
}

type describeOut struct {
	PIDs       []pidInfo          `json:"pids"`
	Metrics    map[string]string  `json:"metrics"`
	Thresholds map[string]float64 `json:"thresholds"`
	DriveTypes map[string]string  `json:"drive_types"`
}

func (s *Server) describe(_ context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
	out := describeOut{Metrics: analyze.MetricDescriptions, DriveTypes: car.DriveTypes, Thresholds: map[string]float64{
		"warm_coolant_c":            analyze.WarmC,
		"overheat_coolant_c":        analyze.OverheatC,
		"low_voltage_running_v":     analyze.LowVoltageRunning,
		"high_voltage_v":            analyze.HighVoltage,
		"fuel_trim_warn_pct":        analyze.TrimWarnPct,
		"fuel_trim_severe_pct":      analyze.TrimSeverePct,
		"idle_unstable_std_rpm":     analyze.IdleUnstableStdRPM,
		"max_idle_rpm":              analyze.MaxIdleRPM,
		"slow_warmup_min":           analyze.SlowWarmup.Minutes(),
		"baseline_flag_abs_z":       2,
		"baseline_min_drives":       analyze.MinBaseline,
		"default_min_drive_minutes": s.MinDuration.Minutes(),
	}}
	for _, p := range obd.All {
		out.PIDs = append(out.PIDs, pidInfo{Name: p.Name, PID: fmt.Sprintf("01%02X", p.Code), Unit: p.Unit, Desc: p.Desc, Rate: p.Rate, Labels: p.Labels})
	}
	return nil, out, nil
}

func dayRange(since, until string) (time.Time, time.Time, error) {
	var s, u time.Time
	var err error
	if since != "" {
		if s, err = time.ParseInLocation(time.DateOnly, since, time.Local); err != nil {
			return s, u, fmt.Errorf("since: %w", err)
		}
	}
	if until != "" {
		if u, err = time.ParseInLocation(time.DateOnly, until, time.Local); err != nil {
			return s, u, fmt.Errorf("until: %w", err)
		}
		u = u.AddDate(0, 0, 1)
	}
	return s, u, nil
}
