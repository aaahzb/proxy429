package config

import "testing"

func TestMatchModel(t *testing.T) {
	cases := []struct {
		pattern string
		name    string
		want    bool
	}{
		{"gpt-5*", "gpt-5-codex", true},
		{"gpt-5*", "gpt-4o", false},
		{"*codex", "gpt-5-codex", true},
		{"*codex", "codex-mini", false},
		{"gpt-*-codex", "gpt-5-high-codex", true},
		{"gpt-*-codex", "gpt-5-high", false},
		{"gpt-5-codex", "gpt-5-codex", true},
		{"gpt-5-codex", "gpt-5-codex-mini", false},
		{"claude-opus*", "claude-opus-4-8", true},
		{"claude-opus*", "claude-sonnet-5", false},
		{"*opus", "claude-opus", true},
		{"*opus", "claude-sonnet", false},
		{"a*b*c", "axxxbyyyc", true},
		{"a*b*c", "abc", true},
		{"a*b*c", "ac", false}, // the middle segment b must appear
		{"exact", "exact", true},
		{"exact", "exact2", false},
		{"*", "anything", true},
	}
	for _, tc := range cases {
		if got := MatchModel(tc.pattern, tc.name); got != tc.want {
			t.Errorf("MatchModel(%q, %q) = %v, want %v", tc.pattern, tc.name, got, tc.want)
		}
	}
}

func TestMatchRouteRule(t *testing.T) {
	c := &Config{Routes: []RouteRule{
		{Pattern: "Fallback", URL: "http://reserved"},
		{Pattern: "pre*", URL: "http://pre"},
		{Pattern: "*fable*", URL: "http://kimi"},
	}}
	cases := []struct {
		model string
		want  string
	}{
		{"prefix-model", "http://pre"},    // first ordered hit wins
		{"claude-fable-5", "http://kimi"}, // wildcard hit past the non-matching head route
		{"Fallback", ""},                  // reserved-name patterns never match
		{"gpt-5", ""},                     // no hit
	}
	for _, tc := range cases {
		rr := MatchRouteRule(c, tc.model)
		got := ""
		if rr != nil {
			got = rr.URL
		}
		if got != tc.want {
			t.Errorf("MatchRouteRule(%q) = %q, want %q", tc.model, got, tc.want)
		}
	}
}
