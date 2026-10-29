# Config String Variables Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let a config file define named string variables in a top-level `vars` object and reference them from any string value as `$NAME` / `${NAME}`, so rotating one upstream key means a single edit instead of N.

**Architecture:** One expansion hook inside `parseConfig` (the single funnel for startup load, tray reload, and the web-console save probe). The hook walks the decoded JSON tree, expands references in string values only, and the existing flow then unmarshals the expanded tree into `Config`. The file on disk keeps `$NAME` references verbatim; the web console edits raw text, so references survive save round-trips, and no handler ever serializes the runtime `Config` back to a client.

**Tech Stack:** Go (single flat package `proxy429`), no new dependencies; tests with the standard `testing` package against `parseConfig`.

**Spec:** `docs/superpowers/specs/2026-10-29-config-vars-design.md` (commit 915c3d6).

## Global Constraints

- Repo: public GitHub repo, single flat Go package at repo root. **Never `git push`** (the user pushes). `git add` explicit file names only — `Temp/` is never added. No `Co-Authored-By` / no Claude attribution in commits. No apostrophes in commit messages. All code comments, docs, and commit messages in English.
- Line endings (autocrlf=true; edits must preserve them): `main.go` LF, `main_test.go` LF, `config.example.json` LF, `docs/usage.md` LF, `test/smoke/config.json` LF, `test/smoke/smoke.sh` LF; `logview.go` CRLF, `docs/DESIGN.md` CRLF.
- CRLF files are edited with perl raw-byte paired patches only (script written to `Temp/patchN.pl` via the Write tool, run with `perl Temp/patchN.pl`; placeholders `<Rd>`/`<NL>` families only — the literal token `<TAB>` gets decoded into a real tab by the tool layer and must never appear in a patch script; the script's save step must `truncate($o, tell($o))` after print). Never `sed -i` or `perl -i` on CRLF files. After patching, audit: `tr -cd '\r' < f | wc -c` must equal `tr -cd '\n' < f | wc -c`.
- LF files may be edited with the Edit tool or small perl patches.
- After any `.go` change: `gofmt -w` the touched files, then `go vet ./... && go test -count=1 ./...` must be green before committing. For the CRLF `logview.go`, gofmt-check via `tr -d '\r' < logview.go > /tmp/x.go && gofmt -l /tmp/x.go` (must print nothing).
- House acceptance chain at the end: commit BEFORE `bash build.sh` (the exe version stamp is short-hash+HHMM and must match HEAD), then `bash test/smoke/smoke.sh && bash test/smoke/exe-watchdog.sh` (smoke uses ports 9099/18083/18084; production 8080/8081 must not be touched; smoke runs `./release/proxy429.exe` so the build must precede it). The user personally accepts the new exe — never say "no need to rebuild".
- TDD: write the failing test first, watch it fail for the right reason, then implement.
- Never print real API keys. `config.example.json` and all fixtures use placeholder keys only.

---

### Task 1: Expander core in `parseConfig` + `Config.Vars` + unit tests

**Files:**
- Modify: `main.go` (parseConfig hook ~line 247-265; new vars machinery inserted after parseConfig; `Config` struct field)
- Test: `main_test.go` (new test near `TestLoadConfigEnumValidation`, main_test.go:1400)

**Interfaces:**
- Consumes: existing `func parseConfig(data []byte) (*Config, []string, error)` and `type Config struct`.
- Produces (used by Tasks 2-3 only through behavior): `func expandConfigVars(top map[string]json.RawMessage) error` — resolves all `$NAME`/`${NAME}` references inside string values of `top` against the top-level `vars` object; error text for undefined names is `undefined variable "NAME" in PATH`, for bad members `vars.NAME must be a string`. `Config` gains field `Vars map[string]string `json:"vars,omitempty"`` holding the expanded (inert) copy.

- [ ] **Step 1: Write the failing test**

In `main_test.go`, insert this test immediately before `func TestLoadConfigEnumValidation` (anchor: the line `func TestLoadConfigEnumValidation(t *testing.T) {` at main_test.go:1400):

```go
// TestParseConfigVars locks the config vars feature: a top-level "vars" object defines named strings that any
// string value in the file can reference as $NAME or ${NAME}. Resolution happens at load inside parseConfig;
// a well-formed reference to an undefined name fails with the config path; $ not introducing a well-formed
// reference stays literal; vars values must be strings and may reference other vars (one level).
func TestParseConfigVars(t *testing.T) {
	mustParse := func(t *testing.T, body string) *Config {
		t.Helper()
		c, _, err := parseConfig([]byte(body))
		if err != nil {
			t.Fatalf("parseConfig: %v", err)
		}
		return c
	}

	t.Run("basic braced and url", func(t *testing.T) {
		c := mustParse(t, `{"vars": {"KIMI_KEY": "sk-real", "KIMI_BASE": "https://api.moonshot.cn"},
			"routes": [{"pattern": "kimi*", "url": "$KIMI_BASE", "api": "${KIMI_KEY}"}]}`)
		if got := c.Routes[0].API; got != "sk-real" {
			t.Errorf("routes[0].api = %q, want sk-real", got)
		}
		if got := c.Routes[0].URL; got != "https://api.moonshot.cn" {
			t.Errorf("routes[0].url = %q, want https://api.moonshot.cn", got)
		}
	})

	t.Run("shared reference and nesting", func(t *testing.T) {
		c := mustParse(t, `{"vars": {"A": "$B-x", "B": "sk", "K": "smoke"},
			"search_fallback": {"url": "http://h", "api": "$K"},
			"routes": [{"pattern": "a", "url": "http://h", "api": "$A"},
			           {"pattern": "b", "url": "http://h", "api": "$K"}]}`)
		if got := c.Routes[0].API; got != "sk-x" {
			t.Errorf("routes[0].api = %q, want sk-x (var referencing a var)", got)
		}
		if got := c.Routes[1].API; got != "smoke" {
			t.Errorf("routes[1].api = %q, want smoke", got)
		}
		if c.SearchFallback == nil || c.SearchFallback.API != "smoke" {
			t.Errorf("search_fallback.api = %v, want smoke", c.SearchFallback)
		}
		if got := c.Vars["A"]; got != "sk-x" {
			t.Errorf("c.Vars[A] = %q, want sk-x", got)
		}
	})

	t.Run("undefined variable names the path", func(t *testing.T) {
		_, _, err := parseConfig([]byte(`{"routes": [{"pattern": "a", "url": "http://h", "api": "$KIMI_KET"}]}`))
		if err == nil {
			t.Fatal("want error for undefined variable")
		}
		for _, sub := range []string{`undefined variable "KIMI_KET"`, "routes[0].api"} {
			if !strings.Contains(err.Error(), sub) {
				t.Errorf("error %q missing %q", err.Error(), sub)
			}
		}
	})

	t.Run("undefined inside vars names vars path", func(t *testing.T) {
		_, _, err := parseConfig([]byte(`{"vars": {"A": "$NOPE"}, "routes": []}`))
		if err == nil || !strings.Contains(err.Error(), "vars.A") {
			t.Fatalf("want error naming vars.A, got %v", err)
		}
	})

	t.Run("non-string var value", func(t *testing.T) {
		_, _, err := parseConfig([]byte(`{"vars": {"KIMI_KEY": 123}, "routes": []}`))
		if err == nil || !strings.Contains(err.Error(), "vars.KIMI_KEY must be a string") {
			t.Fatalf("want vars.KIMI_KEY must be a string, got %v", err)
		}
	})

	t.Run("literal dollar preserved", func(t *testing.T) {
		c := mustParse(t, `{"vars": {}, "upstream": "http://u/$5/x$",
			"routes": [{"pattern": "a", "url": "http://h?a=$ x", "api": "sk"}]}`)
		if got := c.Upstream; got != "http://u/$5/x$" {
			t.Errorf("upstream = %q, want literal $ preserved", got)
		}
		if got := c.Routes[0].URL; got != "http://h?a=$ x" {
			t.Errorf("routes[0].url = %q, want literal $ preserved", got)
		}
	})

	t.Run("empty var value", func(t *testing.T) {
		c := mustParse(t, `{"vars": {"E": ""}, "routes": [{"pattern": "a", "url": "http://h", "api": "$E"}]}`)
		if got := c.Routes[0].API; got != "" {
			t.Errorf("routes[0].api = %q, want empty", got)
		}
	})

	t.Run("vars absent leaves values alone", func(t *testing.T) {
		c := mustParse(t, `{"upstream": "http://u", "routes": [{"pattern": "a", "url": "http://h", "api": "sk-plain"}]}`)
		if got := c.Routes[0].API; got != "sk-plain" {
			t.Errorf("routes[0].api = %q, want sk-plain", got)
		}
	})
}
```

- [ ] **Step 2: Run the test, verify it fails for the right reason**

Run: `go test -count=1 -run TestParseConfigVars .`
Expected: FAIL — `routes[0].api = "$KIMI_KEY", want sk-real` style assertion failures (no expansion exists yet). Not a compile error: the test only calls existing `parseConfig`/`Config`.

- [ ] **Step 3: Implement the expander in `main.go`**

3a. `Config` struct field. Anchor on the `Upstream` field line inside `type Config struct` (main.go:32):

```go
	Upstream                 string           `json:"upstream"`
```

Insert immediately after it:

```go
	Vars                     map[string]string `json:"vars,omitempty"`              // Named string variables: any string value in this file can reference them as $NAME / ${NAME}; resolved at load (the file keeps the references)
```

Note: `map[string]string` is one char wider than the previous widest type column, so `gofmt -w main.go` will realign the whole struct's comment column — expected (same as commit 9e8c36a), do not hand-align.

3b. Hook in `parseConfig`. Anchor (main.go:236-242, inside parseConfig after the removed-keys loop):

```go
	for _, rk := range removedConfigKeys {
		if _, ok := top[rk.key]; ok {
			warnings = append(warnings, rk.key)
		}
	}
	var c Config
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, nil, err
	}
```

Replace with:

```go
	for _, rk := range removedConfigKeys {
		if _, ok := top[rk.key]; ok {
			warnings = append(warnings, rk.key)
		}
	}
	// Resolve $VAR references before struct decoding: the in-memory Config carries final values while the file
	// (and the web console, which edits raw text) keeps the references verbatim.
	if err := expandConfigVars(top); err != nil {
		return nil, nil, err
	}
	full, err := json.Marshal(top)
	if err != nil {
		return nil, nil, err
	}
	var c Config
	if err := json.Unmarshal(full, &c); err != nil {
		return nil, nil, err
	}
```

3c. Vars machinery. Insert immediately before the line `// removedConfigKeys lists top-level config keys removed in this version` (the first such line after parseConfig, ~main.go:252):

```go
// expandConfigVars resolves $NAME / ${NAME} references inside every JSON string value of the top-level config
// object against the top-level "vars" object (whose own values expand too, one level — replacement text is never
// re-scanned, so reference cycles are impossible). Object keys are never touched. A well-formed reference to an
// undefined name fails with the config path; "vars" members must be strings.
func expandConfigVars(top map[string]json.RawMessage) error {
	final := map[string]string{}
	if raw, ok := top["vars"]; ok {
		base, err := varStringMap(raw)
		if err != nil {
			return err
		}
		if base != nil {
			expanded, err := expandVarsInJSON(raw, "vars", func(name string) (string, bool) {
				v, ok := base[name]
				return v, ok
			})
			if err != nil {
				return err
			}
			final, err = varStringMap(expanded) // Values already validated as strings; cannot fail
			if err != nil {
				return err
			}
		}
	}
	lookup := func(name string) (string, bool) {
		v, ok := final[name]
		return v, ok
	}
	return expandVarsInMap(top, "", lookup)
}

// varStringMap decodes a "vars" object into name -> value. Returns nil for an absent/null object; anything that
// is not an object of strings is an error naming the offending member.
func varStringMap(raw json.RawMessage) (map[string]string, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil, nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &m); err != nil {
		return nil, fmt.Errorf("vars must be an object of string values: %v", err)
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		var s string
		if err := json.Unmarshal(v, &s); err != nil {
			return nil, fmt.Errorf("vars.%s must be a string", k)
		}
		out[k] = s
	}
	return out, nil
}

// expandVarsInMap expands every value of m in place; path is the dotted/bracketed location prefix for errors.
func expandVarsInMap(m map[string]json.RawMessage, path string, lookup func(string) (string, bool)) error {
	for k, v := range m {
		ev, err := expandVarsInJSON(v, joinConfigPath(path, k), lookup)
		if err != nil {
			return err
		}
		m[k] = ev
	}
	return nil
}

// joinConfigPath appends a key to a dotted/bracketed config path ("routes" -> "routes[0]" -> "routes[0].api").
func joinConfigPath(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}

// expandVarsInJSON expands references in every string value reachable from node (object values and array
// elements). Non-string scalars pass through as their original bytes; containers holding them re-marshal with
// each RawMessage embedded verbatim, so numeric text is never round-tripped.
func expandVarsInJSON(node json.RawMessage, path string, lookup func(string) (string, bool)) (json.RawMessage, error) {
	trimmed := bytes.TrimSpace(node)
	if len(trimmed) == 0 {
		return node, nil
	}
	switch trimmed[0] {
	case '"':
		var s string
		if err := json.Unmarshal(trimmed, &s); err != nil {
			return node, nil
		}
		out, err := expandVarRefs(s, path, lookup)
		if err != nil {
			return nil, err
		}
		b, _ := json.Marshal(out)
		return json.RawMessage(b), nil
	case '{':
		var m map[string]json.RawMessage
		if err := json.Unmarshal(trimmed, &m); err != nil {
			return node, nil
		}
		if err := expandVarsInMap(m, path, lookup); err != nil {
			return nil, err
		}
		b, _ := json.Marshal(m)
		return json.RawMessage(b), nil
	case '[':
		var a []json.RawMessage
		if err := json.Unmarshal(trimmed, &a); err != nil {
			return node, nil
		}
		for i, v := range a {
			ev, err := expandVarsInJSON(v, fmt.Sprintf("%s[%d]", path, i), lookup)
			if err != nil {
				return nil, err
			}
			a[i] = ev
		}
		b, _ := json.Marshal(a)
		return json.RawMessage(b), nil
	default:
		return node, nil
	}
}

// expandVarRefs replaces $NAME and ${NAME} references using lookup. A name is ASCII letters/digits/underscore
// starting with a letter or underscore. '$' not introducing a well-formed reference stays literal; a well-formed
// reference to an undefined name is an error naming the config path.
func expandVarRefs(s, path string, lookup func(string) (string, bool)) (string, error) {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		if s[i] != '$' || i+1 >= len(s) {
			b.WriteByte(s[i])
			i++
			continue
		}
		var name string
		var n int // length of the whole "$name" token consumed
		if s[i+1] == '{' {
			rest := s[i+2:]
			end := strings.IndexByte(rest, '}')
			if end < 0 {
				b.WriteByte(s[i])
				i++
				continue // Unclosed brace: literal
			}
			if !isVarName(rest[:end]) {
				b.WriteByte(s[i])
				i++
				continue // Malformed braced name: literal
			}
			name, n = rest[:end], end+3
		} else {
			j := i + 1
			for j < len(s) && isVarNameChar(s[j]) {
				j++
			}
			if j == i+1 || !isVarNameStart(s[i+1]) {
				b.WriteByte(s[i])
				i++
				continue // Not a well-formed reference: literal
			}
			name, n = s[i+1:j], j-i
		}
		v, ok := lookup(name)
		if !ok {
			return "", fmt.Errorf("undefined variable %q in %s", name, path)
		}
		b.WriteString(v)
		i += n
	}
	return b.String(), nil
}

func isVarNameStart(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func isVarNameChar(c byte) bool {
	return isVarNameStart(c) || (c >= '0' && c <= '9')
}

func isVarName(s string) bool {
	if s == "" || !isVarNameStart(s[0]) {
		return false
	}
	for i := 1; i < len(s); i++ {
		if !isVarNameChar(s[i]) {
			return false
		}
	}
	return true
}
```

Imports: `bytes`, `encoding/json`, `fmt`, `strings` are all already imported in main.go — nothing to add.

- [ ] **Step 4: Format, vet, run tests**

```bash
gofmt -w main.go main_test.go
go vet ./... && go test -count=1 ./...
```

Expected: gofmt only realigns the Config struct comment column (3a note); vet silent; all tests PASS including `TestParseConfigVars`.

- [ ] **Step 5: Commit**

```bash
git add main.go main_test.go
git commit -m "feat: resolve config string values against a top-level vars section"
```

---

### Task 2: Example config + docs + console help

**Files:**
- Modify: `config.example.json` (LF)
- Modify: `docs/DESIGN.md` (CRLF) — Configuration section
- Modify: `docs/usage.md` (LF)
- Modify: `logview.go` (CRLF) — zh + en quick-reference tables

**Interfaces:**
- Consumes: Task 1 behavior (example and fixtures must load through `parseConfig`).
- Produces: user-facing documentation only; no code interfaces.

- [ ] **Step 1: `config.example.json`** (Edit tool)

1a. Anchor on line 3 (`  "upstream": "https://ark.cn-beijing.volces.com/api/plan",`) — insert after it:

```json
  "vars": { "DEEPSEEK_KEY": "sk-deepseek-replace-with-your-key" },
```

1b. Replace all 6 occurrences of

```json
      "api": "sk-deepseek-replace-with-your-key",
```

(indentation varies per nesting depth — the 6 occurrences live in 3 routes[] entries at 6-space indent plus classifier_route / fast_route / search_fallback at 4-space indent; the multimodal entries carry a different placeholder key and stay literal; verify counts with `grep -c '"api": "sk-deepseek-replace-with-your-key"' config.example.json` — 6 before, 0 after) with the same line at the same indentation but `"api": "$DEEPSEEK_KEY",`. The Edit tool's `replace_all` on the exact 6-space-indented variant covers the route entries; handle the 4-space-indented top-level entries (classifier_route/fast_route/search_fallback at 4 spaces) with a second replace_all of the 4-space variant. `config.example.json` stays LF.

- [ ] **Step 2: `docs/DESIGN.md`** (CRLF — perl raw-byte paired patch via `Temp/patch5.pl`, written with the Write tool, `<NL>`/`<Rd>` placeholders only, save step `truncate($o, tell($o))`)

Anchor (the `- **respect_retry_after**` bullet, which is at 4-space list indentation under "## Configuration"):

```
- **respect_retry_after**: whether to honor the upstream `Retry-After` header first.
```

Insert immediately after it one new bullet (same `- ` prefix, wrapped to match the file's prose style):

```
- **vars** (optional): named string variables, an object at the top level. Any string value anywhere in the file can reference them as `$NAME` or `${NAME}` — e.g. define `"vars": {"KIMI_KEY": "sk-..."}` once and use `"api": "$KIMI_KEY"` in every route that upstream, so rotating a key is a single edit. Resolution happens at load: the file on disk (and the web console, which edits raw text) keeps the references verbatim, only the in-memory config carries final values. A well-formed reference to an undefined name refuses to load (and the console save probe rejects it) with the config path, e.g. `undefined variable "KIMI_KET" in routes[0].api`; values must be strings (empty string allowed); a var may reference another var (one level); a `$` not introducing a well-formed reference (e.g. `$5`) stays literal.
```

- [ ] **Step 3: `docs/usage.md`** (LF — Edit tool)

Locate the routes block (anchor: the line `  { "*",        "url": "https://upstream-a", "api": "sk-aaa", "model": "strong-model" }` at usage.md:~159). Insert immediately after the closing ``` of that fenced block (verify the exact fence line at execution — it is the first ```` ``` ```` line after the anchor):

```
Shared keys can be defined once in a top-level `"vars"` object and referenced from any string value as `$NAME` or `${NAME}` — `"vars": {"KIMI_KEY": "sk-..."}` plus `"api": "$KIMI_KEY"` in N routes means rotating the key edits one place. Resolution happens at load; the file keeps the references. An undefined reference refuses to load/save with the config path; a `$` not forming a reference (e.g. `$5`) stays literal.
```

- [ ] **Step 4: `logview.go` console help** (CRLF — extend `Temp/patch5.pl`)

Two new table rows, one in the zh block and one in the en block of the 参数速查 table. In each block the anchor is the `<tr ...>` line immediately preceding the `<td ...><code>multimodal_fallback</code></td>` cell — the zh block's row opener uses `border-bottom:1px solid #333` (~logview.go:2827), the en block's uses `border-bottom:1px solid #2a2a2a` (~logview.go:2958); anchor each on its two-line `<tr ...>\n      <td ...><code>multimodal_fallback</code></td>` pair and insert the new row BEFORE the anchor `<tr>`.

zh row (note CRLF line endings via the patch machinery):

```
      <tr style="border-bottom:1px solid #333">
      <td style="padding:6px 8px;vertical-align:top"><code>vars</code></td>
      <td style="padding:6px 8px;vertical-align:top">顶层</td>
      <td style="padding:6px 8px;vertical-align:top">命名字符串变量：文件里任意字符串值可写 <code>$NAME</code> 或 <code>${NAME}</code> 引用（key、url 都行），换 key 只改 vars 一处</td>
      <td style="padding:6px 8px;vertical-align:top">加载时解析、文件保留引用原文；引用未定义变量 = 加载/保存报错并指明位置（如 routes[0].api）；值必须是字符串（空串允许）；变量可再引用变量（单层）；$ 后不是合法名字（如 $5）原样保留</td>
      </tr>
```

en row:

```
      <tr style="border-bottom:1px solid #2a2a2a">
      <td style="padding:6px 8px;vertical-align:top"><code>vars</code></td>
      <td style="padding:6px 8px;vertical-align:top">Top level</td>
      <td style="padding:6px 8px;vertical-align:top">Named string variables: any string value in the file can reference them as <code>$NAME</code> / <code>${NAME}</code> (api, url, anything) — rotating a key edits one place instead of N</td>
      <td style="padding:6px 8px;vertical-align:top">Resolved at load, the file keeps the references; an undefined reference fails load/save with the config path (e.g. routes[0].api); values must be strings (empty allowed); vars may reference vars (one level); a $ not forming a reference (e.g. $5) stays literal</td>
      </tr>
```

- [ ] **Step 5: Verify — endings audit, gofmt, full tests**

```bash
tr -cd '\r' < docs/DESIGN.md | wc -c   # must equal: tr -cd '\n' < docs/DESIGN.md | wc -c
tr -cd '\r' < logview.go | wc -c       # must equal: tr -cd '\n' < logview.go | wc -c
tr -d '\r' < logview.go > /tmp/x.go && gofmt -l /tmp/x.go   # must print nothing
go vet ./... && go test -count=1 ./...
```

Expected: CRLF counts match line counts on both CRLF files; gofmt clean; all tests PASS (this run loads the modified `config.example.json` through `config_example_test.go`, proving the example's own `$DEEPSEEK_KEY` references resolve).

- [ ] **Step 6: Commit**

```bash
git add config.example.json docs/DESIGN.md docs/usage.md logview.go
git commit -m "docs: document the config vars section with example and console help"
```

---

### Task 3: Smoke end-to-end + acceptance chain

**Files:**
- Modify: `test/smoke/config.json` (LF)
- Modify: `test/smoke/smoke.sh` (LF) — header comment only

**Interfaces:**
- Consumes: Tasks 1-2. No code changes.

- [ ] **Step 1: `test/smoke/config.json`** (Edit tool)

Anchor on the `search_fallback` line; change `"api": "smoke"` to `"api": "$SMOKE_KEY"` inside `search_fallback` only, and add a top-level vars object. Anchor on:

```json
  "classifier_route": { "url": "http://127.0.0.1:9099/think", "classifier_thinking_cc": "off", "classifier_thinking_codex": "low" },
```

Insert immediately before it:

```json
  "vars": { "SMOKE_KEY": "smoke" },
```

Then in the `search_fallback` line change `"api": "smoke"` to `"api": "$SMOKE_KEY"` (the two routes keep the literal `"api": "smoke"` — a resolved reference and two literals must all keep working).

- [ ] **Step 2: `test/smoke/smoke.sh` header comment** (Edit tool)

Anchor on the file's header comment block (lines 5-9, the lines starting `# Search:`). Append one line after the existing `# Search: ...` block:

```
# Vars: config.json defines vars.SMOKE_KEY and search_fallback references it as $SMOKE_KEY — a broken reference
# refuses config load, so the proxy ports never come up and this smoke ABORTs; passing proves end-to-end resolution.
```

- [ ] **Step 3: vet + tests + commit BEFORE build**

```bash
go vet ./... && go test -count=1 ./...
git add test/smoke/config.json test/smoke/smoke.sh
git commit -m "test: prove config vars end to end through the smoke config"
```

Expected: vet silent, tests PASS. The exe version stamp must match HEAD, so this commit lands before the build.

- [ ] **Step 4: Build and run the acceptance chain**

```bash
bash build.sh
bash test/smoke/smoke.sh
bash test/smoke/exe-watchdog.sh
```

Expected: `BUILD_OK version=beta <short-hash>-<HHMM> ...` with the hash equal to the Task 3 commit; `SMOKE PASS`; `EXE TEST PASS`. If a port from a previous run is orphaned (ABORT: already listening), kill the owning PID via `netstat -ano | grep <port>` and `taskkill //F //PID <pid>` — never touch 8080/8081.

- [ ] **Step 5: Report for acceptance**

Report in Chinese: what was built (vars + `$NAME`/`${NAME}`, strict undefined-error with path, file keeps references, console save probe validates), the test/smoke/watchdog results, and the commit list. The user personally accepts the new exe and pushes all unpushed commits — never push, never say "no need to rebuild".
