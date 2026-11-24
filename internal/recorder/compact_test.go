package recorder

import "testing"

// TestCompactSave pins the console's compaction-result contract: the total input context
// (input + cache_read + cache_creation) versus the summary's output tokens, plus the share compressed away.
func TestCompactSave(t *testing.T) {
	tests := []struct {
		name                         string
		in, cacheRead, cacheCreation int64
		out                          int64
		want                         string
	}{
		{name: "170k example", in: 1000, cacheRead: 169000, out: 20000, want: "170k->20k, 88%"},
		{name: "sub-thousand plain numbers", in: 500, out: 100, want: "500->100, 80%"},
		{name: "thousands with one decimal", in: 2000, out: 500, want: "2.0k->500, 75%"},
		{name: "ten thousands without decimals", in: 10000, out: 1000, want: "10k->1.0k, 90%"},
		{name: "millions", in: 2000000, out: 200000, want: "2.0M->200k, 90%"},
		{name: "cache creation counts as context", cacheRead: 1000, cacheCreation: 9000, out: 1000, want: "10k->1.0k, 90%"},
		{name: "no total", want: ""},
		{name: "no output", in: 1000, want: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := CompactSave(tc.in, tc.cacheRead, tc.cacheCreation, tc.out)
			if got != tc.want {
				t.Errorf("CompactSave(%d, %d, %d, %d) = %q, want %q", tc.in, tc.cacheRead, tc.cacheCreation, tc.out, got, tc.want)
			}
		})
	}
}
