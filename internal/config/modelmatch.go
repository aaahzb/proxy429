package config

import "strings"

// MatchModel reports whether a model name matches a pattern, where '*' matches any run of characters.
func MatchModel(pattern, name string) bool {
	parts := strings.Split(pattern, "*")
	if len(parts) == 1 {
		return pattern == name
	}
	if !strings.HasPrefix(name, parts[0]) {
		return false
	}
	name = name[len(parts[0]):]
	for i := 1; i < len(parts)-1; i++ {
		idx := strings.Index(name, parts[i])
		if idx < 0 {
			return false
		}
		name = name[idx+len(parts[i]):]
	}
	return strings.HasSuffix(name, parts[len(parts)-1])
}

// MatchRouteRule returns the first route matching model (skipping reserved-name patterns), nil on no hit.
// The main handler's route loop and the translation-time prediction share this single match point.
func MatchRouteRule(c *Config, model string) *RouteRule {
	for i := range c.Routes {
		rr := &c.Routes[i]
		if IsReservedRoutePattern(rr.Pattern) {
			continue
		}
		if MatchModel(rr.Pattern, model) {
			return rr
		}
	}
	return nil
}
