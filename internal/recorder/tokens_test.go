package recorder

import "testing"

// TestFmtTokens pins the console's token in/out column contract: total input / output in the
// compact k/M convention, with "-" for a side that was never received (a stream cancelled before
// the usage delta shows "107k/-") and a bare "-" when neither side is known.
func TestFmtTokens(t *testing.T) {
	tests := []struct {
		name    string
		in      int64
		inSeen  bool
		out     int64
		outSeen bool
		want    string
	}{
		{name: "cancelled before usage delta", in: 107000, inSeen: true, want: "107k/-"},
		{name: "completed", in: 107000, inSeen: true, out: 4, outSeen: true, want: "107k/4"},
		{name: "neither known", want: "-"},
		{name: "output only", out: 7, outSeen: true, want: "-/7"},
		{name: "real zero output", in: 18, inSeen: true, out: 0, outSeen: true, want: "18/0"},
		{name: "input cache total", in: 1000, inSeen: true, out: 1200000, outSeen: true, want: "1.0k/1.2M"},
		{name: "ten thousands drop decimals", in: 107000, inSeen: true, out: 10000, outSeen: true, want: "107k/10k"},
		{name: "million boundary rounds up", in: 999500, inSeen: true, want: "1.0M/-"},
		{name: "just below the million boundary", in: 999499, inSeen: true, want: "999k/-"},
		{name: "ten-thousand boundary drops decimals", in: 9950, inSeen: true, want: "10k/-"},
		{name: "just below ten thousand keeps one decimal", in: 9949, inSeen: true, want: "9.9k/-"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := FmtTokens(tc.in, tc.inSeen, tc.out, tc.outSeen); got != tc.want {
				t.Errorf("FmtTokens(%d, %v, %d, %v) = %q, want %q", tc.in, tc.inSeen, tc.out, tc.outSeen, got, tc.want)
			}
		})
	}
}

// TestNoteLiveUsage pins the live display updates decoded from usage events: input appears once
// any component holds a real value (an all-zero ARK message_start is skipped), output only once the
// terminal delta arrived, and a zero-valued Flight needs no initialization.
func TestNoteLiveUsage(t *testing.T) {
	var f Flight
	if got := FmtTokens(f.LiveTokens()); got != "-" {
		t.Fatalf("fresh flight = %q, want -", got)
	}
	f.NoteLiveUsage(0, 0, 0, 1, false) // ARK's all-zero message_start: nothing known yet
	if got := FmtTokens(f.LiveTokens()); got != "-" {
		t.Errorf("all-zero start = %q, want -", got)
	}
	f.NoteLiveUsage(10, 5, 3, 1, false) // message_start with the real split: input known, output not
	if got := FmtTokens(f.LiveTokens()); got != "18/-" {
		t.Errorf("after message_start = %q, want 18/-", got)
	}
	f.NoteLiveUsage(10, 5, 3, 0, true) // terminal delta with output 0: a real zero, not unknown
	if got := FmtTokens(f.LiveTokens()); got != "18/0" {
		t.Errorf("after delta = %q, want 18/0", got)
	}
	f.ResetLiveUsage() // a retried attempt starts over: both sides unseen again
	if got := FmtTokens(f.LiveTokens()); got != "-" {
		t.Errorf("after reset = %q, want -", got)
	}
	f.NoteLiveUsage(0, 0, 0, 20, true) // second attempt's tail only: input missing, output fresh
	if got := FmtTokens(f.LiveTokens()); got != "-/20" {
		t.Errorf("second attempt = %q, want -/20 (no stale first-attempt input)", got)
	}
}
