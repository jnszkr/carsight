package car

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jnszkr/carsight/internal/elm327"
	"github.com/jnszkr/carsight/internal/elmsim"
	"github.com/jnszkr/carsight/internal/obd"
	"github.com/jnszkr/carsight/internal/store"
)

func TestDriveEndsWhenEngineStops(t *testing.T) {
	sim := elmsim.Citroen()
	sim.Speedup, sim.Latency, sim.EngineOffAfter = 30, 0, 30*time.Second
	ctx := context.Background()
	conn := elm327.New(elmsim.New(sim))
	if err := conn.Init(ctx, 0); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(filepath.Join(t.TempDir(), "carsight.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	c := &Car{rw: nopCloser{nil}, Conn: conn, Client: &obd.Client{R: conn, CAN: sim.CAN}, Desc: "sim"}
	d, err := StartDrive(ctx, c, db, DriveOptions{DriveType: "idle_test"})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-d.Done():
	case <-time.After(30 * time.Second):
		t.Fatal("drive still running after the engine stopped")
	}
	if st := d.Status(); !strings.Contains(st.StopReason, "stopped answering") || st.Samples == 0 {
		t.Errorf("stop reason %q, %d samples", st.StopReason, st.Samples)
	}
}
