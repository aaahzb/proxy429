package config

// config.example.json is the template for new configs / first run (go:embed'ed into the exe); this test ensures every field
// in the template really exists in the Config struct (DisallowUnknownFields strict decoding fails on hallucinated fields),
// and that the demo enum values are values the code actually consumes.

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestConfigExampleMatchesStruct(t *testing.T) {
	dec := json.NewDecoder(bytes.NewReader(ConfigExampleBytes))
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
	validEffort := map[string]bool{"": true, "low": true, "mid": true, "high": true, "max": true}
	for i, r := range c.Routes {
		if r.Pattern == "" || r.URL == "" {
			t.Errorf("routes[%d] 缺 pattern/url: %+v", i, r)
		}
		if es := r.EnhanceSearch; es != nil && !validLevel[es.SummaryLevel] {
			t.Errorf("routes[%d].enhance_search.summary_level=%q 不是真实档位", i, es.SummaryLevel)
		}
		if !validEffort[r.Effort] {
			t.Errorf("routes[%d].effort=%q 不是合法枚举", i, r.Effort)
		}
	}
	if c.SearchFallback != nil && !validLevel[c.SearchFallback.SummaryLevel] {
		t.Errorf("search_fallback.summary_level=%q 不是真实档位", c.SearchFallback.SummaryLevel)
	}

	// The claudeSearch* demo values (on search_fallback and routes entries alike) must be enums ValidateConfigEnums actually accepts.
	validSearchThink := map[string]bool{"": true, "off": true, "low": true, "mid": true, "high": true, "max": true}
	validSearchSummary := map[string]bool{"": true, "noForce": true, "forceLow": true, "forceMid": true, "forceHigh": true}
	if c.SearchFallback != nil {
		if !validSearchThink[c.SearchFallback.ClaudeSearchThinking] {
			t.Errorf("search_fallback.claudeSearchThinking=%q 不是合法枚举", c.SearchFallback.ClaudeSearchThinking)
		}
		if !validSearchSummary[c.SearchFallback.ClaudeSearchSummary] {
			t.Errorf("search_fallback.claudeSearchSummary=%q 不是合法枚举", c.SearchFallback.ClaudeSearchSummary)
		}
	}
	for i, r := range c.Routes {
		if !validSearchThink[r.ClaudeSearchThinking] {
			t.Errorf("routes[%d].claudeSearchThinking=%q 不是合法枚举", i, r.ClaudeSearchThinking)
		}
		if !validSearchSummary[r.ClaudeSearchSummary] {
			t.Errorf("routes[%d].claudeSearchSummary=%q 不是合法枚举", i, r.ClaudeSearchSummary)
		}
	}

	// lazyApply demo values must be enums the runtime actually accepts.
	if la := c.LazyApply; la != nil {
		if !validSearchThink[la.ClaudeSearchThinking] {
			t.Errorf("lazyApply.claudeSearchThinking=%q 不是合法枚举", la.ClaudeSearchThinking)
		}
		if la.ClaudeSearchSummary != "" && !validSearchSummary[la.ClaudeSearchSummary] {
			t.Errorf("lazyApply.claudeSearchSummary=%q 不是合法枚举", la.ClaudeSearchSummary)
		}
	}

	// The debug field is excluded from the template by convention.
	var raw map[string]interface{}
	if err := json.Unmarshal(ConfigExampleBytes, &raw); err != nil {
		t.Fatalf("模板不是合法 JSON: %v", err)
	}
	if _, ok := raw["search_debug_dir"]; ok {
		t.Errorf("search_debug_dir 是 debug 字段，不应出现在模板")
	}
}

// TestConfigCoversAllStructFields is the reverse check: every json field on the Config struct
// should be demonstrated in the template, so adding a new field can't forget its demo. The field
// list comes from the struct tags themselves — a hand list drifts silently as fields come and go.
func TestConfigCoversAllStructFields(t *testing.T) {
	var raw map[string]interface{}
	if err := json.Unmarshal(ConfigExampleBytes, &raw); err != nil {
		t.Fatalf("模板不是合法 JSON: %v", err)
	}
	typ := reflect.TypeOf(Config{})
	for i := 0; i < typ.NumField(); i++ {
		name := strings.Split(typ.Field(i).Tag.Get("json"), ",")[0]
		if name == "" || name == "-" {
			continue
		}
		if _, ok := raw[name]; !ok {
			t.Errorf("模板缺字段演示: %s", name)
		}
	}
}
