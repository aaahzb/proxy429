package config

// IsReservedRoutePattern reports whether a pattern exactly matches a reserved name (colliding routes don't take effect): "Fallback" is
// the * catch-all route, "fast_route" is the fast channel's entry name in the Codex menu — Codex scripts send these names as
// model (fast_route gets speed:"fast" injected by the translation layer and goes down the fast branch); a colliding route would intercept that traffic.
func IsReservedRoutePattern(p string) bool {
	return p == "Fallback" || p == "fast_route"
}

// HasCatchAllRoute reports whether routes contains a pattern:"*" catch-all rule (matches every model).
// With one, the top-level upstream may be left empty — every model-bearing request is taken over by the catch-all route and the default upstream goes unused.
func HasCatchAllRoute(routes []RouteRule) bool {
	for i := range routes {
		if routes[i].Pattern == "*" {
			return true
		}
	}
	return false
}
