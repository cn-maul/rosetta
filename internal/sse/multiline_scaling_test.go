package sse

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

// An event assembled from many data: lines must not cost super-linear time,
// and its accounting must stay exact.
//
// The size guard used to recompute the running total by re-walking every part
// already stored, so one event with n lines did O(n^2) work. A wall-clock
// assertion is too flaky for CI (these are sub-millisecond scans), so this
// pins the observable contract instead: many-line events decode to exactly the
// right bytes, the counter does not carry over between events, and the cap
// fires at the same threshold no matter how the bytes were split across lines.
func TestManyLineEventAccountingIsExact(t *testing.T) {
	const lines = 2000
	build := func(events int) []byte {
		var b bytes.Buffer
		for e := 0; e < events; e++ {
			for i := 0; i < lines; i++ {
				b.WriteString("data: ")
				b.Write(bytes.Repeat([]byte{'x'}, 16))
				b.WriteString("\n")
			}
			b.WriteString("\n")
		}
		return b.Bytes()
	}

	// Three consecutive many-line events: each must decode identically, which
	// it only can if the running total is both accurate and reset per event.
	sc := New(bytes.NewReader(build(3)))
	want := lines*16 + (lines - 1) // payload plus the LF separators Join inserts
	for e := 0; e < 3; e++ {
		ev, err := sc.Next()
		if err != nil {
			t.Fatalf("event %d: %v", e, err)
		}
		if len(ev.Data) != want {
			t.Fatalf("event %d: data len = %d, want %d", e, len(ev.Data), want)
		}
	}
	if _, err := sc.Next(); err == nil {
		t.Fatal("expected the stream to end after 3 events")
	}

	// The cap must trip at the same total whether the payload arrives as one
	// huge line or as many small ones: a counter that drifted, over- or
	// under-counted would trip at a different size.
	oneBigLine := []byte("data: " + strings.Repeat("x", maxEventBytes+64) + "\n\n")
	if _, err := New(bytes.NewReader(oneBigLine)).Next(); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("single oversized line: err = %v, want ErrTooLarge", err)
	}
	var manySmall bytes.Buffer
	for written := 0; written < maxEventBytes+1024; written += 4096 {
		manySmall.WriteString("data: ")
		manySmall.Write(bytes.Repeat([]byte{'x'}, 4096))
		manySmall.WriteString("\n")
	}
	if _, err := New(bytes.NewReader(manySmall.Bytes())).Next(); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("many-line oversized event: err = %v, want ErrTooLarge", err)
	}
}

// The incremental counter must enforce exactly the same cap as the previous
// re-walk, and must be reset between events so a large event does not poison
// the next one.
func TestSizeCapCountsAcrossLinesAndResetsPerEvent(t *testing.T) {
	line := "data: " + strings.Repeat("x", 1<<20) + "\n"

	// Over the 16MiB cap: rejected.
	var over bytes.Buffer
	for i := 0; i < 17; i++ {
		over.WriteString(line)
	}
	if _, err := New(bytes.NewReader(over.Bytes())).Next(); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("err = %v, want ErrTooLarge", err)
	}

	// Under the cap: accepted, and the data is intact.
	var under bytes.Buffer
	for i := 0; i < 10; i++ {
		under.WriteString(line)
	}
	under.WriteString("\n")
	ev, err := New(bytes.NewReader(under.Bytes())).Next()
	if err != nil {
		t.Fatalf("under-limit event rejected: %v", err)
	}
	if want := 10*(1<<20) + 9; len(ev.Data) != want {
		// 10 data lines joined with LF => 10 MiB of payload plus the 9
		// newline separators bytes.Join inserts.
		t.Fatalf("data len = %d, want %d", len(ev.Data), want)
	}

	// A second event of the same size must still be accepted — the counter
	// has to be cleared by take(), not merely by reaching a blank line.
	var two bytes.Buffer
	for i := 0; i < 10; i++ {
		two.WriteString(line)
	}
	two.WriteString("\n")
	two.WriteString(line)
	two.WriteString("\n")
	sc := New(bytes.NewReader(two.Bytes()))
	if _, err := sc.Next(); err != nil {
		t.Fatalf("first event: %v", err)
	}
	if _, err := sc.Next(); err != nil {
		t.Fatalf("second event rejected — counter not reset between events: %v", err)
	}
}
