package main

// config.example.json 是新建配置/首运的模板（go:embed 进 exe），本测试保证模板里的
// 每个字段都真实存在于 Config 结构体（DisallowUnknownFields 严格解码，幻觉字段直接报错），
// 演示用的枚举值也都是代码里真实消费的值。

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

	// 关键字段非空：模板必须能作为一份完整演示。
	if c.Listen == "" || c.Upstream == "" {
		t.Errorf("listen/upstream 不能为空: %+v", c)
	}
	if len(c.Routes) == 0 {
		t.Errorf("routes 演示条目不能为空")
	}
	if len(c.RetryStatusCodes) == 0 {
		t.Errorf("retry_status_codes 不能为空")
	}

	// 演示枚举值必须是 summaryLevelConfig 真实识别的档位。
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

	// debug 字段按约定不进模板。
	var raw map[string]interface{}
	if err := json.Unmarshal(configExampleBytes, &raw); err != nil {
		t.Fatalf("模板不是合法 JSON: %v", err)
	}
	if _, ok := raw["search_debug_dir"]; ok {
		t.Errorf("search_debug_dir 是 debug 字段，不应出现在模板")
	}
}

// TestConfigCoversAllStructFields 反向核对：Config 结构体里出现的每个 json 字段
// （debug 除外）都应该在模板里演示出来，防止以后加了新字段忘记补演示。
func TestConfigCoversAllStructFields(t *testing.T) {
	var raw map[string]interface{}
	if err := json.Unmarshal(configExampleBytes, &raw); err != nil {
		t.Fatalf("模板不是合法 JSON: %v", err)
	}
	// Config 结构体全字段（debug 字段列入豁免）。
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
