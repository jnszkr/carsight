package dashboard

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/jnszkr/carsight/internal/obd"
	"github.com/jnszkr/carsight/internal/store"
)

func TestThinKeepsSpikes(t *testing.T) {
	n := 10000
	ts, vs := make([]float64, n), make([]float64, n)
	for i := range n {
		ts[i] = float64(i)
	}
	vs[4321] = 99
	ot, ov := thin(ts, vs, 100)
	if len(ot) > 100 || len(ot) != len(ov) {
		t.Fatalf("%d points", len(ot))
	}
	found := false
	for i, v := range ov {
		if v == 99 && ot[i] == 4321 {
			found = true
		}
		if i > 0 && ot[i] <= ot[i-1] {
			t.Fatalf("times not increasing at %d", i)
		}
	}
	if !found {
		t.Error("spike lost")
	}
}

func TestLocalOnly(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	h := New(db, nil).Handler()
	for host, want := range map[string]int{
		"127.0.0.1:7341": http.StatusOK, "localhost:7341": http.StatusOK, "[::1]:7341": http.StatusOK,
		"evil.example:7341": http.StatusForbidden, "192.168.1.5:7341": http.StatusForbidden,
	} {
		r := httptest.NewRequest("GET", "/api/sessions", nil)
		r.Host = host
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != want {
			t.Errorf("%s: %d, want %d", host, w.Code, want)
		}
	}
}

// oneDrive stores a minute of warm idle with a stored DTC, logged by v9.9.9.
func oneDrive(t *testing.T) (*store.Store, int64) {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.UpsertVehicle(ctx, store.Vehicle{VIN: "VIN1", Name: "Test car"}); err != nil {
		t.Fatal(err)
	}
	start := time.Unix(1_700_000_000, 0)
	id, err := db.StartSession(ctx, "VIN1", "test", start)
	if err != nil {
		t.Fatal(err)
	}
	var samples []store.Sample
	for i := range 60 {
		at := start.Add(time.Duration(i) * time.Second)
		samples = append(samples,
			store.Sample{T: at, PID: obd.RPM.Name, Value: 750},
			store.Sample{T: at, PID: obd.Speed.Name, Value: 0},
			store.Sample{T: at, PID: obd.CoolantTemp.Name, Value: 85})
	}
	if err := db.AddSamples(ctx, id, samples); err != nil {
		t.Fatal(err)
	}
	if err := db.AddDTCs(ctx, id, "stored", []string{"P1351"}, start); err != nil {
		t.Fatal(err)
	}
	if err := db.SetCollector(ctx, id, "v9.9.9", "ELM327 v1.5"); err != nil {
		t.Fatal(err)
	}
	if err := db.EndSession(ctx, id, start.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	return db, id
}

// get serves one request to the dashboard.
func get(t *testing.T, db *store.Store, path string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest("GET", path, nil)
	r.Host = "127.0.0.1:7341"
	w := httptest.NewRecorder()
	New(db, nil).Handler().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("%s: status %d: %s", path, w.Code, w.Body)
	}
	return w
}

func TestDriveAnalysis(t *testing.T) {
	db, id := oneDrive(t)

	w := get(t, db, fmt.Sprintf("/api/drives/%d/analysis", id))
	var out struct {
		Drive struct {
			Idle   *struct{ RPM struct{ Mean float64 } } `json:"idle"`
			Events []struct{ Kind string }               `json:"events"`
		} `json:"drive"`
		Baseline []any `json:"baseline_deltas"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Drive.Idle == nil || out.Drive.Idle.RPM.Mean != 750 {
		t.Errorf("idle %+v", out.Drive.Idle)
	}
	if len(out.Drive.Events) != 1 || out.Drive.Events[0].Kind != "dtc" {
		t.Errorf("events %+v", out.Drive.Events)
	}
	if out.Baseline == nil {
		t.Error("baseline_deltas missing")
	}
}

func TestDriveRecordsLogger(t *testing.T) {
	db, id := oneDrive(t)
	var out struct {
		Session struct {
			Software string `json:"logged_by"`
			Adapter  string `json:"adapter"`
		} `json:"session"`
		Version string `json:"version"`
	}
	if err := json.Unmarshal(get(t, db, fmt.Sprintf("/api/drives/%d", id)).Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Session.Software != "v9.9.9" || out.Session.Adapter != "ELM327 v1.5" || out.Version == "" {
		t.Errorf("%+v", out)
	}
}

func TestSamplesCSV(t *testing.T) {
	db, id := oneDrive(t)
	w := get(t, db, fmt.Sprintf("/api/drives/%d/samples.csv", id))
	rows, err := csv.NewReader(w.Body).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	// Header plus rpm, speed and coolant for 60 seconds.
	if len(rows) != 1+180 || rows[0][2] != "pid" || rows[1][2] != obd.RPM.Name || rows[1][4] != obd.RPM.Unit {
		t.Errorf("%d rows, first %v %v", len(rows), rows[0], rows[1])
	}
}

func TestVehicleTrends(t *testing.T) {
	db, _ := oneDrive(t)
	var vs struct {
		Vehicles []struct {
			VIN    string `json:"vin"`
			Drives int    `json:"drives"`
		} `json:"vehicles"`
	}
	if err := json.Unmarshal(get(t, db, "/api/vehicles").Body.Bytes(), &vs); err != nil {
		t.Fatal(err)
	}
	if len(vs.Vehicles) != 1 || vs.Vehicles[0].VIN != "VIN1" || vs.Vehicles[0].Drives != 1 {
		t.Errorf("%+v", vs)
	}
	// The drive is a minute long: left out by default, included with
	// min_minutes=0.
	for q, want := range map[string]int{"": 0, "?min_minutes=0": 1} {
		var tr struct {
			Drives []any `json:"drives"`
		}
		if err := json.Unmarshal(get(t, db, "/api/vehicles/VIN1/trends"+q).Body.Bytes(), &tr); err != nil {
			t.Fatal(err)
		}
		if len(tr.Drives) != want {
			t.Errorf("%q: %d drives, want %d", q, len(tr.Drives), want)
		}
	}
}
