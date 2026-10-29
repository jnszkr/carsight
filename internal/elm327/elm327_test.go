package elm327

import (
	"bytes"
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"
)

// scripted replies to each command with a canned response.
type scripted struct {
	replies map[string]string
	out     bytes.Buffer
	pending strings.Builder
}

func (s *scripted) Write(p []byte) (int, error) {
	s.pending.Write(p)
	for {
		buf := s.pending.String()
		i := strings.IndexByte(buf, '\r')
		if i < 0 {
			break
		}
		cmd := buf[:i]
		s.pending.Reset()
		s.pending.WriteString(buf[i+1:])
		reply, ok := s.replies[cmd]
		if !ok {
			reply = "?"
		}
		s.out.WriteString(reply + "\r\r>")
	}
	return len(p), nil
}

func (s *scripted) Read(p []byte) (int, error) { return s.out.Read(p) }

func TestInit(t *testing.T) {
	s := &scripted{replies: map[string]string{
		"": "", "ATZ": "\r\rELM327 v1.5", "ATE0": "ATE0\rOK", "ATL0": "OK", "ATS0": "OK",
		"ATH0": "OK", "ATAT1": "OK", "ATSP0": "OK", "0100": "SEARCHING...\r4100BE3FA813", "ATDPN": "A6",
	}}
	c := New(s)
	if err := c.Init(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	if c.Protocol != 6 || !c.Protocol.IsCAN() {
		t.Errorf("protocol = %v", c.Protocol)
	}
	if c.Version != "ELM327 v1.5" {
		t.Errorf("version = %q", c.Version)
	}
}

// Opening the serial port can reset the adapter, which prints its banner,
// and the port probe's bare CR gets a "?" of its own. Neither reply may be
// taken for ATZ's.
func TestInitSkipsRepliesAlreadyWaiting(t *testing.T) {
	s := &scripted{replies: map[string]string{
		"": "?", "ATZ": "ATZ\r\r\rELM327 v2.3", "ATE0": "OK", "ATL0": "OK", "ATS0": "OK", "ATH0": "OK",
		"ATAT1": "OK", "ATSP5": "OK", "0100": "BUS INIT: OK\r4100983B4011", "ATDPN": "5",
	}}
	s.out.WriteString("\r\rELM327 v2.3\r\r>\r?\r\r>")
	c := New(s)
	if err := c.Init(context.Background(), 5); err != nil {
		t.Fatal(err)
	}
	if c.Protocol != 5 || c.Version != "ELM327 v2.3" {
		t.Errorf("protocol = %v, version = %q", c.Protocol, c.Version)
	}
}

// slow holds back the reply to one command, like a K-line bus handshake.
type slow struct {
	*scripted
	cmd   string
	delay time.Duration
	until time.Time
}

func (s *slow) Write(p []byte) (int, error) {
	if strings.TrimSuffix(string(p), "\r") == s.cmd {
		s.until = time.Now().Add(s.delay)
	}
	return s.scripted.Write(p)
}

func (s *slow) Read(p []byte) (int, error) {
	if time.Now().Before(s.until) {
		return 0, io.EOF
	}
	return s.scripted.Read(p)
}

func TestInitSlowSearch(t *testing.T) {
	s := &slow{scripted: &scripted{replies: map[string]string{
		"": "", "ATZ": "ELM327 v1.5", "ATE0": "OK", "ATL0": "OK", "ATS0": "OK", "ATH0": "OK", "ATAT1": "OK",
		"ATSP0": "OK", "0100": "SEARCHING...\r4100BE1FA813", "ATDPN": "A5",
	}}, cmd: "0100", delay: 300 * time.Millisecond}
	c := New(s)
	c.timeout = 100 * time.Millisecond // the search must not be held to it
	if err := c.Init(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	if c.Protocol != 5 {
		t.Errorf("protocol = %v", c.Protocol)
	}
}

func TestInitProtocol(t *testing.T) {
	s := &scripted{replies: map[string]string{
		"": "", "ATZ": "ELM327 v1.5", "ATE0": "OK", "ATL0": "OK", "ATS0": "OK", "ATH0": "OK", "ATAT1": "OK",
		"ATSP3": "OK", "0100": "BUS INIT: ...OK\r4100BE1FA813", "ATDPN": "3",
	}}
	c := New(s)
	if err := c.Init(context.Background(), 3); err != nil {
		t.Fatal(err)
	}
	if c.Protocol != 3 || c.Protocol.IsCAN() {
		t.Errorf("protocol = %v", c.Protocol)
	}
	for _, reply := range []string{"BUS INIT: ERROR", "BUS INIT: ...ERROR"} {
		s.replies["0100"] = reply
		c := New(s)
		c.retryWait = time.Millisecond
		if err := c.Init(context.Background(), 3); !errors.Is(err, ErrUnableConnect) {
			t.Errorf("%s: %v", reply, err)
		}
	}
	if err := New(s).Init(context.Background(), 10); err == nil {
		t.Error("protocol 10 accepted")
	}
}

// busy fails the bus initialisation a few times, like a K-line ECU that
// still holds an earlier session.
type busy struct {
	*scripted
	fails int
}

func (b *busy) Write(p []byte) (int, error) {
	if string(p) == "0100\r" && b.fails > 0 {
		b.fails--
		b.out.WriteString("BUS INIT: ERROR\r\r>")
		return len(p), nil
	}
	return b.scripted.Write(p)
}

func TestInitRetriesBusInit(t *testing.T) {
	replies := map[string]string{
		"": "", "ATZ": "ELM327 v2.3", "ATE0": "OK", "ATL0": "OK", "ATS0": "OK", "ATH0": "OK", "ATAT1": "OK",
		"ATSP5": "OK", "0100": "BUS INIT: OK\r4100983B4011", "ATDPN": "5",
	}
	for fails, wantErr := range []bool{false, false, false, true} {
		c := New(&busy{scripted: &scripted{replies: replies}, fails: fails})
		c.retryWait = time.Millisecond
		if err := c.Init(context.Background(), 5); (err != nil) != wantErr {
			t.Errorf("%d failed inits: %v", fails, err)
		}
	}
}

func TestRequestErrors(t *testing.T) {
	s := &scripted{replies: map[string]string{"0100": "UNABLE TO CONNECT", "0902": "NO DATA", "01A6": "CAN ERROR"}}
	c := New(s)
	ctx := context.Background()
	if _, err := c.Request(ctx, "0100"); !errors.Is(err, ErrUnableConnect) {
		t.Errorf("0100: %v", err)
	}
	if _, err := c.Request(ctx, "0902"); !errors.Is(err, ErrNoData) {
		t.Errorf("0902: %v", err)
	}
	if _, err := c.Request(ctx, "01A6"); !errors.Is(err, ErrBus) {
		t.Errorf("01A6: %v", err)
	}
	s.replies["0A"] = "7F0A11"
	var neg *NegativeResponse
	if _, err := c.Request(ctx, "0A"); !errors.Is(err, ErrNoData) || !errors.As(err, &neg) || neg.Service != 0x0A || neg.Code != 0x11 {
		t.Errorf("0A: %v", err)
	}
	s.replies["03"] = "7F0311\r43010133"
	if got, err := c.Request(ctx, "03"); err != nil || len(got) != 1 || got[0][0] != 0x43 {
		t.Errorf("03 with one refusal: %x, %v", got, err)
	}
	if _, err := c.Request(ctx, "ZZ"); !errors.Is(err, ErrUnknownCmd) {
		t.Errorf("ZZ: %v", err)
	}
}

func TestDecodeResponse(t *testing.T) {
	tests := []struct {
		name  string
		lines []string
		want  [][]byte
	}{
		{"single", []string{"410C1AF8"}, [][]byte{{0x41, 0x0C, 0x1A, 0xF8}}},
		{"spaces", []string{"41 0C 1A F8"}, [][]byte{{0x41, 0x0C, 0x1A, 0xF8}}},
		{"two ecus", []string{"4100BE3FA813", "410098180001"}, [][]byte{
			{0x41, 0x00, 0xBE, 0x3F, 0xA8, 0x13}, {0x41, 0x00, 0x98, 0x18, 0x00, 0x01}}},
		{"multi-frame", []string{"014", "0:490201314D38", "1:4A363844303130", "2:3030303030303030"}, [][]byte{
			{0x49, 0x02, 0x01, '1', 'M', '8', 'J', '6', '8', 'D', '0', '1', '0', '0', '0', '0', '0', '0', '0', '0'}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := decodeResponse(tt.lines)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("got % X, want % X", got, tt.want)
			}
		})
	}
}

// A command abandoned before its prompt must not hand its reply to the
// next command.
func TestResyncAfterAbandonedCommand(t *testing.T) {
	s := &scripted{replies: map[string]string{"0100": "41 00 BE 3F A8 13", "010D": "41 0D 32"}}
	c := New(s)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.Command(ctx, "0100"); err == nil {
		t.Fatal("cancelled command succeeded")
	}
	got, err := c.Request(context.Background(), "010D")
	if err != nil || len(got) != 1 || !reflect.DeepEqual(got[0], []byte{0x41, 0x0D, 0x32}) {
		t.Errorf("after resync: % X %v", got, err)
	}
}
