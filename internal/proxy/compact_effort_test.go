package proxy

import (
	"bytes"
	"strings"
	"testing"

	"proxy429/internal/config"
)

// compactAdaptiveHigh is a client context-compaction request (Claude Code compact prompt in the final user
// message) carrying adaptive thinking with an explicit effort.
const compactAdaptiveHigh = `{"model":"claude-opus-4-8","max_tokens":64,"stream":true,"thinking":{"type":"adaptive"},"output_config":{"effort":"high"},"messages":[{"role":"user","content":"CRITICAL: Respond with TEXT ONLY. Do NOT call any tools."}]}`

// TestCompactEffortBeatsRouteEffortE2E locks the precedence: a compaction request with compactEffort set gets
// that level on the wire even when the route pins max, the [from->to] badge shows the compact level, and the
// model column carries the compactEffort level in the [level] slot, replacing the overridden route [max].
func TestCompactEffortBeatsRouteEffortE2E(t *testing.T) {
	bodies, _, _, recent := runEffortE2E(t, func(routeURL, fbURL string) *config.Config {
		c := effortBaseConfig(routeURL)
		c.Routes[0].Model = "deepseek-test"
		c.CompactEffort = "low"
		return c
	}, compactAdaptiveHigh)
	if len(bodies) != 1 {
		t.Fatalf("upstream should receive one request, got %d", len(bodies))
	}
	up := string(bodies[0])
	if !strings.Contains(up, `"effort":"low"`) || strings.Contains(up, `"effort":"high"`) || strings.Contains(up, `"effort":"max"`) {
		t.Errorf("compactEffort must force low over both the client's high and the route's max: %s", up)
	}
	for _, sub := range []string{`"thinkFrom":"high"`, `"thinkTo":"low"`, `"compaction":true`} {
		if !strings.Contains(recent, sub) {
			t.Errorf("recent flight should carry %q: %s", sub, recent)
		}
	}
	models := recentModels(t, recent)
	if !hasModel(models, "claude-opus-4-8 -> [low] deepseek-test") {
		t.Errorf("the compactEffort level should ride the [level] slot before the target model, got %v", models)
	}
	for _, m := range models {
		if strings.Contains(m, "[max]") {
			t.Errorf("the overridden route [max] must not show on a compactEffort flight, got %q", m)
		}
	}
}

// TestCompactEffortWithoutRouteEffortE2E locks the independent half: compactEffort applies on a route that
// sets no effort at all.
func TestCompactEffortWithoutRouteEffortE2E(t *testing.T) {
	bodies, _, _, recent := runEffortE2E(t, func(routeURL, fbURL string) *config.Config {
		c := effortBaseConfig(routeURL)
		c.Routes[0].Effort = ""
		c.CompactEffort = "max"
		return c
	}, compactAdaptiveHigh)
	if len(bodies) != 1 {
		t.Fatalf("upstream should receive one request, got %d", len(bodies))
	}
	up := string(bodies[0])
	if !strings.Contains(up, `"effort":"max"`) || strings.Contains(up, `"effort":"high"`) {
		t.Errorf("compactEffort should force max over the client's high: %s", up)
	}
	for _, sub := range []string{`"thinkFrom":"high"`, `"thinkTo":"max"`, `"compaction":true`} {
		if !strings.Contains(recent, sub) {
			t.Errorf("recent flight should carry %q: %s", sub, recent)
		}
	}
	models := recentModels(t, recent)
	if !hasModel(models, "claude-opus-4-8 [max]") {
		t.Errorf("the compactEffort level should show in the [level] slot even without a route pin, got %v", models)
	}
}

// TestCompactEffortMidUsesWireMediumE2E locks the wire vocabulary: mid is written upstream as medium and the
// console carries the wire form too.
func TestCompactEffortMidUsesWireMediumE2E(t *testing.T) {
	bodies, _, _, recent := runEffortE2E(t, func(routeURL, fbURL string) *config.Config {
		c := effortBaseConfig(routeURL)
		c.CompactEffort = "mid"
		return c
	}, compactAdaptiveHigh)
	if len(bodies) != 1 {
		t.Fatalf("upstream should receive one request, got %d", len(bodies))
	}
	up := string(bodies[0])
	if !strings.Contains(up, `"effort":"medium"`) || strings.Contains(up, `"effort":"mid"`) || strings.Contains(up, `"effort":"max"`) {
		t.Errorf("compactEffort mid must reach the upstream as medium: %s", up)
	}
	for _, sub := range []string{`"thinkFrom":"high"`, `"thinkTo":"medium"`} {
		if !strings.Contains(recent, sub) {
			t.Errorf("recent flight should carry %q: %s", sub, recent)
		}
	}
	models := recentModels(t, recent)
	if !hasModel(models, "claude-opus-4-8 [medium]") {
		t.Errorf("the [level] slot should carry the wire form (mid reads medium), got %v", models)
	}
}

// TestCompactEffortUnsetKeepsRoutePinE2E locks the opt-out: without compactEffort, a compaction request keeps
// exactly the route pin behavior, including the model column's route [max] tag.
func TestCompactEffortUnsetKeepsRoutePinE2E(t *testing.T) {
	bodies, _, _, recent := runEffortE2E(t, func(routeURL, fbURL string) *config.Config {
		return effortBaseConfig(routeURL)
	}, compactAdaptiveHigh)
	if len(bodies) != 1 {
		t.Fatalf("upstream should receive one request, got %d", len(bodies))
	}
	up := string(bodies[0])
	if !strings.Contains(up, `"effort":"max"`) || strings.Contains(up, `"effort":"high"`) {
		t.Errorf("the route effort pin should still win without compactEffort: %s", up)
	}
	for _, sub := range []string{`"thinkFrom":"high"`, `"thinkTo":"max"`} {
		if !strings.Contains(recent, sub) {
			t.Errorf("recent flight should carry %q: %s", sub, recent)
		}
	}
	models := recentModels(t, recent)
	if !hasModel(models, "claude-opus-4-8 [max]") {
		t.Errorf("without compactEffort the route [max] tag should stay on the compaction row, got %v", models)
	}
}

// TestCompactEffortLeavesNormalRequestsAloneE2E locks the compaction-only scope: a normal adaptive request gets
// the route pin and keeps the model column's route [max] tag.
func TestCompactEffortLeavesNormalRequestsAloneE2E(t *testing.T) {
	bodies, _, _, recent := runEffortE2E(t, func(routeURL, fbURL string) *config.Config {
		c := effortBaseConfig(routeURL)
		c.CompactEffort = "low"
		return c
	}, effortAdaptiveHigh)
	if len(bodies) != 1 {
		t.Fatalf("upstream should receive one request, got %d", len(bodies))
	}
	up := string(bodies[0])
	if !strings.Contains(up, `"effort":"max"`) || strings.Contains(up, `"effort":"low"`) {
		t.Errorf("a non-compaction request must keep the route pin: %s", up)
	}
	models := recentModels(t, recent)
	if !hasModel(models, "claude-opus-4-8 [max]") {
		t.Errorf("the route [max] tag should stay on a normal request, got %v", models)
	}
}

// TestCompactEffortNonAdaptiveCompactDisplayOnlyE2E locks the adaptive-only gate: a compaction without a thinking
// field is forwarded byte-identical, yet the model column still shows the configured compactEffort level in the
// [level] slot, replacing the overridden route [max], even though no rewrite happened (the tag follows the
// compaction scope, not the rewritten-body flag).
func TestCompactEffortNonAdaptiveCompactDisplayOnlyE2E(t *testing.T) {
	bodies, _, _, recent := runEffortE2E(t, func(routeURL, fbURL string) *config.Config {
		c := effortBaseConfig(routeURL)
		c.CompactEffort = "high"
		return c
	}, compactionRequest)
	if len(bodies) != 1 {
		t.Fatalf("upstream should receive one request, got %d", len(bodies))
	}
	if !bytes.Equal(bodies[0], []byte(compactionRequest)) {
		t.Errorf("a non-adaptive compaction must pass through byte-identical:\n got: %s\nwant: %s", bodies[0], compactionRequest)
	}
	if !strings.Contains(recent, `"compaction":true`) {
		t.Errorf("recent flight should carry the compaction flag: %s", recent)
	}
	assertNoThinkRewriteBadge(t, recent)
	models := recentModels(t, recent)
	if !hasModel(models, "claude-opus-4-8 [high]") {
		t.Errorf("the compactEffort level should show in the [level] slot even without a rewrite, got %v", models)
	}
	for _, m := range models {
		if strings.Contains(m, "[max]") {
			t.Errorf("the overridden route [max] must not show on a compactEffort flight, got %q", m)
		}
	}
}

// TestCompactEffortNoRouteE2E locks the scope: a plain deployment with no routes at all still gets the
// compactEffort pin on a compaction request — the pin point is global, not inside the routes[] hit branch.
func TestCompactEffortNoRouteE2E(t *testing.T) {
	bodies, _, _, recent := runEffortE2E(t, func(routeURL, fbURL string) *config.Config {
		return &config.Config{Upstream: routeURL, MaxRetries: 0, TotalBudgetSec: 10, RecentSampleWindow: 5, CompactEffort: "low"}
	}, compactAdaptiveHigh)
	if len(bodies) != 1 {
		t.Fatalf("upstream should receive one request, got %d", len(bodies))
	}
	up := string(bodies[0])
	if !strings.Contains(up, `"effort":"low"`) || strings.Contains(up, `"effort":"high"`) {
		t.Errorf("compactEffort should force low over the client's high without any route: %s", up)
	}
	for _, sub := range []string{`"thinkFrom":"high"`, `"thinkTo":"low"`, `"compaction":true`} {
		if !strings.Contains(recent, sub) {
			t.Errorf("recent flight should carry %q: %s", sub, recent)
		}
	}
	models := recentModels(t, recent)
	if !hasModel(models, "claude-opus-4-8 [low]") {
		t.Errorf("the [level] slot should carry compactEffort without any route, got %v", models)
	}
}
