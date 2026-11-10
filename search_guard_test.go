package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// searchGuardResultSSE builds one complete Anthropic content_block_start event for detector fixtures.
func searchGuardResultSSE(content string) string {
	return "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"web_search_tool_result\",\"tool_use_id\":\"srvtoolu_test\",\"content\":" + content + "}}\n\n"
}

func TestOnlyEmptySearchResults(t *testing.T) {
	nonEmpty := `[{"type":"web_search_tool_result_content","url":"https://example.com","title":"result"}]`
	tests := []struct {
		name string
		raw  string
		want bool
	}{
		{name: "one empty", raw: searchGuardResultSSE(`[]`), want: true},
		{name: "multiple empty", raw: searchGuardResultSSE(`[]`) + searchGuardResultSSE(`[ ]`), want: true},
		{name: "no result block", raw: "data: {\"type\":\"message_start\"}\n\n", want: false},
		{name: "non empty", raw: searchGuardResultSSE(nonEmpty), want: false},
		{name: "mixed empty and non empty", raw: searchGuardResultSSE(`[]`) + searchGuardResultSSE(nonEmpty), want: false},
		{name: "missing content", raw: "data: {\"type\":\"content_block_start\",\"content_block\":{\"type\":\"web_search_tool_result\"}}\n\n", want: false},
		{name: "null is not an empty array", raw: searchGuardResultSSE(`null`), want: false},
		{name: "object is not an empty array", raw: searchGuardResultSSE(`{}`), want: false},
		{name: "error object is not an empty array", raw: searchGuardResultSSE(`{"type":"web_search_tool_result_error","error_code":"unavailable"}`), want: false},
		{name: "malformed result JSON", raw: "data: {\"type\":\"content_block_start\",\"content_block\":{\"type\":\"web_search_tool_result\",\"content\":[]", want: false},
		{name: "empty array in prose does not count", raw: "data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"web_search_tool_result content []\"}}\n\n", want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := onlyEmptySearchResults([]byte(tc.raw)); got != tc.want {
				t.Errorf("onlyEmptySearchResults() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestSynthesizeSearchGuardSSE(t *testing.T) {
	usage := searchGuardUsage{input: 101, cacheRead: 23, cacheCreation: 7, output: 42}
	raw := synthesizeSearchGuardSSE(1222, "claude-fable-5-1", usage)
	text := string(raw)

	for _, want := range []string{
		`"id":"msg_proxy429_search_guard_1222"`,
		`"model":"claude-fable-5-1"`,
		`"input_tokens":101`,
		`"cache_read_input_tokens":23`,
		`"cache_creation_input_tokens":7`,
		`"output_tokens":42`,
		searchGuardFixedText,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("synthetic stream missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "web_search_tool_result") || strings.Contains(text, "thinking_delta") {
		t.Errorf("synthetic stream must contain only the fixed text block:\n%s", text)
	}

	var events []string
	var fixedText string
	var deltaUsage map[string]interface{}
	for _, line := range strings.Split(text, "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var ev map[string]interface{}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev); err != nil {
			t.Fatalf("invalid synthetic JSON line %q: %v", line, err)
		}
		typ, _ := ev["type"].(string)
		events = append(events, typ)
		if typ == "content_block_delta" {
			delta := ev["delta"].(map[string]interface{})
			fixedText += delta["text"].(string)
		}
		if typ == "message_delta" {
			deltaUsage = ev["usage"].(map[string]interface{})
		}
	}
	wantEvents := []string{"message_start", "content_block_start", "content_block_delta", "content_block_stop", "message_delta", "message_stop"}
	if fmt.Sprint(events) != fmt.Sprint(wantEvents) {
		t.Errorf("event sequence = %v, want %v", events, wantEvents)
	}
	if fixedText != searchGuardFixedText {
		t.Errorf("text = %q, want exact fixed text %q", fixedText, searchGuardFixedText)
	}
	if deltaUsage["output_tokens"] != float64(42) {
		t.Errorf("message_delta output_tokens = %v, want 42", deltaUsage["output_tokens"])
	}
}

func TestResolveSearchGuard(t *testing.T) {
	on, off := true, false
	tests := []struct {
		name     string
		route    *bool
		fallback *bool
		want     bool
	}{
		{name: "all unset", want: false},
		{name: "route on direct", route: &on, want: true},
		{name: "route off direct", route: &off, want: false},
		{name: "inherit fallback on", fallback: &on, want: true},
		{name: "inherit fallback off", fallback: &off, want: false},
		{name: "route on overrides fallback off", route: &on, fallback: &off, want: true},
		{name: "route off overrides fallback on", route: &off, fallback: &on, want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rr := &RouteRule{NoBullshitOnEmptyClaudeSearch: tc.route}
			sf := &SearchRoute{NoBullshitOnEmptyClaudeSearch: tc.fallback}
			if got := resolveSearchGuard(rr, sf); got != tc.want {
				t.Errorf("resolveSearchGuard() = %v, want %v", got, tc.want)
			}
		})
	}
	if resolveSearchGuard(nil, nil) {
		t.Error("nil routes must default to guard off")
	}
}

func TestSearchGuardJSONOptionName(t *testing.T) {
	var route RouteRule
	if err := json.Unmarshal([]byte(`{"noBullshitOnEmptyClaudeSearch":true}`), &route); err != nil {
		t.Fatalf("unmarshal route option: %v", err)
	}
	if route.NoBullshitOnEmptyClaudeSearch == nil || !*route.NoBullshitOnEmptyClaudeSearch {
		t.Fatal("noBullshitOnEmptyClaudeSearch:true did not enable the route guard")
	}

	var fallback SearchRoute
	if err := json.Unmarshal([]byte(`{"noBullshitOnEmptyClaudeSearch":false}`), &fallback); err != nil {
		t.Fatalf("unmarshal fallback option: %v", err)
	}
	if fallback.NoBullshitOnEmptyClaudeSearch == nil || *fallback.NoBullshitOnEmptyClaudeSearch {
		t.Fatal("explicit noBullshitOnEmptyClaudeSearch:false must remain distinguishable from unset")
	}
}

func TestSearchGuardMarkerInjection(t *testing.T) {
	on := true
	out, bodies, _, _ := runSearchE2EFull(t,
		RouteRule{Pattern: "claude-opus*", API: "sk-route", Model: "kimi-for-coding", NoBullshitOnEmptyClaudeSearch: &on},
		nil, true, searchSubagentReq)
	if len(bodies) != 1 {
		t.Fatalf("route upstream should receive one POST, got %d", len(bodies))
	}
	if !strings.Contains(string(bodies[0]), searchGuardInstruction) {
		t.Errorf("guard-only route should inject the short no-results constraint: %s", bodies[0])
	}
	checkPassthrough(t, out) // A non-empty result is not replaced.
}

func TestSearchGuardFallbackMarkerInjection(t *testing.T) {
	on := true
	_, bodies, _, _ := runSearchE2EFull(t,
		RouteRule{Pattern: "claude-opus*", API: "sk-route", Model: "ark-opus", NoSearch: true},
		&SearchRoute{API: "sk-sf", Model: "kimi-for-coding", NoBullshitOnEmptyClaudeSearch: &on},
		false, searchSubagentReq)
	if len(bodies) != 1 {
		t.Fatalf("fallback upstream should receive one POST, got %d", len(bodies))
	}
	if !strings.Contains(string(bodies[0]), searchGuardInstruction) {
		t.Errorf("unset route should inherit search_fallback guard injection: %s", bodies[0])
	}
}

func TestSearchGuardNoMarkerNoInjection(t *testing.T) {
	on := true
	body := `{"model":"claude-opus-4-8","stream":true,"tools":[{"type":"web_search_20250305","name":"web_search"}],"messages":[{"role":"user","content":"search without the subagent marker"}]}`
	_, bodies, _, _ := runSearchE2EFull(t,
		RouteRule{Pattern: "claude-opus*", API: "sk-route", Model: "kimi-for-coding", NoBullshitOnEmptyClaudeSearch: &on},
		nil, true, body)
	if len(bodies) != 1 {
		t.Fatalf("route upstream should receive one POST, got %d", len(bodies))
	}
	if strings.Contains(string(bodies[0]), searchGuardInstruction) {
		t.Errorf("an unmarked request must not receive guard injection: %s", bodies[0])
	}
}
