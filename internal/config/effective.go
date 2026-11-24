package config

// ResolveSearchBehavior computes the effective summary-injection level and thinking-rewrite level for a standalone
// search sub-agent request handled by sf (the real search_fallback with any route-level claudeSearch* overrides merged
// in per field, or one synthesized from a route's enhance_search legacy fields plus its claudeSearch* fields).
// The new claudeSearch* fields win; the legacy summary_mode/summary_level/summary_thinking triple applies only when
// the corresponding new field is unset.
// sf: the search route in effect (nil tolerated). Returns (injectLevel ""/low/mid/high/max, thinkLevel ""/off/low/mid/high/max).
func ResolveSearchBehavior(sf *SearchRoute) (injectLevel, thinkLevel string) {
	if sf == nil {
		return "", ""
	}
	switch sf.ClaudeSearchSummary {
	case "forceLow":
		injectLevel = "low"
	case "forceMid":
		injectLevel = "mid"
	case "forceHigh":
		injectLevel = "high"
	case "noForce":
		// Explicit opt-out: beats summary_mode, nothing injected.
	default: // Unset: legacy summary_mode governs
		if sf.SummaryMode {
			injectLevel = sf.SummaryLevel
			if injectLevel == "" {
				injectLevel = "low"
			}
		}
	}
	if sf.ClaudeSearchThinking != "" {
		thinkLevel = sf.ClaudeSearchThinking
	} else if sf.SummaryThinking {
		thinkLevel = "low"
	}
	return injectLevel, thinkLevel
}

// EffectiveSearchGuard resolves the guard with lazyApply materialized: route local > route lazy >
// fallback local > fallback lazy > false. Defaults never arm the guard without a matched route or
// fallback object, so the unmatched default upstream stays unchanged. An object's own noLazyApply
// removes only that object's inherited layer; an explicit local value always wins.
func EffectiveSearchGuard(c *Config, route *RouteRule, fallback *SearchRoute) bool {
	// lazyApply behaves as if the key were written into every supported object. Resolve the route
	// first (local then lazy), so a route-level default keeps the existing route-over-fallback
	// precedence; a route noLazyApply removes only the inherited layer. The fallback resolves the
	// same way, and an unmatched request (both nil) never arms the guard.
	if route != nil {
		if route.Presence.Written("noBullshitOnEmptyClaudeSearch", route.NoBullshitOnEmptyClaudeSearch != nil) {
			return route.NoBullshitOnEmptyClaudeSearch != nil && *route.NoBullshitOnEmptyClaudeSearch
		}
		if la := LazyApplyFor(c, route.NoLazyApply); la != nil && la.Presence.Written("noBullshitOnEmptyClaudeSearch", la.NoBullshitOnEmptyClaudeSearch != nil) {
			return la.NoBullshitOnEmptyClaudeSearch != nil && *la.NoBullshitOnEmptyClaudeSearch
		}
	}
	if fallback != nil {
		if fallback.Presence.Written("noBullshitOnEmptyClaudeSearch", fallback.NoBullshitOnEmptyClaudeSearch != nil) {
			return fallback.NoBullshitOnEmptyClaudeSearch != nil && *fallback.NoBullshitOnEmptyClaudeSearch
		}
		if la := LazyApplyFor(c, fallback.NoLazyApply); la != nil && la.Presence.Written("noBullshitOnEmptyClaudeSearch", la.NoBullshitOnEmptyClaudeSearch != nil) {
			return la.NoBullshitOnEmptyClaudeSearch != nil && *la.NoBullshitOnEmptyClaudeSearch
		}
	}
	return false
}
