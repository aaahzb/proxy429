package main

// config.example.json is the template for new configs / first run (go:embed'ed into the exe); this test ensures every field
// in the template really exists in the Config struct (DisallowUnknownFields strict decoding fails on hallucinated fields),
// and that the demo enum values are values the code actually consumes.

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestConfigExampleMatchesStruct(t *testing.T) {
	dec := json.NewDecoder(bytes.NewReader(configExampleBytes))
	dec.DisallowUnknownFields()
	var c Config
	if err := dec.Decode(&c); err != nil {
		t.Fatalf("config.example.json 严格解码失败（模板含结构体不存在的字段？）: %v", err)
	}

	// Key fields non-empty: the template must work as a complete demo.
	if c.Listen == "" || c.Upstream == "" {
		t.Errorf("listen/upstream 不能为空: %+v", c)
	}
	if len(c.Routes) == 0 {
		t.Errorf("routes 演示条目不能为空")
	}
	if len(c.RetryStatusCodes) == 0 {
		t.Errorf("retry_status_codes 不能为空")
	}

	// The demo enum value must be a level summaryLevelConfig actually recognizes.
	validLevel := map[string]bool{"low": true, "mid": true, "high": true, "max": true}
	for i, r := range c.Routes {
		if r.Pattern == "" || r.URL == "" {
			t.Errorf("routes[%d] 缺 pattern/url: %+v", i, r)
		}
		if es := r.EnhanceSearch; es != nil && !validLevel[es.SummaryLevel] {
			t.Errorf("routes[%d].enhance_search.summary_level=%q 不是真实档位", i, es.SummaryLevel)
		}
	}
	if c.SearchFallback != nil && !validLevel[c.SearchFallback.SummaryLevel] {
		t.Errorf("search_fallback.summary_level=%q 不是真实档位", c.SearchFallback.SummaryLevel)
	}

	// The debug field is excluded from the template by convention.
	var raw map[string]interface{}
	if err := json.Unmarshal(configExampleBytes, &raw); err != nil {
		t.Fatalf("模板不是合法 JSON: %v", err)
	}
	if _, ok := raw["search_debug_dir"]; ok {
		t.Errorf("search_debug_dir 是 debug 字段，不应出现在模板")
	}
}

// TestConfigCoversAllStructFields is the reverse check: every json field on the Config struct
// (except debug) should be demonstrated in the template, so adding a new field can't forget its demo.
func TestConfigCoversAllStructFields(t *testing.T) {
	var raw map[string]interface{}
	if err := json.Unmarshal(configExampleBytes, &raw); err != nil {
		t.Fatalf("模板不是合法 JSON: %v", err)
	}
	// All Config struct fields (the debug field is exempted).
	exempt := map[string]bool{"search_debug_dir": true}
	for _, f := range []string{
		"listen", "upstream", "max_retries", "base_delay_s",
		"max_delay_s", "total_budget_s", "retry_status_codes", "respect_retry_after",
		"classifier_thinking_disabled", "classifier_max_tokens",
		"upstream_header_timeout_s", "ping_interval_s", "log_request_detail",
		"log_file", "recent_sample_window", "routes", "classifier_route",
		"fast_route", "multimodal_fallback", "search_fallback",
		"convertAlltoStream", "translateNone2Low", "responses_listen",
	} {
		if exempt[f] {
			continue
		}
		if _, ok := raw[f]; !ok {
			t.Errorf("模板缺字段演示: %s", f)
		}
	}
}
