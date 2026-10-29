// Package dashboard serves a local web page that charts drives: the drive
// being logged, live over server-sent events, and stored drives for replay.
// It listens on the loopback interface only.
package dashboard

import (
	"context"
	_ "embed"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/jnszkr/carsight/internal/analyze"
	"github.com/jnszkr/carsight/internal/buildinfo"
	"github.com/jnszkr/carsight/internal/car"
	"github.com/jnszkr/carsight/internal/obd"
	"github.com/jnszkr/carsight/internal/store"
)

//go:embed index.html
var page []byte

// DefaultAddr is where the dashboard listens unless told otherwise. If the
// port is taken (another carsight is serving), a free one is used.
const DefaultAddr = "127.0.0.1:7341"

// maxPoints caps the points per PID returned for a stored drive.
const maxPoints = 4000

// Baseline for a drive's analysis, as the MCP server uses: up to
// baselineDrives earlier drives of the same car lasting at least
// baselineMinDur.
const (
	baselineDrives = 10
	baselineMinDur = 5 * time.Minute
)

// Server serves the dashboard.
type Server struct {
	db *store.Store
	// live returns the drive being logged (or the last one), if any.
	live func() *car.Drive

	mu  sync.Mutex
	srv *http.Server
	url string
}

// New returns a dashboard for db. live may be nil when nothing is logged in
// this process.
func New(db *store.Store, live func() *car.Drive) *Server {
	if live == nil {
		live = func() *car.Drive { return nil }
	}
	return &Server{db: db, live: live}
}

// Start listens on addr (DefaultAddr if empty), falling back to a free
// loopback port if it is taken, and returns the base URL. It is a no-op
// returning the same URL once started.
func (s *Server) Start(addr string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.srv != nil {
		return s.url, nil
	}
	if addr == "" {
		addr = DefaultAddr
	}
	ln, err := net.Listen("tcp", addr)
	if errors.Is(err, syscall.EADDRINUSE) {
		ln, err = net.Listen("tcp", "127.0.0.1:0")
	}
	if err != nil {
		return "", err
	}
	s.srv = &http.Server{Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second}
	s.url = "http://" + ln.Addr().String()
	go s.srv.Serve(ln)
	return s.url, nil
}

// URL returns the base URL, or "" if the dashboard isn't running.
func (s *Server) URL() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.url
}

// Close stops the server.
func (s *Server) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.srv == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := s.srv.Shutdown(ctx)
	if errors.Is(err, context.DeadlineExceeded) {
		// Live streams don't end on their own.
		err = s.srv.Close()
	}
	s.srv, s.url = nil, ""
	return err
}

// Handler returns the dashboard's HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	serve := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(page)
	}
	mux.HandleFunc("GET /{$}", serve)
	mux.HandleFunc("GET /drive/{id}", serve)
	mux.HandleFunc("GET /api/sessions", s.sessions)
	mux.HandleFunc("GET /api/drives/{id}", s.drive)
	mux.HandleFunc("GET /api/drives/{id}/live", s.stream)
	mux.HandleFunc("GET /api/drives/{id}/analysis", s.analysis)
	mux.HandleFunc("GET /api/drives/{id}/samples.csv", s.samplesCSV)
	mux.HandleFunc("GET /vehicle/{vin}", serve)
	mux.HandleFunc("GET /api/vehicles", s.vehicles)
	mux.HandleFunc("GET /api/vehicles/{vin}/trends", s.trends)
	return localOnly(mux)
}

// localOnly refuses requests whose Host isn't a loopback name, so a web page
// can't read the data through DNS rebinding.
func localOnly(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, err := net.SplitHostPort(r.Host)
		if err != nil {
			host = r.Host
		}
		if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
			http.Error(w, "forbidden host", http.StatusForbidden)
			return
		}
		h.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// liveDrive returns the drive logging session id, if it is still running.
func (s *Server) liveDrive(id int64) *car.Drive {
	d := s.live()
	if d == nil {
		return nil
	}
	if st := d.Status(); st.SessionID != id || !st.Running {
		return nil
	}
	return d
}

type sessionOut struct {
	ID        int64     `json:"id"`
	VIN       string    `json:"vin"`
	Vehicle   string    `json:"vehicle,omitempty"`
	StartedAt time.Time `json:"started_at"`
	Duration  float64   `json:"duration_s"`
	Live      bool      `json:"live"`
	Protocol  string    `json:"protocol,omitempty"`
	DriveType string    `json:"drive_type,omitempty"`
	Notes     string    `json:"notes,omitempty"`
	Software  string    `json:"logged_by,omitempty"` // carsight version that logged it
	Adapter   string    `json:"adapter,omitempty"`
}

func (s *Server) sessionOut(ctx context.Context, sess store.Session, names map[string]string) sessionOut {
	name, ok := names[sess.VIN]
	if !ok {
		v, _ := s.db.Vehicle(ctx, sess.VIN)
		name = v.Name
		names[sess.VIN] = name
	}
	o := sessionOut{ID: sess.ID, VIN: sess.VIN, Vehicle: name, StartedAt: sess.StartedAt, Protocol: sess.Protocol,
		DriveType: sess.DriveType, Notes: sess.Notes, Software: sess.Software, Adapter: sess.Adapter, Live: s.liveDrive(sess.ID) != nil}
	if !sess.EndedAt.IsZero() {
		o.Duration = math.Round(sess.EndedAt.Sub(sess.StartedAt).Seconds())
	} else if o.Live {
		o.Duration = math.Round(time.Since(sess.StartedAt).Seconds())
	}
	return o
}

func (s *Server) sessions(w http.ResponseWriter, r *http.Request) {
	ss, err := s.db.Sessions(r.Context(), "", time.Time{}, time.Time{})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	names := map[string]string{}
	out := []sessionOut{}
	for _, sess := range ss {
		out = append(out, s.sessionOut(r.Context(), sess, names))
	}
	writeJSON(w, map[string]any{"sessions": out, "version": buildinfo.Version()})
}

type pidInfo struct {
	Name   string         `json:"name"`
	Desc   string         `json:"desc"`
	Unit   string         `json:"unit"`
	Rate   obd.Rate       `json:"rate"`
	Labels map[int]string `json:"labels,omitempty"`
}

type series struct {
	T []float64 `json:"t"` // seconds from the start of the drive
	V []float64 `json:"v"`
}

type dtcOut struct {
	Code string `json:"code"`
	Kind string `json:"kind"`
}

type driveOut struct {
	Session sessionOut         `json:"session"`
	PIDs    []pidInfo          `json:"pids"` // in polling order
	Series  map[string]*series `json:"series"`
	DTCs    []dtcOut           `json:"dtcs"`
	Status  *car.Status        `json:"status,omitempty"` // while live
	// Info holds the snapshots taken at the start and end of the drive.
	Info json.RawMessage `json:"info,omitempty"`
	// Units of the snapshot values, by PID name.
	Units map[string]string `json:"units"`
	// Version is the carsight serving the page (the drive's logger is
	// Session.Software).
	Version string `json:"version"`
}

func (s *Server) sessionByID(w http.ResponseWriter, r *http.Request) (store.Session, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad session id", http.StatusBadRequest)
		return store.Session{}, false
	}
	sess, err := s.db.Session(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		http.Error(w, fmt.Sprintf("no session %d", id), http.StatusNotFound)
		return sess, false
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return sess, false
	}
	return sess, true
}

func (s *Server) drive(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.sessionByID(w, r)
	if !ok {
		return
	}
	all, err := s.db.Samples(r.Context(), sess.ID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	out := driveOut{Session: s.sessionOut(r.Context(), sess, map[string]string{}), Series: map[string]*series{}, DTCs: []dtcOut{},
		Version: buildinfo.Version()}
	for _, sm := range all {
		ser := out.Series[sm.PID]
		if ser == nil {
			ser = &series{}
			out.Series[sm.PID] = ser
		}
		ser.T = append(ser.T, math.Round(sm.T.Sub(sess.StartedAt).Seconds()*1000)/1000)
		ser.V = append(ser.V, sm.Value)
	}
	if d := s.liveDrive(sess.ID); d != nil {
		st := d.Status()
		out.Status = &st
		for _, name := range st.PIDs {
			if out.Series[name] == nil {
				out.Series[name] = &series{T: []float64{}, V: []float64{}}
			}
		}
	} else {
		// A live page keeps every point; a stored drive is thinned.
		for _, ser := range out.Series {
			ser.T, ser.V = thin(ser.T, ser.V, maxPoints)
		}
	}
	out.Units = map[string]string{}
	for _, p := range obd.All {
		out.Units[p.Name] = p.Unit
		if _, ok := out.Series[p.Name]; ok {
			out.PIDs = append(out.PIDs, pidInfo{p.Name, p.Desc, p.Unit, p.Rate, p.Labels})
		}
	}
	if sess.Info != "" {
		out.Info = json.RawMessage(sess.Info)
	}
	dtcs, err := s.db.DTCs(r.Context(), sess.ID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	seen := map[dtcOut]bool{}
	for _, d := range dtcs {
		if o := (dtcOut{d.Code, d.Kind}); !seen[o] {
			seen[o] = true
			out.DTCs = append(out.DTCs, o)
		}
	}
	writeJSON(w, out)
}

// analysis is the drive summary and its comparison with the car's earlier
// drives: what get_drive_summary gives an AI assistant.
func (s *Server) analysis(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.sessionByID(w, r)
	if !ok {
		return
	}
	a, err := analyze.AnalyzeDrive(r.Context(), s.db, sess, baselineDrives, baselineMinDur)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	a.Drive.Snapshots = nil // the page already has them from /api/drives/{id}
	writeJSON(w, struct {
		analyze.DriveAnalysis
		MetricDescriptions map[string]string `json:"metric_descriptions"`
		Version            string            `json:"version"`
	}{a, analyze.MetricDescriptions, buildinfo.Version()})
}

// samplesCSV is every sample of a drive at full resolution, one row per
// reading.
func (s *Server) samplesCSV(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.sessionByID(w, r)
	if !ok {
		return
	}
	all, err := s.db.Samples(r.Context(), sess.ID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	units := map[string]string{}
	for _, p := range obd.All {
		units[p.Name] = p.Unit
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="carsight-drive-%d.csv"`, sess.ID))
	cw := csv.NewWriter(w)
	cw.Write([]string{"time", "seconds", "pid", "value", "unit"})
	for _, sm := range all {
		cw.Write([]string{sm.T.UTC().Format(time.RFC3339Nano), strconv.FormatFloat(sm.T.Sub(sess.StartedAt).Seconds(), 'f', 3, 64),
			sm.PID, strconv.FormatFloat(sm.Value, 'f', -1, 64), units[sm.PID]})
	}
	cw.Flush()
}

type vehicleOut struct {
	VIN       string    `json:"vin"`
	Name      string    `json:"name,omitempty"`
	Protocol  string    `json:"protocol,omitempty"`
	Drives    int       `json:"drives"`
	LastDrive time.Time `json:"last_drive,omitempty"`
}

func (s *Server) vehicles(w http.ResponseWriter, r *http.Request) {
	vs, err := s.db.Vehicles(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	out := []vehicleOut{}
	for _, v := range vs {
		ss, err := s.db.Sessions(r.Context(), v.VIN, time.Time{}, time.Time{})
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		o := vehicleOut{VIN: v.VIN, Name: v.Name, Protocol: v.Protocol, Drives: len(ss)}
		if len(ss) > 0 {
			o.LastDrive = ss[0].StartedAt
		}
		out = append(out, o)
	}
	writeJSON(w, map[string]any{"vehicles": out, "version": buildinfo.Version()})
}

// trends is a car's key metrics across its drives. min_minutes sets the
// shortest drive that counts (default 5, as the MCP server uses).
func (s *Server) trends(w http.ResponseWriter, r *http.Request) {
	vin := r.PathValue("vin")
	v, err := s.db.Vehicle(r.Context(), vin)
	if errors.Is(err, store.ErrNotFound) {
		http.Error(w, fmt.Sprintf("no vehicle %s", vin), http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	minDur := baselineMinDur
	if m := r.URL.Query().Get("min_minutes"); m != "" {
		f, err := strconv.ParseFloat(m, 64)
		if err != nil || f < 0 {
			http.Error(w, "bad min_minutes", http.StatusBadRequest)
			return
		}
		minDur = time.Duration(f * float64(time.Minute))
	}
	t, err := analyze.AnalyzeTrends(r.Context(), s.db, analyze.TrendFilter{VIN: vin, MinDuration: minDur,
		DriveType: strings.TrimSpace(r.URL.Query().Get("drive_type"))})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, struct {
		analyze.TrendAnalysis
		Protocol           string            `json:"protocol,omitempty"`
		MinMinutes         float64           `json:"min_minutes"`
		MetricDescriptions map[string]string `json:"metric_descriptions"`
		Version            string            `json:"version"`
	}{t, v.Protocol, minDur.Minutes(), analyze.MetricDescriptions, buildinfo.Version()})
}

// thin reduces a series to at most n points by keeping the minimum and
// maximum of each bucket, so spikes survive.
func thin(t, v []float64, n int) ([]float64, []float64) {
	if len(t) <= n {
		return t, v
	}
	buckets := n / 2
	size := float64(len(t)) / float64(buckets)
	var ot, ov []float64
	for b := range buckets {
		lo, hi := int(float64(b)*size), int(float64(b+1)*size)
		mn, mx := lo, lo
		for i := lo; i < hi; i++ {
			if v[i] < v[mn] {
				mn = i
			}
			if v[i] > v[mx] {
				mx = i
			}
		}
		for _, i := range []int{min(mn, mx), max(mn, mx)} {
			if len(ot) == 0 || ot[len(ot)-1] != t[i] {
				ot = append(ot, t[i])
				ov = append(ov, v[i])
			}
		}
	}
	return ot, ov
}

// stream sends the drive's readings as server-sent events: "samples" with
// [pid, t, value] triples each polling cycle, "status" every second, and
// "end" with the final status when logging stops.
func (s *Server) stream(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.sessionByID(w, r)
	if !ok {
		return
	}
	d := s.liveDrive(sess.ID)
	if d == nil {
		http.Error(w, "this drive isn't being logged", http.StatusGone)
		return
	}
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	ch, unsubscribe := d.Subscribe()
	defer unsubscribe()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	send := func(event string, v any) error {
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b); err != nil {
			return err
		}
		fl.Flush()
		return nil
	}
	if send("status", d.Status()) != nil {
		return
	}
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	start := sess.StartedAt
	for {
		select {
		case <-r.Context().Done():
			return
		case <-tick.C:
			if send("status", d.Status()) != nil {
				return
			}
		case cycle, ok := <-ch:
			if !ok {
				send("end", d.Status())
				return
			}
			rows := make([][3]any, len(cycle))
			for i, sm := range cycle {
				rows[i] = [3]any{sm.PID, math.Round(sm.T.Sub(start).Seconds()*1000) / 1000, sm.Value}
			}
			if send("samples", rows) != nil {
				return
			}
		}
	}
}
