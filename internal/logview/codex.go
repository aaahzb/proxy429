package logview

import (
	"bytes"
	_ "embed"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"proxy429/internal/platform"
)

// codexSetupPS1 / codexSetupSH are the embedded Codex one-click setup script templates (Windows / macOS·Linux),
// served via /__codexsetup.ps1 and /__codexsetup.sh: the server bakes their BAKED anchors per query params
// (model/base/catalog) before sending; the web console only hands the user a one-line fetch command (same format as DeepSeek's docs).
// The scripts are also published standalone under internal/logview/scripts/ for users running them directly.
//
//go:embed scripts/codex-setup.ps1
var codexSetupPS1 []byte

//go:embed scripts/codex-setup.sh
var codexSetupSH []byte

// Bake anchors: character-for-character identical to how they're written in the template files (codex_setup_test.go locks each to appear exactly once).
// Values are only substituted into the single-quoted empty-string positions after whitelist validation, so quote escape is impossible.
var codexPS1Anchors = [5]string{"$BAKED_BASE_URL = ''", "$BAKED_MODEL    = ''", "$BAKED_CATALOG  = ''", "$BAKED_CONTEXT_WINDOW  = ''", "$BAKED_COMPACT_PERCENT = ''"}

var codexSHAnchors = [5]string{"BAKED_BASE_URL=''", "BAKED_MODEL=''", "BAKED_CATALOG=''", "BAKED_CONTEXT_WINDOW=''", "BAKED_COMPACT_PERCENT=''"}

// slug whitelist: allowed characters for model names/catalog entries (* in route patterns is also allowed); no quotes, whitespace, or URL separators
var codexSlugRE = regexp.MustCompile(`^[A-Za-z0-9._*/:+-]{1,100}$`)

// base whitelist: http(s) addresses (host may be IPv4/domain/bracketed IPv6, path optional)
var codexBaseURLRE = regexp.MustCompile(`^https?://[A-Za-z0-9.:\[\]-]+(/[A-Za-z0-9._~/-]*)?$`)

// ctx/compact whitelist: pure digit strings (baked into the script as JSON numbers in the model catalog; no quotes allowed)
var codexDigitsRE = regexp.MustCompile(`^[0-9]{1,7}$`)

// codexScriptHandler bakes query parameters into the script template's BAKED anchors before serving — the one-line command
// the config page gives the user (irm '<url>' | iex / bash <(curl -fsSL '<url>')) pulls exactly this baked version, paste and run:
//
//	?model=<slug>     default model (__restore__ = restore defaults directly; empty = interactive menu inside the script)
//	?base=<url>       Responses listener address (empty = the script asks or uses its default)
//	?catalog=a,b,c    model list written into Codex's /model menu (empty = the script's built-in demo models)
//	?ctx=262144       context window declared by the catalog (empty = 262144; range 4096..2000000)
//	?compact=95       auto-compact trigger percentage (empty = 95; range 1..99)
//
// With isSH=true, CRLF is additionally normalized to LF (guarding Windows checkouts with autocrlf; bash rejects CR);
// otherwise (ps1) the template file's BOM is stripped: irm|iex decodes by HTTP charset and doesn't need a BOM,
// and iex measurably glues a leading U+FEFF into the first command name and errors (a BOM only helps double-click/file execution).
// Localhost only (same rule as the other /__* management endpoints).
func codexScriptHandler(tpl []byte, anchors [5]string, isSH bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !platform.IsLocalRequest(r) {
			http.Error(w, "forbidden (local only)", http.StatusForbidden)
			return
		}
		q := r.URL.Query()
		model := q.Get("model")
		base := q.Get("base")
		catalog := q.Get("catalog")
		ctxWin := q.Get("ctx")
		compact := q.Get("compact")
		if model != "" && model != "__restore__" && !codexSlugRE.MatchString(model) {
			http.Error(w, "model parameter contains illegal characters", http.StatusBadRequest)
			return
		}
		if base != "" && (len(base) > 200 || !codexBaseURLRE.MatchString(base)) {
			http.Error(w, "base parameter is not a valid http(s) URL", http.StatusBadRequest)
			return
		}
		if catalog != "" {
			slugs := strings.Split(catalog, ",")
			if len(slugs) > 64 || len(catalog) > 4000 {
				http.Error(w, "catalog parameter too long", http.StatusBadRequest)
				return
			}
			for _, s := range slugs {
				if !codexSlugRE.MatchString(s) {
					http.Error(w, "catalog parameter contains illegal characters: "+s, http.StatusBadRequest)
					return
				}
			}
		}
		// ctx/compact are used as JSON numbers once baked into the script, so clamp the range hard here (window 4k..2M, compact 1..99%)
		if ctxWin != "" {
			if n, err := strconv.Atoi(ctxWin); !codexDigitsRE.MatchString(ctxWin) || err != nil || n < 4096 || n > 2000000 {
				http.Error(w, "ctx parameter must be an integer in 4096..2000000", http.StatusBadRequest)
				return
			}
		}
		if compact != "" {
			if n, err := strconv.Atoi(compact); !codexDigitsRE.MatchString(compact) || err != nil || n < 1 || n > 99 {
				http.Error(w, "compact parameter must be an integer in 1..99", http.StatusBadRequest)
				return
			}
		}
		out := tpl
		if isSH {
			out = bytes.ReplaceAll(out, []byte("\r\n"), []byte("\n"))
		} else {
			out = bytes.TrimPrefix(out, []byte{0xEF, 0xBB, 0xBF})
		}
		for i, v := range [5]string{base, model, catalog, ctxWin, compact} {
			if v == "" {
				continue
			}
			a := anchors[i]
			// Anchors end with an empty string ''; replace with a single-quoted string carrying the value (the value passed the whitelist, no single quotes)
			out = bytes.Replace(out, []byte(a), []byte(strings.TrimSuffix(a, "''")+"'"+v+"'"), 1)
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(out)
	}
}
