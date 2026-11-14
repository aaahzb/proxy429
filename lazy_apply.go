package main

// lazyApplyFor returns the defaults object that may apply to one route-like object. A nil config or
// noLazyApply:true makes the object ignore lazyApply entirely; the object's own fields stay authoritative.
func lazyApplyFor(c *Config, optOut bool) *LazyApplyConfig {
	if c == nil || optOut {
		return nil
	}
	return c.LazyApply
}

// effectiveRouteTextOnly resolves routes[].text_only: explicit local > lazyApply > false.
func effectiveRouteTextOnly(c *Config, rr *RouteRule) bool {
	if rr == nil {
		return false
	}
	if rr.presence.written("text_only", rr.TextOnly) {
		return rr.TextOnly
	}
	if la := lazyApplyFor(c, rr.NoLazyApply); la != nil && la.presence.written("text_only", la.TextOnly) {
		return la.TextOnly
	}
	return false
}

// effectiveRouteNoSearch resolves routes[].no_search: explicit local > lazyApply > false.
func effectiveRouteNoSearch(c *Config, rr *RouteRule) bool {
	if rr == nil {
		return false
	}
	if rr.presence.written("no_search", rr.NoSearch) {
		return rr.NoSearch
	}
	if la := lazyApplyFor(c, rr.NoLazyApply); la != nil && la.presence.written("no_search", la.NoSearch) {
		return la.NoSearch
	}
	return false
}

// effectiveRouteThinking resolves routes[].thinking: explicit local > lazyApply > "".
func effectiveRouteThinking(c *Config, rr *RouteRule) string {
	if rr == nil {
		return ""
	}
	if rr.presence.written("thinking", rr.Thinking != "") {
		return rr.Thinking
	}
	if la := lazyApplyFor(c, rr.NoLazyApply); la != nil && la.presence.written("thinking", la.Thinking != "") {
		return la.Thinking
	}
	return ""
}

// effectiveConvertOff2Low resolves convertOff2Low for one route-like object: explicit local > lazyApply > "".
// localWritten must come from that object's key-presence metadata (or non-zero convention for Go literals).
func effectiveConvertOff2Low(c *Config, local string, localWritten, optOut bool) string {
	if localWritten {
		return local
	}
	if la := lazyApplyFor(c, optOut); la != nil && la.presence.written("convertOff2Low", la.ConvertOff2Low != "") {
		return la.ConvertOff2Low
	}
	return ""
}

// effectiveDirectSearchSettings builds the SearchRoute-shaped view used by resolveSearchBehavior for a
// direct route. Every field is resolved independently: a written local value wins, otherwise lazyApply,
// otherwise the built-in zero. enhance_search is atomic: a local object (or explicit null) wins whole,
// so lazyApply never partially merges into a local enhance object.
func effectiveDirectSearchSettings(c *Config, rr *RouteRule) *SearchRoute {
	if rr == nil {
		return &SearchRoute{}
	}
	la := lazyApplyFor(c, rr.NoLazyApply)
	synth := &SearchRoute{}
	if rr.presence.written("claudeSearchSummary", rr.ClaudeSearchSummary != "") {
		synth.ClaudeSearchSummary = rr.ClaudeSearchSummary
	} else if la != nil && la.presence.written("claudeSearchSummary", la.ClaudeSearchSummary != "") {
		synth.ClaudeSearchSummary = la.ClaudeSearchSummary
	}
	if rr.presence.written("claudeSearchThinking", rr.ClaudeSearchThinking != "") {
		synth.ClaudeSearchThinking = rr.ClaudeSearchThinking
	} else if la != nil && la.presence.written("claudeSearchThinking", la.ClaudeSearchThinking != "") {
		synth.ClaudeSearchThinking = la.ClaudeSearchThinking
	}
	enhance := rr.EnhanceSearch
	if !rr.presence.written("enhance_search", rr.EnhanceSearch != nil) {
		enhance = nil
		if la != nil && la.presence.written("enhance_search", la.EnhanceSearch != nil) {
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

// effectiveFallbackSearchSettings builds the SearchRoute-shaped view for a no_search diversion.
// Per field: route claudeSearch* > search_fallback > lazyApply > built-in. The route contributes only
// its two claudeSearch* fields; the fallback owns convertOff2Low and the legacy summary fields.
func effectiveFallbackSearchSettings(c *Config, rr *RouteRule, sf *SearchRoute) *SearchRoute {
	if sf == nil {
		return &SearchRoute{}
	}
	eff := *sf
	if la := lazyApplyFor(c, sf.NoLazyApply); la != nil {
		if !sf.presence.written("convertOff2Low", sf.ConvertOff2Low != "") && la.presence.written("convertOff2Low", la.ConvertOff2Low != "") {
			eff.ConvertOff2Low = la.ConvertOff2Low
		}
		if !sf.presence.written("summary_mode", sf.SummaryMode) && la.presence.written("summary_mode", la.SummaryMode) {
			eff.SummaryMode = la.SummaryMode
		}
		if !sf.presence.written("summary_thinking", sf.SummaryThinking) && la.presence.written("summary_thinking", la.SummaryThinking) {
			eff.SummaryThinking = la.SummaryThinking
		}
		if !sf.presence.written("summary_level", sf.SummaryLevel != "") && la.presence.written("summary_level", la.SummaryLevel != "") {
			eff.SummaryLevel = la.SummaryLevel
		}
		if !sf.presence.written("claudeSearchThinking", sf.ClaudeSearchThinking != "") && la.presence.written("claudeSearchThinking", la.ClaudeSearchThinking != "") {
			eff.ClaudeSearchThinking = la.ClaudeSearchThinking
		}
		if !sf.presence.written("claudeSearchSummary", sf.ClaudeSearchSummary != "") && la.presence.written("claudeSearchSummary", la.ClaudeSearchSummary != "") {
			eff.ClaudeSearchSummary = la.ClaudeSearchSummary
		}
		if !sf.presence.written("noBullshitOnEmptyClaudeSearch", sf.NoBullshitOnEmptyClaudeSearch != nil) && la.presence.written("noBullshitOnEmptyClaudeSearch", la.NoBullshitOnEmptyClaudeSearch != nil) {
			eff.NoBullshitOnEmptyClaudeSearch = la.NoBullshitOnEmptyClaudeSearch
		}
	}
	if rr != nil {
		// lazyApply is a materialization layer: a route that omits a claudeSearch* key receives the
		// global value exactly as if it had been written locally, so it participates in the existing
		// route-over-fallback precedence. A route-level noLazyApply removes only the inherited layer.
		laRoute := lazyApplyFor(c, rr.NoLazyApply)
		routeSummary := ""
		if rr.presence.written("claudeSearchSummary", rr.ClaudeSearchSummary != "") {
			routeSummary = rr.ClaudeSearchSummary
		} else if laRoute != nil && laRoute.presence.written("claudeSearchSummary", laRoute.ClaudeSearchSummary != "") {
			routeSummary = laRoute.ClaudeSearchSummary
		}
		if routeSummary != "" {
			eff.ClaudeSearchSummary = routeSummary
		}
		routeThinking := ""
		if rr.presence.written("claudeSearchThinking", rr.ClaudeSearchThinking != "") {
			routeThinking = rr.ClaudeSearchThinking
		} else if laRoute != nil && laRoute.presence.written("claudeSearchThinking", laRoute.ClaudeSearchThinking != "") {
			routeThinking = laRoute.ClaudeSearchThinking
		}
		if routeThinking != "" {
			eff.ClaudeSearchThinking = routeThinking
		}
	}
	return &eff
}
