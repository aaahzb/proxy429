package config

// LazyApplyFor returns the defaults object that may apply to one route-like object. A nil config or
// noLazyApply:true makes the object ignore lazyApply entirely; the object's own fields stay authoritative.
func LazyApplyFor(c *Config, optOut bool) *LazyApplyConfig {
	if c == nil || optOut {
		return nil
	}
	return c.LazyApply
}

// EffectiveRouteTextOnly resolves routes[].text_only: explicit local > lazyApply > false.
func EffectiveRouteTextOnly(c *Config, rr *RouteRule) bool {
	if rr == nil {
		return false
	}
	if rr.Presence.Written("text_only", rr.TextOnly) {
		return rr.TextOnly
	}
	if la := LazyApplyFor(c, rr.NoLazyApply); la != nil && la.Presence.Written("text_only", la.TextOnly) {
		return la.TextOnly
	}
	return false
}

// EffectiveRouteNoSearch resolves routes[].no_search: explicit local > lazyApply > false.
func EffectiveRouteNoSearch(c *Config, rr *RouteRule) bool {
	if rr == nil {
		return false
	}
	if rr.Presence.Written("no_search", rr.NoSearch) {
		return rr.NoSearch
	}
	if la := LazyApplyFor(c, rr.NoLazyApply); la != nil && la.Presence.Written("no_search", la.NoSearch) {
		return la.NoSearch
	}
	return false
}

// EffectiveRouteThinking resolves routes[].thinking: explicit local > lazyApply > "".
func EffectiveRouteThinking(c *Config, rr *RouteRule) string {
	if rr == nil {
		return ""
	}
	if rr.Presence.Written("thinking", rr.Thinking != "") {
		return rr.Thinking
	}
	if la := LazyApplyFor(c, rr.NoLazyApply); la != nil && la.Presence.Written("thinking", la.Thinking != "") {
		return la.Thinking
	}
	return ""
}

// EffectiveConvertOff2Low resolves convertOff2Low for one route-like object: explicit local > lazyApply > "".
// localWritten must come from that object's key-presence metadata (or non-zero convention for Go literals).
func EffectiveConvertOff2Low(c *Config, local string, localWritten, optOut bool) string {
	if localWritten {
		return local
	}
	if la := LazyApplyFor(c, optOut); la != nil && la.Presence.Written("convertOff2Low", la.ConvertOff2Low != "") {
		return la.ConvertOff2Low
	}
	return ""
}

// EffectiveDirectSearchSettings builds the SearchRoute-shaped view used by ResolveSearchBehavior for a
// direct route. Every field is resolved independently: a written local value wins, otherwise lazyApply,
// otherwise the built-in zero. enhance_search is atomic: a local object (or explicit null) wins whole,
// so lazyApply never partially merges into a local enhance object.
func EffectiveDirectSearchSettings(c *Config, rr *RouteRule) *SearchRoute {
	if rr == nil {
		return &SearchRoute{}
	}
	la := LazyApplyFor(c, rr.NoLazyApply)
	synth := &SearchRoute{}
	if rr.Presence.Written("claudeSearchSummary", rr.ClaudeSearchSummary != "") {
		synth.ClaudeSearchSummary = rr.ClaudeSearchSummary
	} else if la != nil && la.Presence.Written("claudeSearchSummary", la.ClaudeSearchSummary != "") {
		synth.ClaudeSearchSummary = la.ClaudeSearchSummary
	}
	if rr.Presence.Written("claudeSearchThinking", rr.ClaudeSearchThinking != "") {
		synth.ClaudeSearchThinking = rr.ClaudeSearchThinking
	} else if la != nil && la.Presence.Written("claudeSearchThinking", la.ClaudeSearchThinking != "") {
		synth.ClaudeSearchThinking = la.ClaudeSearchThinking
	}
	enhance := rr.EnhanceSearch
	if !rr.Presence.Written("enhance_search", rr.EnhanceSearch != nil) {
		enhance = nil
		if la != nil && la.Presence.Written("enhance_search", la.EnhanceSearch != nil) {
			enhance = la.EnhanceSearch
		}
	}
	if enhance != nil {
		synth.SummaryMode = true
		synth.SummaryLevel = enhance.SummaryLevel
		synth.SummaryThinking = enhance.SummaryThinking
	}
	return synth
}

// EffectiveFallbackSearchSettings builds the SearchRoute-shaped view for a no_search diversion.
// Per field: route claudeSearch* > search_fallback > lazyApply > built-in. The route contributes only
// its two claudeSearch* fields; the fallback owns convertOff2Low and the legacy summary fields.
func EffectiveFallbackSearchSettings(c *Config, rr *RouteRule, sf *SearchRoute) *SearchRoute {
	if sf == nil {
		return &SearchRoute{}
	}
	eff := *sf
	if la := LazyApplyFor(c, sf.NoLazyApply); la != nil {
		if !sf.Presence.Written("convertOff2Low", sf.ConvertOff2Low != "") && la.Presence.Written("convertOff2Low", la.ConvertOff2Low != "") {
			eff.ConvertOff2Low = la.ConvertOff2Low
		}
		if !sf.Presence.Written("summary_mode", sf.SummaryMode) && la.Presence.Written("summary_mode", la.SummaryMode) {
			eff.SummaryMode = la.SummaryMode
		}
		if !sf.Presence.Written("summary_thinking", sf.SummaryThinking) && la.Presence.Written("summary_thinking", la.SummaryThinking) {
			eff.SummaryThinking = la.SummaryThinking
		}
		if !sf.Presence.Written("summary_level", sf.SummaryLevel != "") && la.Presence.Written("summary_level", la.SummaryLevel != "") {
			eff.SummaryLevel = la.SummaryLevel
		}
		if !sf.Presence.Written("claudeSearchThinking", sf.ClaudeSearchThinking != "") && la.Presence.Written("claudeSearchThinking", la.ClaudeSearchThinking != "") {
			eff.ClaudeSearchThinking = la.ClaudeSearchThinking
		}
		if !sf.Presence.Written("claudeSearchSummary", sf.ClaudeSearchSummary != "") && la.Presence.Written("claudeSearchSummary", la.ClaudeSearchSummary != "") {
			eff.ClaudeSearchSummary = la.ClaudeSearchSummary
		}
		if !sf.Presence.Written("noBullshitOnEmptyClaudeSearch", sf.NoBullshitOnEmptyClaudeSearch != nil) && la.Presence.Written("noBullshitOnEmptyClaudeSearch", la.NoBullshitOnEmptyClaudeSearch != nil) {
			eff.NoBullshitOnEmptyClaudeSearch = la.NoBullshitOnEmptyClaudeSearch
		}
	}
	if rr != nil {
		// lazyApply is a materialization layer: a route that omits a claudeSearch* key receives the
		// global value exactly as if it had been written locally, so it participates in the existing
		// route-over-fallback precedence. A route-level noLazyApply removes only the inherited layer.
		laRoute := LazyApplyFor(c, rr.NoLazyApply)
		routeSummary := ""
		if rr.Presence.Written("claudeSearchSummary", rr.ClaudeSearchSummary != "") {
			routeSummary = rr.ClaudeSearchSummary
		} else if laRoute != nil && laRoute.Presence.Written("claudeSearchSummary", laRoute.ClaudeSearchSummary != "") {
			routeSummary = laRoute.ClaudeSearchSummary
		}
		if routeSummary != "" {
			eff.ClaudeSearchSummary = routeSummary
		}
		routeThinking := ""
		if rr.Presence.Written("claudeSearchThinking", rr.ClaudeSearchThinking != "") {
			routeThinking = rr.ClaudeSearchThinking
		} else if laRoute != nil && laRoute.Presence.Written("claudeSearchThinking", laRoute.ClaudeSearchThinking != "") {
			routeThinking = laRoute.ClaudeSearchThinking
		}
		if routeThinking != "" {
			eff.ClaudeSearchThinking = routeThinking
		}
	}
	return &eff
}

// EffectiveFallbackThinking resolves search_fallback.thinking for a request the fallback will serve:
// explicit local > lazyApply > "". The hit route's thinking describes the route's own target model and
// never participates once the request is diverted; the fallback's target is the model actually called.
func EffectiveFallbackThinking(c *Config, sf *SearchRoute) string {
	if sf == nil {
		return ""
	}
	if sf.Presence.Written("thinking", sf.Thinking != "") {
		return sf.Thinking
	}
	if la := LazyApplyFor(c, sf.NoLazyApply); la != nil && la.Presence.Written("thinking", la.Thinking != "") {
		return la.Thinking
	}
	return ""
}
