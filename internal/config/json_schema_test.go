package config

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestConfigJSONSchemaStable(t *testing.T) {
	allow := false
	presence := ConfigPresence{"text_only": {}}
	c := Config{
		AllowNoThinkBlock4Anthropic: &allow,
		LazyApply:                   &LazyApplyConfig{Presence: presence},
		Routes:                      []RouteRule{{Presence: presence}},
		ClassifierRoute:             &ClassifierRoute{Presence: presence},
		FastRoute:                   &FastRoute{Presence: presence},
		MultimodalFallback:          &MultimodalRoute{Presence: presence},
		SearchFallback:              &SearchRoute{Presence: presence},
	}
	out, err := json.Marshal(c)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if bytes.Contains(out, []byte("Presence")) {
		t.Errorf("internal presence metadata leaked into JSON: %s", out)
	}
	if !bytes.Contains(out, []byte(`"allowNoThinkBlock4Anthropic":false`)) {
		t.Errorf("allowNoThinkBlock4Anthropic key changed: %s", out)
	}
}

func TestLazyApplyRejectsPresenceMetadata(t *testing.T) {
	_, _, err := ParseConfig([]byte(`{"upstream":"http://u","lazyApply":{"Presence":{}}}`))
	if err == nil || !strings.Contains(err.Error(), `unknown field "Presence"`) {
		t.Fatalf("ParseConfig error = %v, want unknown Presence field", err)
	}
}
