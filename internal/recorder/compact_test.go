package recorder

import "testing"

// TestCompactResult pins the console's compaction-result contract: the total input context
// (input + cache_read + cache_creation) versus the summary's output tokens, as the badge percent
// and the full hover description.
func TestCompactResult(t *testing.T) {
	tests := []struct {
		name                         string
		in, cacheRead, cacheCreation int64
		out                          int64
		wantPct, wantDetail          string
	}{
		{name: "170k example", in: 1000, cacheRead: 169000, out: 20000, wantPct: "88%", wantDetail: "170k->20k, 88%"},
		{name: "sub-thousand plain numbers", in: 500, out: 100, wantPct: "80%", wantDetail: "500->100, 80%"},
		{name: "thousands with one decimal", in: 2000, out: 500, wantPct: "75%", wantDetail: "2.0k->500, 75%"},
		{name: "ten thousands without decimals", in: 10000, out: 1000, wantPct: "90%", wantDetail: "10k->1.0k, 90%"},
		{name: "millions", in: 2000000, out: 200000, wantPct: "90%", wantDetail: "2.0M->200k, 90%"},
		{name: "cache creation counts as context", cacheRead: 1000, cacheCreation: 9000, out: 1000, wantPct: "90%", wantDetail: "10k->1.0k, 90%"},
		{name: "summary larger than context floors at 0", in: 2000, out: 3000, wantPct: "0%", wantDetail: "2.0k->3.0k, 0%"},
		{name: "no total"},
		{name: "no output", in: 1000},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			pct, detail := CompactResult(tc.in, tc.cacheRead, tc.cacheCreation, tc.out)
			if pct != tc.wantPct || detail != tc.wantDetail {
				t.Errorf("CompactResult(%d, %d, %d, %d) = %q, %q, want %q, %q", tc.in, tc.cacheRead, tc.cacheCreation, tc.out, pct, detail, tc.wantPct, tc.wantDetail)
			}
		})
	}
}
