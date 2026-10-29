// Package elm327 talks to an ELM327-compatible OBD-II adapter (such as the
// Vgate vLinker FS) over a byte stream, usually a USB serial port.
package elm327

import (
	"bufio"
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
)

// Errors reported by the adapter in place of data.
var (
	ErrNoData        = errors.New("elm327: no data")
	ErrUnknownCmd    = errors.New("elm327: unknown command")
	ErrUnableConnect = errors.New("elm327: unable to connect to vehicle")
	ErrBus           = errors.New("elm327: bus error")
	ErrTimeout       = errors.New("elm327: timeout waiting for prompt")
)

// NegativeResponse is a negative response (7F, service, code) from the ECU:
// it understood the request but won't answer it, usually because it doesn't
// support the service. It matches ErrNoData.
type NegativeResponse struct{ Service, Code byte }

func (e *NegativeResponse) Error() string {
	reasons := map[byte]string{0x11: "service not supported", 0x12: "sub-function not supported",
		0x22: "conditions not correct", 0x31: "request out of range"}
	if r, ok := reasons[e.Code]; ok {
		return fmt.Sprintf("elm327: ECU refused service %02X: %s", e.Service, r)
	}
	return fmt.Sprintf("elm327: ECU refused service %02X: code %02X", e.Service, e.Code)
}

func (e *NegativeResponse) Is(target error) bool { return target == ErrNoData }

// searchTimeout bounds the first request after ATSP. The adapter may try
// every protocol in turn, and a K-line handshake (ISO 9141 or KWP2000 with
// 5-baud init) alone can take several seconds.
const searchTimeout = 30 * time.Second

// Protocol is an OBD protocol number as reported by ATDPN.
type Protocol byte

// IsCAN reports whether the protocol is one of the ISO 15765-4 CAN variants.
func (p Protocol) IsCAN() bool { return p >= 6 && p <= 9 }

func (p Protocol) String() string {
	names := map[Protocol]string{
		1: "SAE J1850 PWM",
		2: "SAE J1850 VPW",
		3: "ISO 9141-2",
		4: "ISO 14230-4 KWP (5 baud init)",
		5: "ISO 14230-4 KWP (fast init)",
		6: "ISO 15765-4 CAN (11 bit, 500 kbaud)",
		7: "ISO 15765-4 CAN (29 bit, 500 kbaud)",
		8: "ISO 15765-4 CAN (11 bit, 250 kbaud)",
		9: "ISO 15765-4 CAN (29 bit, 250 kbaud)",
	}
	if n, ok := names[p]; ok {
		return n
	}
	return fmt.Sprintf("protocol %d", byte(p))
}

// Conn is a connection to an ELM327 adapter. It is safe for use by one
// goroutine at a time; calls are serialised internally.
type Conn struct {
	mu      sync.Mutex
	rw      io.ReadWriter
	r       *bufio.Reader
	timeout time.Duration
	// unsynced is set when a command gave up before the adapter's prompt
	// (cancelled or timed out): its reply may still arrive and must not be
	// taken for the next command's. resync is how long to wait for it.
	unsynced bool
	resync   time.Duration
	// retryWait is the pause before retrying a failed bus initialisation.
	retryWait time.Duration
	Protocol  Protocol
	Version   string
}

// New wraps rw. The read side should return within a reasonable time
// (serial ports should be opened with a read timeout) so a stalled adapter
// does not block forever.
func New(rw io.ReadWriter) *Conn {
	return &Conn{rw: rw, r: bufio.NewReader(rw), timeout: 5 * time.Second, retryWait: 6 * time.Second}
}

// Init resets the adapter and configures it for compact, machine-readable
// output, then connects to the car with protocol p, or searches for the
// protocol when p is 0.
func (c *Conn) Init(ctx context.Context, p Protocol) error {
	if p > 9 {
		return fmt.Errorf("elm327: unknown protocol %d (want 0-9)", p)
	}
	// A stray partial command may be sitting in the adapter's buffer; a bare
	// CR flushes it and gets us a prompt. Replies may already be waiting too:
	// opening the port can reset the adapter, which prints its banner, and the
	// port probe's CR gets an answer of its own. Discard everything until the
	// line goes quiet, or each reply would be taken for the next command's.
	_, _ = c.Command(ctx, "")
	if err := c.drain(ctx); err != nil {
		return fmt.Errorf("reset: %w", err)
	}
	ver, err := c.Command(ctx, "ATZ")
	if err != nil {
		return fmt.Errorf("reset: %w", err)
	}
	for _, l := range ver {
		if strings.Contains(strings.ToUpper(l), "ELM") {
			c.Version = l
		}
	}
	for _, cmd := range []string{"ATE0", "ATL0", "ATS0", "ATH0", "ATAT1", fmt.Sprintf("ATSP%d", p)} {
		if _, err := c.Command(ctx, cmd); err != nil {
			return fmt.Errorf("%s: %w", cmd, err)
		}
	}
	// ATSP only takes effect on the first request; 0100 is supported by
	// every OBD-II car and forces the search or the bus initialisation.
	if err := c.connect(ctx); err != nil {
		if p == 0 {
			return fmt.Errorf("protocol search: %w", err)
		}
		return fmt.Errorf("connect with %v: %w", p, err)
	}
	dpn, err := c.Command(ctx, "ATDPN")
	if err != nil || len(dpn) == 0 {
		return fmt.Errorf("read protocol: %w", err)
	}
	n := strings.TrimPrefix(strings.ToUpper(dpn[0]), "A")
	if len(n) != 1 || n[0] < '1' || n[0] > '9' {
		return fmt.Errorf("unexpected ATDPN response %q", dpn[0])
	}
	c.Protocol = Protocol(n[0] - '0')
	return nil
}

// connect sends the first request. A K-line ECU refuses a new handshake
// while it still holds a session, such as one the adapter kept alive after
// the previous run. The session ends after 5 s without bus traffic (P3max),
// and every handshake attempt counts as traffic, so retry only after that.
func (c *Conn) connect(ctx context.Context) error {
	for attempt := 1; ; attempt++ {
		_, err := c.command(ctx, "0100", searchTimeout)
		if !errors.Is(err, ErrUnableConnect) || attempt == 3 {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(c.retryWait):
		}
	}
}

// Close ends the session with the car, so the adapter stops sending the
// keep-alive requests that would hold a K-line ECU's session open for the
// next connection. It does not close the underlying port.
func (c *Conn) Close(ctx context.Context) error {
	_, err := c.Command(ctx, "ATPC")
	return err
}

// Command sends cmd and returns the non-empty response lines, without the
// echo, status chatter ("SEARCHING...") or the trailing prompt.
func (c *Conn) Command(ctx context.Context, cmd string) ([]string, error) {
	return c.command(ctx, cmd, c.timeout)
}

func (c *Conn) command(ctx context.Context, cmd string, timeout time.Duration) ([]string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.unsynced {
		// Wait out the abandoned reply. Writing now would interrupt the
		// adapter mid-command or leave its answer in the buffer.
		if _, err := c.readUntilPrompt(context.WithoutCancel(ctx), c.resync); err != nil {
			return nil, err
		}
		c.unsynced = false
	}
	if _, err := io.WriteString(c.rw, cmd+"\r"); err != nil {
		return nil, fmt.Errorf("elm327: write: %w", err)
	}
	raw, err := c.readUntilPrompt(ctx, timeout)
	if err != nil {
		c.unsynced, c.resync = true, timeout
		return nil, err
	}
	return parseLines(cmd, raw)
}

// drainQuiet is how long the adapter must stay silent for drain to finish.
const drainQuiet = 300 * time.Millisecond

// drain discards input until nothing has arrived for drainQuiet, giving up
// if the adapter is still talking a command timeout later.
func (c *Conn) drain(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	deadline := time.Now().Add(drainQuiet + c.timeout)
	last := time.Now()
	for time.Since(last) < drainQuiet {
		if err := ctx.Err(); err != nil {
			return err
		}
		if time.Now().After(deadline) {
			return ErrTimeout
		}
		if _, err := c.r.ReadByte(); err == io.EOF {
			time.Sleep(time.Millisecond)
		} else if err != nil {
			return fmt.Errorf("elm327: read: %w", err)
		} else {
			last = time.Now()
		}
	}
	c.unsynced = false
	return nil
}

func (c *Conn) readUntilPrompt(ctx context.Context, timeout time.Duration) ([]byte, error) {
	deadline := time.Now().Add(timeout)
	var buf bytes.Buffer
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if time.Now().After(deadline) {
			return nil, ErrTimeout
		}
		b, err := c.r.ReadByte()
		if err == io.EOF {
			// Serial reads return EOF on read timeout; keep waiting.
			time.Sleep(time.Millisecond)
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("elm327: read: %w", err)
		}
		if b == '>' {
			return buf.Bytes(), nil
		}
		if b != 0 {
			buf.WriteByte(b)
		}
	}
}

func parseLines(cmd string, raw []byte) ([]string, error) {
	var lines []string
	for _, l := range strings.FieldsFunc(string(raw), func(r rune) bool { return r == '\r' || r == '\n' }) {
		l = strings.TrimSpace(l)
		up := strings.ToUpper(l)
		switch {
		case l == "":
			continue
		case cmd != "" && strings.EqualFold(strings.ReplaceAll(l, " ", ""), strings.ReplaceAll(cmd, " ", "")):
			continue // echo, before ATE0 takes effect
		case strings.HasPrefix(up, "SEARCHING"), up == "BUS INIT: ...OK", up == "BUS INIT: OK", up == "BUS INIT...":
			continue
		case up == "NO DATA":
			return nil, ErrNoData
		case up == "?":
			return nil, fmt.Errorf("%w: %q", ErrUnknownCmd, cmd)
		case strings.Contains(up, "UNABLE TO CONNECT"), strings.HasPrefix(up, "BUS INIT") && strings.Contains(up, "ERROR"):
			// "BUS INIT: ...ERROR", or "BUS INIT: ERROR" on some clones: the
			// K-line ECU did not answer the handshake.
			return nil, ErrUnableConnect
		case strings.Contains(up, "CAN ERROR"), strings.Contains(up, "BUS ERROR"),
			strings.Contains(up, "FB ERROR"), strings.Contains(up, "DATA ERROR"),
			strings.Contains(up, "BUFFER FULL"), up == "STOPPED":
			return nil, fmt.Errorf("%w: %s", ErrBus, l)
		}
		lines = append(lines, l)
	}
	return lines, nil
}

// Request sends an OBD request (hex, e.g. "010C") and returns the payload
// bytes of each ECU response. On CAN, multi-frame (ISO-TP) responses are
// reassembled into a single payload.
func (c *Conn) Request(ctx context.Context, req string) ([][]byte, error) {
	lines, err := c.Command(ctx, req)
	if err != nil {
		return nil, err
	}
	return decodeResponse(lines)
}

// decodeResponse turns headerless response lines into payloads. Multi-frame
// CAN responses look like:
//
//	014
//	0:490201314D38
//	1:4A363844303130
//	2:3030303030303030
//
// where the first line is the total byte count and each following line is
// prefixed with a frame index.
func decodeResponse(lines []string) ([][]byte, error) {
	var out [][]byte
	var multi []byte
	multiLen := -1
	for _, l := range lines {
		l = strings.ReplaceAll(l, " ", "")
		if idx := strings.IndexByte(l, ':'); idx >= 0 {
			b, err := hexBytes(l[idx+1:])
			if err != nil {
				return nil, err
			}
			multi = append(multi, b...)
			continue
		}
		if len(l) == 3 {
			var n int
			if _, err := fmt.Sscanf(l, "%x", &n); err == nil {
				if multi != nil {
					out = append(out, trim(multi, multiLen))
					multi = nil
				}
				multiLen = n
				multi = []byte{}
				continue
			}
		}
		b, err := hexBytes(l)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	if multi != nil {
		out = append(out, trim(multi, multiLen))
	}
	// Keep the answers; a refusal only matters if no ECU answered.
	var neg error
	pos := out[:0]
	for _, b := range out {
		if len(b) >= 3 && b[0] == 0x7F {
			neg = &NegativeResponse{Service: b[1], Code: b[2]}
			continue
		}
		pos = append(pos, b)
	}
	if len(pos) == 0 && neg != nil {
		return nil, neg
	}
	if len(pos) == 0 {
		return nil, ErrNoData
	}
	return pos, nil
}

func trim(b []byte, n int) []byte {
	if n >= 0 && n < len(b) {
		return b[:n]
	}
	return b
}

func hexBytes(s string) ([]byte, error) {
	b, err := hex.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("elm327: bad hex %q: %w", s, err)
	}
	return b, nil
}
