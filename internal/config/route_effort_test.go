package config

import (
	"strings"
	"testing"
)

// TestValidateRouteEffortEnum locks the route-level effort enum: the four levels and an unset value load,
// anything else fails at parse time naming the route and the field.
func TestValidateRouteEffortEnum(t *testing.T) {
	for _, ok := range []string{"", "low", "mid", "high", "max"} {
		body := `{"upstream":"http://u","routes":[{"pattern":"x","url":"http://r","effort":"` + ok + `"}]}`
		if _, _, err := ParseConfig([]byte(body)); err != nil {
			t.Errorf("effort=%q should load, got %v", ok, err)
		}
	}
	body := `{"upstream":"http://u","routes":[{"pattern":"x","url":"http://r","effort":"bogus"}]}`
	_, _, err := ParseConfig([]byte(body))
	if err == nil || !strings.Contains(err.Error(), "effort") || !strings.Contains(err.Error(), `pattern "x"`) {
		t.Errorf("effort=bogus should fail naming the route and field, got %v", err)
	}
}
