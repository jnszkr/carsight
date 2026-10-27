package elmsim

import (
	"context"
	"reflect"
	"testing"

	"github.com/jnszkr/carsight/internal/elm327"
	"github.com/jnszkr/carsight/internal/obd"
)

func TestSimulatorRoundTrip(t *testing.T) {
	for _, car := range []Car{Volvo(), Citroen()} {
		car.DTCs = []string{"P0133", "P0420", "P0171"}
		t.Run(car.VIN, func(t *testing.T) {
			ctx := context.Background()
			conn := elm327.New(New(car))
			if err := conn.Init(ctx); err != nil {
				t.Fatal(err)
			}
			if conn.Protocol.IsCAN() != car.CAN {
				t.Fatalf("protocol %v, CAN=%v", conn.Protocol, car.CAN)
			}
			c := &obd.Client{R: conn, CAN: car.CAN}
			vin, err := c.VIN(ctx)
			if err != nil || vin != car.VIN {
				t.Errorf("VIN %q, %v", vin, err)
			}
			sup, err := c.SupportedPIDs(ctx)
			if err != nil {
				t.Fatal(err)
			}
			for _, p := range car.PIDs {
				if p%0x20 != 0 && !sup[p] {
					t.Errorf("PID %02X missing", p)
				}
			}
			rpm, err := c.Read(ctx, obd.RPM)
			if err != nil || rpm < 500 || rpm > 2000 {
				t.Errorf("rpm %v, %v", rpm, err)
			}
			dtcs, err := c.DTCs(ctx, false)
			if want := []string{"P0133", "P0171", "P0420"}; err != nil || !reflect.DeepEqual(dtcs, want) {
				t.Errorf("DTCs %v, %v", dtcs, err)
			}
			if err := c.ClearDTCs(ctx); err != nil {
				t.Fatal(err)
			}
			if dtcs, _ := c.DTCs(ctx, false); len(dtcs) != 0 {
				t.Errorf("DTCs after clear: %v", dtcs)
			}
		})
	}
}
