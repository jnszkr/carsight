package dashboard

import (
	"context"
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

func TestDriveAnalysis(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
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
	if err := db.EndSession(ctx, id, start.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}

	r := httptest.NewRequest("GET", fmt.Sprintf("/api/drives/%d/analysis", id), nil)
	r.Host = "127.0.0.1:7341"
	w := httptest.NewRecorder()
	New(db, nil).Handler().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
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
