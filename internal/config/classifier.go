package config

type ClassifierSource int

const (
	ClassifierNone  ClassifierSource = iota // No classifier signature
	ClassifierCC                            // Claude Code safety check ("You are a security monitor")
	ClassifierCodex                         // Codex guardian approval assessment ("You are judging one planned coding-agent action.")
)

// ClassifierThinkingModeFor resolves the effective thinking policy for a classifier source:
// the per-source override (classifier_thinking_cc / classifier_thinking_codex) wins;
// unset falls back to the shared classifier_thinking; "" = the request's thinking goes through untouched.
func ClassifierThinkingModeFor(c *Config, kind ClassifierSource) string {
	if c == nil || c.ClassifierRoute == nil {
		return ""
	}
	cr := c.ClassifierRoute
	la := LazyApplyFor(c, cr.NoLazyApply)
	// Resolve each field independently: explicit local > lazyApply > built-in empty.
	shared := cr.ClassifierThinking
	if !cr.Presence.Written("classifier_thinking", shared != "") {
		shared = ""
		if la != nil && la.Presence.Written("classifier_thinking", la.ClassifierThinking != "") {
			shared = la.ClassifierThinking
		}
	}
	cc := cr.ClassifierThinkingCC
	if !cr.Presence.Written("classifier_thinking_cc", cc != "") {
		cc = ""
		if la != nil && la.Presence.Written("classifier_thinking_cc", la.ClassifierThinkingCC != "") {
			cc = la.ClassifierThinkingCC
		}
	}
	codex := cr.ClassifierThinkingCodex
	if !cr.Presence.Written("classifier_thinking_codex", codex != "") {
		codex = ""
		if la != nil && la.Presence.Written("classifier_thinking_codex", la.ClassifierThinkingCodex != "") {
			codex = la.ClassifierThinkingCodex
		}
	}
	if kind == ClassifierCodex && codex != "" {
		return codex
	}
	if kind == ClassifierCC && cc != "" {
		return cc
	}
	return shared
}
