package serialhelper

import (
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

// chunkReader replays scripted read results, mimicking tarm/serial where an
// expired ReadTimeout yields (0, io.EOF) rather than (0, nil).
type chunkReader struct {
	chunks []readResult
	pos    int
}

type readResult struct {
	data string
	err  error
}

func (r *chunkReader) Read(p []byte) (int, error) {
	if r.pos >= len(r.chunks) {
		return 0, io.EOF
	}
	c := r.chunks[r.pos]
	r.pos++
	if c.data == "" {
		return 0, c.err
	}
	n := copy(p, c.data)
	return n, c.err
}

func TestReadUntilMarkersTreatsTimeoutEOFAsIdle(t *testing.T) {
	dump := strings.Join([]string{
		"m12",
		"",
		"10: 00 00 03 1e ff ff ff ff ff ff ff ff ff ff ff ff",
		"",
		"O^K",
	}, "\r\n") + "\r\n"

	// Node is slow to start, then pauses between lines: every pause surfaces as
	// io.EOF from the serial port.
	r := &chunkReader{chunks: []readResult{
		{err: io.EOF},
		{err: io.EOF},
		{data: dump[:20]},
		{err: io.EOF},
		{data: dump[20:]},
		{err: io.EOF},
		{err: io.EOF},
		{err: io.EOF},
	}}

	got, first, err := readUntilMarkers(r, 2*time.Second, []byte("O^K"), []byte("E^RROR"))
	if err != nil {
		t.Fatalf("readUntilMarkers: %v", err)
	}
	if string(got) != dump {
		t.Fatalf("response = %q, want %q", got, dump)
	}
	if first.IsZero() {
		t.Fatal("expected a first-byte timestamp")
	}
}

func TestReadUntilMarkersReturnsRealErrors(t *testing.T) {
	want := errors.New("port disappeared")
	r := &chunkReader{chunks: []readResult{{err: want}}}

	if _, _, err := readUntilMarkers(r, time.Second, []byte("O^K")); !errors.Is(err, want) {
		t.Fatalf("err = %v, want %v", err, want)
	}
}

func TestReadUntilMarkersStopsAtTimeoutWhenSilent(t *testing.T) {
	r := &chunkReader{}

	start := time.Now()
	got, first, err := readUntilMarkers(r, 150*time.Millisecond, []byte("O^K"))
	if err != nil {
		t.Fatalf("readUntilMarkers: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("response = %q, want empty", got)
	}
	if !first.IsZero() {
		t.Fatal("expected zero first-byte timestamp when nothing arrived")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("took %s, expected to stop near the timeout", elapsed)
	}
}

func TestDrainSerialIgnoresTimeoutEOF(t *testing.T) {
	r := &chunkReader{chunks: []readResult{
		{data: "leftover"},
		{err: io.EOF},
		{data: "more"},
	}}

	// Drain stops at the first quiet read, so leftover bytes before it count.
	if got := drainSerial(r, 200*time.Millisecond); got != len("leftover") {
		t.Fatalf("drained %d bytes, want %d", got, len("leftover"))
	}
}
