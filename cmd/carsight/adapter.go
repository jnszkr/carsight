package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"go.bug.st/serial"

	"github.com/jnszkr/carsight/internal/elmsim"
)

// openAdapter opens the adapter named by port: a serial device path, "auto"
// to pick the first /dev/tty.usbserial-* device, or "sim:volvo" /
// "sim:citroen" for the built-in simulator.
func openAdapter(port string, baud int, simSpeedup float64) (io.ReadWriteCloser, string, error) {
	if sim, ok := strings.CutPrefix(port, "sim:"); ok {
		var car elmsim.Car
		switch sim {
		case "volvo":
			car = elmsim.Volvo()
		case "citroen":
			car = elmsim.Citroen()
		default:
			return nil, "", fmt.Errorf("unknown simulator %q (want sim:volvo or sim:citroen)", sim)
		}
		car.Speedup = simSpeedup
		return nopCloser{elmsim.New(car)}, port, nil
	}
	if port == "auto" {
		matches, _ := filepath.Glob("/dev/tty.usbserial-*")
		if len(matches) == 0 {
			matches, _ = filepath.Glob("/dev/ttyUSB*")
		}
		if len(matches) == 0 {
			return nil, "", errors.New("no USB serial adapter found; is the vLinker plugged in? (use -port to choose one)")
		}
		port = matches[0]
	}
	bauds := []int{baud}
	if baud == 0 {
		bauds = []int{115200, 38400, 230400, 500000, 9600}
	}
	var lastErr error
	for _, b := range bauds {
		p, err := serial.Open(port, &serial.Mode{BaudRate: b})
		if err != nil {
			return nil, "", fmt.Errorf("open %s: %w", port, err)
		}
		if err := p.SetReadTimeout(100 * time.Millisecond); err != nil {
			p.Close()
			return nil, "", err
		}
		sp := serialPort{p}
		if probe(sp) {
			return sp, fmt.Sprintf("%s @ %d baud", port, b), nil
		}
		p.Close()
		lastErr = fmt.Errorf("no ELM327 prompt from %s at %d baud", port, b)
	}
	return nil, "", lastErr
}

// probe sends a bare CR and waits for the '>' prompt.
func probe(rw io.ReadWriter) bool {
	if _, err := rw.Write([]byte("\r")); err != nil {
		return false
	}
	r := bufio.NewReader(rw)
	deadline := time.Now().Add(1500 * time.Millisecond)
	for time.Now().Before(deadline) {
		b, err := r.ReadByte()
		if err != nil {
			continue
		}
		if b == '>' {
			return true
		}
	}
	return false
}

// serialPort reports read timeouts as io.EOF, which elm327.Conn treats as
// "nothing yet, keep waiting".
type serialPort struct{ serial.Port }

func (s serialPort) Read(p []byte) (int, error) {
	n, err := s.Port.Read(p)
	if n == 0 && err == nil {
		return 0, io.EOF
	}
	return n, err
}

type nopCloser struct{ io.ReadWriter }

func (nopCloser) Close() error { return nil }
