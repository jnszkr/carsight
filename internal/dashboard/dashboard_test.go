package dashboard

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

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
