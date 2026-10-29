package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestStore(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if err := s.UpsertVehicle(ctx, Vehicle{VIN: "VIN1", Name: "Volvo V70", Protocol: "CAN"}); err != nil {
		t.Fatal(err)
	}
	// A later upsert without a name keeps the existing one.
	if err := s.UpsertVehicle(ctx, Vehicle{VIN: "VIN1", Protocol: "CAN"}); err != nil {
		t.Fatal(err)
	}
	if v, err := s.Vehicle(ctx, "VIN1"); err != nil || v.Name != "Volvo V70" {
		t.Errorf("vehicle %+v, %v", v, err)
	}
	if _, err := s.Vehicle(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing vehicle: %v", err)
	}

	t0 := time.UnixMilli(1_700_000_000_000)
	id, err := s.StartSession(ctx, "VIN1", "CAN", t0)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddSamples(ctx, id, []Sample{
		{t0.Add(time.Second), "rpm", 800},
		{t0, "coolant_temp", 20},
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.AddDTCs(ctx, id, "stored", []string{"P0133"}, t0); err != nil {
		t.Fatal(err)
	}
	if err := s.SetReadiness(ctx, id, `{"mil":false}`); err != nil {
		t.Fatal(err)
	}
	if err := s.SetCollector(ctx, id, "v0.1.2", "ELM327 v1.5"); err != nil {
		t.Fatal(err)
	}
	if err := s.EndSession(ctx, id, t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}

	ss, err := s.Session(ctx, id)
	if err != nil || !ss.EndedAt.Equal(t0.Add(time.Minute)) || ss.Readiness != `{"mil":false}` ||
		ss.Software != "v0.1.2" || ss.Adapter != "ELM327 v1.5" {
		t.Errorf("session %+v, %v", ss, err)
	}
	samples, err := s.Samples(ctx, id)
	if err != nil || len(samples) != 2 || samples[0].PID != "coolant_temp" {
		t.Errorf("samples %+v, %v", samples, err)
	}
	dtcs, err := s.DTCs(ctx, id)
	if err != nil || len(dtcs) != 1 || dtcs[0].Code != "P0133" {
		t.Errorf("dtcs %+v, %v", dtcs, err)
	}
	list, err := s.Sessions(ctx, "VIN1", t0.Add(-time.Hour), time.Time{})
	if err != nil || len(list) != 1 {
		t.Errorf("sessions %+v, %v", list, err)
	}
	if list, _ := s.Sessions(ctx, "VIN1", t0.Add(time.Hour), time.Time{}); len(list) != 0 {
		t.Errorf("since filter: %+v", list)
	}
}

func TestMigrateAddsDriveType(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	old, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	// The sessions table as created before drive types existed.
	if _, err := old.Exec(`CREATE TABLE vehicles (vin TEXT PRIMARY KEY, name TEXT NOT NULL DEFAULT '', protocol TEXT NOT NULL DEFAULT '', created_at INTEGER NOT NULL);
		CREATE TABLE sessions (id INTEGER PRIMARY KEY AUTOINCREMENT, vin TEXT NOT NULL REFERENCES vehicles(vin), started_at INTEGER NOT NULL,
			ended_at INTEGER, protocol TEXT NOT NULL DEFAULT '', readiness TEXT NOT NULL DEFAULT '', notes TEXT NOT NULL DEFAULT '');
		INSERT INTO vehicles VALUES ('V', 'Car', 'CAN', 0);
		INSERT INTO sessions (vin, started_at, notes) VALUES ('V', 0, 'old');`); err != nil {
		t.Fatal(err)
	}
	old.Close()

	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	if err := s.Describe(ctx, 1, "commute", "old"); err != nil {
		t.Fatal(err)
	}
	ss, err := s.Session(ctx, 1)
	if err != nil || ss.DriveType != "commute" || ss.Notes != "old" {
		t.Fatalf("%+v %v", ss, err)
	}
	if err := s.Describe(ctx, 99, "", ""); !errors.Is(err, ErrNotFound) {
		t.Errorf("describe missing session: %v", err)
	}
}
