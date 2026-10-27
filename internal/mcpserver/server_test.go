package mcpserver

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/jnszkr/carsight/internal/car"
	"github.com/jnszkr/carsight/internal/obd"
	"github.com/jnszkr/carsight/internal/store"
)

// seed stores n 10-minute drives, one per day, sampled at 2 Hz.
func seed(t *testing.T, n int) *store.Store {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.UpsertVehicle(ctx, store.Vehicle{VIN: "VIN1", Name: "Volvo V70", Protocol: "CAN"}); err != nil {
		t.Fatal(err)
	}
	t0 := time.Unix(1_700_000_000, 0)
	for d := range n {
		start := t0.Add(time.Duration(d) * 24 * time.Hour)
		// LTFT creeps up slowly, then the last drive runs clearly lean.
		ltft := float64(d) / 2
		if d == n-1 && n > 1 {
			ltft = 6
		}
		id, err := db.StartSession(ctx, "VIN1", "CAN", start)
		if err != nil {
			t.Fatal(err)
		}
		var samples []store.Sample
		for i := range 10 * 60 * 2 {
			ts := start.Add(time.Duration(i) * 500 * time.Millisecond)
			sec := float64(i) / 2
			add := func(p obd.PID, v float64) { samples = append(samples, store.Sample{T: ts, PID: p.Name, Value: v}) }
			add(obd.RPM, 750+5*math.Sin(sec))
			add(obd.Speed, 0)
			add(obd.CoolantTemp, 90-80*math.Exp(-sec/100))
			add(obd.STFTBank1, math.Sin(sec))
			add(obd.LTFTBank1, ltft)
			add(obd.ModuleVoltage, 14.1)
		}
		if err := db.AddSamples(ctx, id, samples); err != nil {
			t.Fatal(err)
		}
		if err := db.AddDTCs(ctx, id, "stored", []string{"P0171"}, start); err != nil {
			t.Fatal(err)
		}
		if err := db.EndSession(ctx, id, start.Add(10*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	return db
}

func connect(t *testing.T, db *store.Store) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()
	ct, st := mcp.NewInMemoryTransports()
	srv := New(db, Config{Version: "test", Adapter: car.Adapter{Port: "sim:volvo", SimSpeedup: 30}, Dashboard: "127.0.0.1:0"})
	t.Cleanup(srv.Close)
	if _, err := srv.MCP.Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

func call(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any, out any) string {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	text := res.Content[0].(*mcp.TextContent).Text
	if res.IsError {
		t.Fatalf("%s: tool error: %s", name, text)
	}
	if out != nil {
		if err := json.Unmarshal([]byte(text), out); err != nil {
			t.Fatalf("%s: decode %q: %v", name, text, err)
		}
	}
	return text
}

func TestTools(t *testing.T) {
	cs := connect(t, seed(t, 5))
	ctx := context.Background()

	tools, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	writes := map[string]bool{"start_drive": true, "stop_drive": true, "label_drive": true, "clear_dtcs": true}
	for _, tool := range tools.Tools {
		names = append(names, tool.Name)
		if ro := tool.Annotations != nil && tool.Annotations.ReadOnlyHint; ro == writes[tool.Name] {
			t.Errorf("%s: read-only %v", tool.Name, ro)
		}
	}
	if len(names) != 14 {
		t.Errorf("tools: %v", names)
	}
	clear := tools.Tools[slices.IndexFunc(tools.Tools, func(t *mcp.Tool) bool { return t.Name == "clear_dtcs" })]
	if d := clear.Annotations.DestructiveHint; d == nil || !*d {
		t.Error("clear_dtcs is not marked destructive")
	}

	var vs struct{ Vehicles []store.Vehicle }
	call(t, cs, "list_vehicles", nil, &vs)
	if len(vs.Vehicles) != 1 || vs.Vehicles[0].Name != "Volvo V70" {
		t.Errorf("vehicles: %+v", vs)
	}

	var ss listSessionsOut
	call(t, cs, "list_sessions", map[string]any{"limit": 3}, &ss)
	if len(ss.Sessions) != 3 || !ss.More || ss.Sessions[0].ID != 5 || ss.Sessions[0].Duration != 600 {
		t.Errorf("sessions: %+v", ss)
	}

	var drive struct {
		Drive struct {
			SessionID int64              `json:"session_id"`
			Metrics   map[string]float64 `json:"metrics"`
			DTCs      []struct{ Code string }
		}
		BaselineSessions []int64 `json:"baseline_sessions"`
		Baseline         []struct {
			Metric string
			Flag   bool
		} `json:"baseline_deltas"`
	}
	call(t, cs, "get_drive_summary", nil, &drive)
	if drive.Drive.SessionID != 5 || len(drive.BaselineSessions) != 4 || len(drive.Drive.DTCs) != 1 {
		t.Errorf("drive summary: %+v", drive)
	}
	flagged := false
	for _, d := range drive.Baseline {
		if d.Metric == "ltft_warm_mean" && d.Flag {
			flagged = true
		}
	}
	if !flagged {
		t.Errorf("LTFT drift not flagged: %+v", drive.Baseline)
	}

	report := call(t, cs, "get_drive_report", map[string]any{"session_id": 2}, nil)
	if !strings.HasPrefix(report, "# Drive 2: Volvo V70") {
		t.Errorf("report: %.80s", report)
	}

	var tr struct {
		Drives []struct{ SessionID int64 } `json:"drives"`
		Trends []struct {
			Metric        string
			SlopePerDrive float64 `json:"slope_per_drive"`
		}
	}
	call(t, cs, "get_trends", nil, &tr)
	if len(tr.Drives) != 5 {
		t.Errorf("trend drives: %+v", tr.Drives)
	}
	for _, l := range tr.Trends {
		if l.Metric == "ltft_warm_mean" && l.SlopePerDrive <= 0.5 {
			t.Errorf("ltft slope %v, want > 0.5", l.SlopePerDrive)
		}
	}

	var sm samplesOut
	call(t, cs, "get_samples", map[string]any{"session_id": 1, "pids": []string{"rpm", "coolant_temp"}, "max_points": 100}, &sm)
	rpm := sm.Series["rpm"]
	if len(sm.Series) != 2 || rpm == nil || rpm.Total != 1200 || rpm.Returned != 100 || !rpm.Truncated || rpm.Unit != "rpm" {
		t.Errorf("samples: %+v", rpm)
	}
	call(t, cs, "get_samples", map[string]any{"session_id": 1, "pids": []string{"rpm"}, "from_s": 10, "to_s": 20}, &sm)
	if r := sm.Series["rpm"]; r.Total != 21 || r.Truncated || r.T[0] != 10 || r.T[len(r.T)-1] != 20 {
		t.Errorf("windowed samples: %+v", r)
	}

	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "get_samples", Arguments: map[string]any{"session_id": 1, "pids": []string{"bogus"}}})
	if err != nil || !res.IsError {
		t.Errorf("unknown PID should be a tool error: %v %+v", err, res)
	}

	var desc describeOut
	call(t, cs, "describe_pids", nil, &desc)
	if len(desc.PIDs) != len(obd.All) || desc.Metrics["ltft_warm_mean"] == "" {
		t.Errorf("describe: %+v", desc)
	}
}

func TestPrompts(t *testing.T) {
	cs := connect(t, seed(t, 1))
	ctx := context.Background()
	res, err := cs.GetPrompt(ctx, &mcp.GetPromptParams{Name: "analyze_drive", Arguments: map[string]string{"session_id": "7"}})
	if err != nil {
		t.Fatal(err)
	}
	text := res.Messages[0].Content.(*mcp.TextContent).Text
	if !strings.Contains(text, "drive 7") || !strings.Contains(text, "get_drive_summary") || strings.Contains(text, "%!") {
		t.Errorf("prompt: %s", text)
	}
	if _, err := cs.GetPrompt(ctx, &mcp.GetPromptParams{Name: "analyze_trends"}); err != nil {
		t.Fatal(err)
	}
}

func callErr(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) string {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if !res.IsError {
		t.Fatalf("%s: expected a tool error", name)
	}
	return res.Content[0].(*mcp.TextContent).Text
}

func TestDriveTools(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	cs := connect(t, db)

	var sc car.Scan
	call(t, cs, "scan_car", nil, &sc)
	if sc.VIN == "" || len(sc.LoggedPIDs) == 0 || sc.Readiness == nil {
		t.Errorf("scan: %+v", sc)
	}
	if callErr(t, cs, "stop_drive", nil) == "" {
		t.Error("stop_drive with nothing running")
	}

	var st car.Status
	call(t, cs, "start_drive", map[string]any{"drive_type": "Cold start", "notes": "test", "vehicle_name": "Volvo V70"}, &st)
	if st.SessionID == 0 || !st.Running || st.DriveType != "cold_start" {
		t.Fatalf("start: %+v", st)
	}
	if msg := callErr(t, cs, "scan_car", nil); !strings.Contains(msg, "drive is being logged") {
		t.Errorf("scan while driving: %s", msg)
	}
	callErr(t, cs, "start_drive", nil)
	callErr(t, cs, "clear_dtcs", map[string]any{"confirm": true})

	deadline := time.Now().Add(10 * time.Second)
	for {
		call(t, cs, "drive_status", nil, &st)
		if st.Latest["rpm"] > 0 && st.Samples > 50 || time.Now().After(deadline) {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if st.Samples <= 50 || st.Latest["rpm"] == 0 {
		t.Fatalf("status: %+v", st)
	}
	checkDashboard(t, cs, st.SessionID)

	call(t, cs, "stop_drive", nil, &st)
	if st.Running || st.StopReason != "stopped" {
		t.Errorf("stop: %+v", st)
	}
	var drive struct {
		Drive struct {
			SessionID   int64  `json:"session_id"`
			DriveType   string `json:"drive_type"`
			Vehicle     string `json:"vehicle"`
			SampleCount int    `json:"sample_count"`
			Snapshots   *obd.SessionInfo
		}
	}
	call(t, cs, "get_drive_summary", nil, &drive)
	if sn := drive.Drive.Snapshots; sn == nil || sn.Start == nil || sn.End == nil || sn.Start.VehicleInfo == nil ||
		sn.Start.Labels["fuel_type"] != "gasoline" || sn.End.Values["coolant_temp"] == 0 {
		t.Errorf("snapshots: %+v", sn)
	}
	if drive.Drive.SessionID != st.SessionID || drive.Drive.DriveType != "cold_start" || drive.Drive.Vehicle != "Volvo V70" {
		t.Errorf("summary: %+v", drive)
	}

	call(t, cs, "label_drive", map[string]any{"session_id": st.SessionID, "drive_type": "idle test"}, nil)
	var ss listSessionsOut
	call(t, cs, "list_sessions", map[string]any{"drive_type": "idle_test"}, &ss)
	if len(ss.Sessions) != 1 || ss.Sessions[0].Notes != "test" || ss.Sessions[0].Running {
		t.Errorf("sessions: %+v", ss)
	}
	call(t, cs, "list_sessions", map[string]any{"drive_type": "highway"}, &ss)
	if len(ss.Sessions) != 0 {
		t.Errorf("highway sessions: %+v", ss)
	}

	// A timed drive stops by itself.
	call(t, cs, "start_drive", map[string]any{"duration_min": 0.01}, &st)
	// It then takes the end snapshot before it is saved.
	for deadline := time.Now().Add(20 * time.Second); st.Running && time.Now().Before(deadline); {
		time.Sleep(200 * time.Millisecond)
		call(t, cs, "drive_status", nil, &st)
	}
	if st.Running || st.StopReason != "duration reached" {
		t.Errorf("timed drive: %+v", st)
	}

	callErr(t, cs, "clear_dtcs", nil)
	call(t, cs, "clear_dtcs", map[string]any{"confirm": true}, nil)
}

// checkDashboard follows the live drive's dashboard link: the page, the
// stored samples so far, and live readings over server-sent events.
func checkDashboard(t *testing.T, cs *mcp.ClientSession, id int64) {
	t.Helper()
	var out driveStatusOut
	call(t, cs, "drive_status", nil, &out)
	want := fmt.Sprintf("/drive/%d", id)
	if !strings.HasPrefix(out.DashboardURL, "http://127.0.0.1:") || !strings.HasSuffix(out.DashboardURL, want) {
		t.Fatalf("dashboard_url %q", out.DashboardURL)
	}
	base := strings.TrimSuffix(out.DashboardURL, want)
	get := func(path string) *http.Response {
		t.Helper()
		resp, err := http.Get(base + path)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s: %s", path, resp.Status)
		}
		return resp
	}
	resp := get(want)
	page, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(page), "EventSource") {
		t.Error("page doesn't look like the dashboard")
	}

	var drive struct {
		Session struct{ Live bool }
		Series  map[string]struct{ T, V []float64 }
	}
	resp = get(fmt.Sprintf("/api/drives/%d", id))
	err := json.NewDecoder(resp.Body).Decode(&drive)
	resp.Body.Close()
	// Samples reach the store every 2 s, so there may be none yet.
	if _, ok := drive.Series["rpm"]; err != nil || !drive.Session.Live || !ok {
		t.Errorf("drive: %+v %v", drive, err)
	}

	resp = get(fmt.Sprintf("/api/drives/%d/live", id))
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		if sc.Text() == "event: samples" && sc.Scan() {
			var rows [][3]any
			if err := json.Unmarshal([]byte(strings.TrimPrefix(sc.Text(), "data: ")), &rows); err != nil || len(rows) == 0 {
				t.Errorf("samples event %q: %v", sc.Text(), err)
			}
			return
		}
	}
	t.Errorf("no samples event: %v", sc.Err())
}

func TestCloseSavesDrive(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	srv := New(db, Config{Adapter: car.Adapter{Port: "sim:citroen"}, Dashboard: "127.0.0.1:0"})
	ct, st := mcp.NewInMemoryTransports()
	ctx := context.Background()
	if _, err := srv.MCP.Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	call(t, cs, "start_drive", nil, nil)
	var status car.Status
	for deadline := time.Now().Add(10 * time.Second); status.Samples == 0 && time.Now().Before(deadline); {
		time.Sleep(100 * time.Millisecond)
		call(t, cs, "drive_status", nil, &status)
	}
	cs.Close()
	srv.Close()
	ss, err := db.Sessions(ctx, "", time.Time{}, time.Time{})
	if err != nil || len(ss) != 1 || ss[0].EndedAt.IsZero() {
		t.Fatalf("sessions: %+v %v", ss, err)
	}
	if n, _ := db.Samples(ctx, ss[0].ID); len(n) == 0 {
		t.Error("no samples saved")
	}
}
