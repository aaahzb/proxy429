# Route Pattern Arrays Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let one `routes[]` entry match several model patterns via `"pattern": ["a","b"]` (OR), exactly equivalent to hand-written duplicate routes.

**Architecture:** Load-time desugar in `parseConfig` (main.go), on the raw JSON tree right after config-vars expansion: array-pattern entries become N adjacent copies with identical other fields. All Go matchers stay untouched. The console's Codex menu JS (which reads the raw editor JSON independently) flattens arrays the same way.

**Tech Stack:** Go (stdlib only: bytes, encoding/json, fmt), HTML-embedded JavaScript in logview.go, bash smoke suite.

## Global Constraints

- Spec: `docs/superpowers/specs/2026-10-30-route-pattern-array-design.md` (approved).
- Repo conventions: all code comments / docs / commit messages in English, no Claude attribution; UI help stays bilingual zh+en.
- main.go, main_test.go, docs/usage.md, test/smoke/* = LF. logview.go, docs/DESIGN.md = CRLF — patch via perl raw-byte paired replacement (Write the patch script to Temp/patchN.pl, then `perl Temp/patchN.pl`), NEVER sed -i; normalize replacement newlines to `\r\n` (`$new =~ s/\r?\n/\r\n/g`) and audit `tr -cd '\r' | wc -c` == `tr -cd '\n' | wc -c` afterwards. Bulletproof save: print, close, then `truncate` to the known length in a separate handle, verify size.
- House chain after ANY change: `go vet ./... && go test -count=1 ./...`; commit BEFORE build (exe stamp must equal HEAD); then `bash build.sh && bash test/smoke/smoke.sh && bash test/smoke/exe-watchdog.sh`.
- NEVER git push (user pushes). Temp/ stays untracked: git add explicit files only.
- gofmt check for CRLF logview.go: `tr -d '\r' < logview.go > /tmp/x.go && gofmt -l /tmp/x.go` must print nothing.
- TDD: write the failing test, watch it fail for the right reason, then implement.

---

### Task 1: Go desugar in parseConfig + unit tests

**Files:**
- Modify: `main.go` (hook at :445-447, new function before `// removedConfigKeys lists` at :408 — note removedConfigKeys sits BEFORE the hook; place the new function right after `expandConfigVars`'s helpers end, i.e. immediately before the `// removedConfigKeys lists` comment)
- Test: `main_test.go` (insert `TestParseConfigPatternArray` right before `func TestLoadConfigEnumValidation` at :1496)

**Interfaces:**
- Produces: `func expandRoutePatterns(top map[string]json.RawMessage) error` — called only from parseConfig; no other consumer.

- [ ] **Step 1: Write the failing test**

Insert before `func TestLoadConfigEnumValidation` in main_test.go:

```go
func TestParseConfigPatternArray(t *testing.T) {
	t.Run("array expands in place preserving order and fields", func(t *testing.T) {
		data := []byte(`{
			"upstream": "http://default",
			"routes": [
				{"pattern": "pre*", "url": "http://pre"},
				{"pattern": ["*fable*", "*opus*"], "url": "http://kimi", "api": "k", "model": "k3"},
				{"pattern": "post*", "url": "http://post"}
			]
		}`)
		c, _, err := parseConfig(data)
		if err != nil {
			t.Fatalf("parseConfig: %v", err)
		}
		if len(c.Routes) != 4 {
			t.Fatalf("want 4 routes after expansion, got %d", len(c.Routes))
		}
		want := []string{"pre*", "*fable*", "*opus*", "post*"}
		for i, p := range want {
			if c.Routes[i].Pattern != p {
				t.Errorf("route #%d pattern = %q, want %q", i+1, c.Routes[i].Pattern, p)
			}
		}
		for _, i := range []int{1, 2} {
			if c.Routes[i].URL != "http://kimi" || c.Routes[i].API != "k" || c.Routes[i].Model != "k3" {
				t.Errorf("copy %d lost fields: %+v", i, c.Routes[i])
			}
		}
	})

	t.Run("both patterns match the same upstream", func(t *testing.T) {
		data := []byte(`{
			"upstream": "http://default",
			"routes": [{"pattern": ["*fable*", "*opus*"], "url": "http://kimi", "api": "k"}]
		}`)
		c, _, err := parseConfig(data)
		if err != nil {
			t.Fatalf("parseConfig: %v", err)
		}
		for _, model := range []string{"claude-fable-5", "claude-opus-4-8"} {
			rr := matchRouteRule(c, model)
			if rr == nil || rr.URL != "http://kimi" {
				t.Errorf("matchRouteRule(%q) = %+v, want http://kimi route", model, rr)
			}
		}
		if rr := matchRouteRule(c, "gpt-5"); rr != nil {
			t.Errorf("unrelated model unexpectedly matched: %+v", rr)
		}
	})

	t.Run("empty array rejected", func(t *testing.T) {
		data := []byte(`{"upstream": "http://d", "routes": [{"pattern": [], "url": "http://x"}]}`)
		_, _, err := parseConfig(data)
		if err == nil || !strings.Contains(err.Error(), "route #1 pattern array is empty") {
			t.Fatalf("want empty-array error, got %v", err)
		}
	})

	t.Run("non-string member rejected", func(t *testing.T) {
		for _, bad := range []string{`["a", 5]`, `["a", null]`, `["a", ["b"]]`, `["a", true]`} {
			data := []byte(`{"upstream": "http://d", "routes": [{"pattern": ` + bad + `, "url": "http://x"}]}`)
			_, _, err := parseConfig(data)
			if err == nil || !strings.Contains(err.Error(), "route #1 pattern must be a string or an array of strings") {
				t.Fatalf("pattern %s: want member-type error, got %v", bad, err)
			}
		}
	})

	t.Run("single missing and null patterns untouched", func(t *testing.T) {
		data := []byte(`{
			"upstream": "http://d",
			"routes": [{"pattern": "a", "url": "http://x"}, {"url": "http://y"}, {"pattern": null, "url": "http://z"}]
		}`)
		c, _, err := parseConfig(data)
		if err != nil {
			t.Fatalf("parseConfig: %v", err)
		}
		if len(c.Routes) != 3 || c.Routes[0].Pattern != "a" || c.Routes[1].Pattern != "" || c.Routes[2].Pattern != "" {
			t.Fatalf("unexpected routes: %+v", c.Routes)
		}
	})

	t.Run("catch-all inside array counts", func(t *testing.T) {
		data := []byte(`{"routes": [{"pattern": ["a", "*"], "url": "http://x"}]}`)
		c, _, err := parseConfig(data)
		if err != nil {
			t.Fatalf("parseConfig: %v", err)
		}
		if !hasCatchAllRoute(c.Routes) {
			t.Fatal("array member * should satisfy hasCatchAllRoute")
		}
	})

	t.Run("reserved name disables only that copy", func(t *testing.T) {
		data := []byte(`{"upstream": "http://d", "routes": [{"pattern": ["Fallback", "*opus*"], "url": "http://kimi"}]}`)
		c, _, err := parseConfig(data)
		if err != nil {
			t.Fatalf("parseConfig: %v", err)
		}
		if len(c.Routes) != 2 {
			t.Fatalf("want 2 copies, got %d", len(c.Routes))
		}
		if rr := matchRouteRule(c, "Fallback"); rr != nil {
			t.Errorf("reserved copy should be skipped, got %+v", rr)
		}
		if rr := matchRouteRule(c, "claude-opus-4"); rr == nil || rr.URL != "http://kimi" {
			t.Errorf("sibling copy should still match, got %+v", rr)
		}
	})

	t.Run("vars inside pattern array resolved before expansion", func(t *testing.T) {
		data := []byte(`{
			"vars": {"P1": "*fable*"},
			"upstream": "http://d",
			"routes": [{"pattern": ["$P1", "*opus*"], "url": "http://kimi"}]
		}`)
		c, _, err := parseConfig(data)
		if err != nil {
			t.Fatalf("parseConfig: %v", err)
		}
		if len(c.Routes) != 2 || c.Routes[0].Pattern != "*fable*" || c.Routes[1].Pattern != "*opus*" {
			t.Fatalf("vars not resolved in pattern array: %+v", c.Routes)
		}
	})
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -run TestParseConfigPatternArray ./ 2>&1 | head -20`
Expected: FAIL — "array expands in place" gets 3 routes instead of 4; "empty array rejected" gets nil error; etc. (Single-string tests may already pass; that is fine, they are guards.)

- [ ] **Step 3: Write minimal implementation**

In main.go, add the hook immediately after the existing vars block at :445-447 (so it reads):

```go
	if err := expandConfigVars(top); err != nil {
		return nil, nil, err
	}
	if err := expandRoutePatterns(top); err != nil {
		return nil, nil, err
	}
	full, err := json.Marshal(top)
```

Add the function immediately before `// removedConfigKeys lists top-level config keys removed in this version`:

```go
// expandRoutePatterns desugars routes[] entries whose pattern is an array into N adjacent copies (one pattern per
// copy, every other field identical): an array entry is exactly equivalent to hand-written duplicate routes, so all
// matchers, cache anchors, and validations keep seeing plain string patterns. Runs on the raw JSON tree after vars
// expansion, so $VAR references inside pattern arrays are already resolved. Single-string, missing, and null patterns
// pass through untouched; an empty array or a non-string member is an error naming the route (1-based).
func expandRoutePatterns(top map[string]json.RawMessage) error {
	raw, ok := top["routes"]
	if !ok {
		return nil
	}
	var entries []json.RawMessage
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil // Not an array: leave it for the final struct decode to report
	}
	out := make([]json.RawMessage, 0, len(entries))
	for i, entry := range entries {
		trimmed := bytes.TrimSpace(entry)
		if len(trimmed) == 0 || trimmed[0] != '{' {
			out = append(out, entry)
			continue
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal(trimmed, &m); err != nil {
			out = append(out, entry)
			continue
		}
		pr, ok := m["pattern"]
		if !ok {
			out = append(out, entry)
			continue
		}
		pr = bytes.TrimSpace(pr)
		if len(pr) == 0 || pr[0] != '[' {
			out = append(out, entry) // String / null / anything non-array: keep today's behavior
			continue
		}
		var pats []json.RawMessage
		if err := json.Unmarshal(pr, &pats); err != nil {
			out = append(out, entry)
			continue
		}
		if len(pats) == 0 {
			return fmt.Errorf("route #%d pattern array is empty", i+1)
		}
		for _, p := range pats {
			pt := bytes.TrimSpace(p)
			if len(pt) == 0 || pt[0] != '"' {
				return fmt.Errorf("route #%d pattern must be a string or an array of strings", i+1)
			}
			cp := make(map[string]json.RawMessage, len(m))
			for k, v := range m {
				cp[k] = v
			}
			cp["pattern"] = p
			b, err := json.Marshal(cp)
			if err != nil {
				return err
			}
			out = append(out, b)
		}
	}
	b, err := json.Marshal(out)
	if err != nil {
		return err
	}
	top["routes"] = b
	return nil
}
```

Note: the `pt[0] != '"'` check (not `json.Unmarshal` into a string) is deliberate — Go's unmarshal of `null` into a string succeeds silently, which would let `[null]` through.

- [ ] **Step 4: Run the tests and gofmt**

Run: `gofmt -w main.go main_test.go && go vet ./... && go test -count=1 -run 'TestParseConfigPatternArray|TestParseConfigVars' ./ && go test -count=1 ./...`
Expected: all PASS (full suite green, including the example-config and routing tests).

- [ ] **Step 5: Commit**

```bash
git add main.go main_test.go
git commit -m "feat: allow routes pattern arrays as OR-matched duplicate entries"
```

---

### Task 2: Console Codex menu JS + docs (usage, DESIGN, console help)

**Files:**
- Modify: `logview.go` (CRLF! JS `deriveCodexModels` at :2065-2082, reserved-name check at :2142-2146, zh routing-help paragraph at :2845, en paragraph at :2986)
- Modify: `docs/usage.md` (LF, after :165)
- Modify: `docs/DESIGN.md` (CRLF, after the **routes** bullet at :77)

**Interfaces:**
- Consumes: the desugar semantics from Task 1 (menu must mirror them).
- Produces: no new API.

- [ ] **Step 1: Patch logview.go via perl paired replacement (Temp/patchN.pl)**

Replacement 1 — `deriveCodexModels` route loop. Old:

```js
  for(const r of (c.routes || [])){
    if(!r || typeof r.pattern !== 'string' || !r.pattern) continue;
    if(r.pattern === 'Fallback' || r.pattern === 'fast_route') continue; // 保留名（* 兜底 / fast 通道在菜单里的名字）：全字撞名的路由不生效也不进菜单
    const m = (typeof r.model === 'string') ? r.model.trim() : '';
    const name = (r.pattern === '*') ? 'Fallback' : ((m && matchPat(r.pattern, m)) ? m : (r.pattern.replace(/\*/g, '') || m));
    // 名字要过服务端 slug 白名单（烤进脚本单引号串 + URL query 都不能有特殊字符）：不合规的跳过
    if(name && /^[A-Za-z0-9._*/:+-]+$/.test(name) && !out.some(o => o.name === name)) out.push({name: name, src: r.pattern});
  }
```

New (arrays flatten into per-member entries, same order and dedup as duplicate routes):

```js
  for(const r of (c.routes || [])){
    if(!r) continue;
    // pattern 可为数组（OR，等价于复制多份路由）：每个成员各出一个代表名，顺序与服务端展开一致
    for(const p of (Array.isArray(r.pattern) ? r.pattern : [r.pattern])){
      if(typeof p !== 'string' || !p) continue;
      if(p === 'Fallback' || p === 'fast_route') continue; // 保留名（* 兜底 / fast 通道在菜单里的名字）：全字撞名的路由不生效也不进菜单
      const m = (typeof r.model === 'string') ? r.model.trim() : '';
      const name = (p === '*') ? 'Fallback' : ((m && matchPat(p, m)) ? m : (p.replace(/\*/g, '') || m));
      // 名字要过服务端 slug 白名单（烤进脚本单引号串 + URL query 都不能有特殊字符）：不合规的跳过
      if(name && /^[A-Za-z0-9._*/:+-]+$/.test(name) && !out.some(o => o.name === name)) out.push({name: name, src: p});
    }
  }
```

Replacement 2 — reserved-name check in `genCodexCmds`. Old:

```js
  const reservedHit = (c.routes || []).find(r => r && (r.pattern === 'Fallback' || r.pattern === 'fast_route'));
  if(reservedHit){
    document.getElementById('codexMsg').innerHTML = '<span class="err">⚠ pattern 不允许全字叫 "' + reservedHit.pattern + '"（Codex 菜单的保留名：* 兜底路由=Fallback、fast 通道=fast_route），撞名的路由不生效，请改名</span>';
  }
```

New:

```js
  let reservedName = '';
  for(const r of (c.routes || [])){
    if(!r) continue;
    for(const p of (Array.isArray(r.pattern) ? r.pattern : [r.pattern])){
      if(p === 'Fallback' || p === 'fast_route'){ reservedName = p; break; }
    }
    if(reservedName) break;
  }
  if(reservedName){
    document.getElementById('codexMsg').innerHTML = '<span class="err">⚠ pattern 不允许全字叫 "' + reservedName + '"（Codex 菜单的保留名：* 兜底路由=Fallback、fast 通道=fast_route），撞名的路由不生效，请改名</span>';
  }
```

Replacement 3 — zh routing-help paragraph (:2845): append a sentence before `</p>`:

```
另外 <code>pattern</code> 可以写成数组表示"或"：<code>"pattern": ["*fable*", "*opus*"]</code> 等价于把同一条路由复制多份、每份一个 pattern（其余字段共享，位置与数组顺序即匹配顺序）；数组成员里的 <code>*</code> 同样算兜底，保留名只废掉对应那一项；空数组或非字符串成员会在加载/保存时报错。
```

Replacement 4 — en routing-help paragraph (:2986): append before `</p>`:

```
<code>pattern</code> also accepts an array (OR): <code>"pattern": ["*fable*", "*opus*"]</code> is exactly equivalent to duplicating the route once per pattern (other fields shared; the entry position and array order are the match order). A <code>*</code> member counts as a catch-all; a reserved name disables only that member; an empty array or a non-string member fails load/save.
```

CRLF discipline: patch script converts every replacement's newlines with `$new =~ s/\r?\n/\r\n/g`; save with print → close → separate-handle truncate to known length → size verify. Then audit: `tr -cd '\r' < logview.go | wc -c` == `tr -cd '\n' < logview.go | wc -c`, and `tr -d '\r' < logview.go > /tmp/x.go && gofmt -l /tmp/x.go` prints nothing.

- [ ] **Step 2: docs/usage.md (LF, plain Edit)**

Insert after line 165 (the "Matches in array order" paragraph):

```markdown
`pattern` also accepts an array of patterns (OR): `{"pattern": ["*fable*", "*opus*"], ...}` is exactly equivalent to writing the same route twice, once per pattern — the copies keep the entry's position, match in array order, and share every other field. A `*` member counts as a catch-all; reserved names (`Fallback`, `fast_route`) disable just that member; an empty array or a non-string member fails load (and the console save probe) with `route #N ...`.
```

- [ ] **Step 3: docs/DESIGN.md (CRLF, same perl patch script or a second one)**

Insert a new bullet immediately after the **routes** bullet (:77):

```markdown
- **pattern arrays**: a `routes[]` entry may write `"pattern": ["a","b"]` (OR). At load, `parseConfig` desugars it into N adjacent copies (after vars expansion, on the raw JSON tree), so every matcher, cache anchor, and validation rule sees plain string patterns — exactly the hand-written-duplicate semantics. Empty arrays and non-string members are load/save errors naming the route; the console's Codex menu derivation flattens arrays the same way.
```

- [ ] **Step 4: Verify**

Run: `go vet ./... && go test -count=1 ./...` plus the CRLF/gofmt audits from Step 1.
Expected: green; CR==LF on logview.go and docs/DESIGN.md.

- [ ] **Step 5: Commit**

```bash
git add logview.go docs/usage.md docs/DESIGN.md
git commit -m "docs: document pattern arrays and flatten them in the Codex menu"
```

---

### Task 3: Smoke end-to-end + acceptance chain

**Files:**
- Modify: `test/smoke/config.json` (LF, smoke-sf route line)
- Modify: `test/smoke/smoke.sh` (LF, header comment after the Vars lines)

- [ ] **Step 1: Switch the smoke-sf route to an array pattern**

In test/smoke/config.json change:

```json
    { "pattern": "smoke-sf", "url": "http://127.0.0.1:9099/ok", "api": "smoke", "no_search": true, "claudeSearchSummary": "forceHigh" },
```

to:

```json
    { "pattern": ["smoke-sf", "smoke-sf-alt"], "url": "http://127.0.0.1:9099/ok", "api": "smoke", "no_search": true, "claudeSearchSummary": "forceHigh" },
```

Existing smoke requests still target the `smoke-sf` model, so a green run proves array expansion on the real startup path.

- [ ] **Step 2: Record it in the smoke.sh header**

After the two `# Vars:` lines add:

```bash
# Pattern arrays: smoke-sf is written as "pattern": ["smoke-sf", "smoke-sf-alt"] (desugared into two route copies at
# load) — a green run proves array expansion works end to end alongside vars.
```

- [ ] **Step 3: Full suite, then commit BEFORE build**

Run: `go vet ./... && go test -count=1 ./...`
Expected: green.

```bash
git add test/smoke/config.json test/smoke/smoke.sh
git commit -m "test: prove route pattern arrays end to end through the smoke config"
```

- [ ] **Step 4: Build + smoke + watchdog**

Run: `bash build.sh && bash test/smoke/smoke.sh && bash test/smoke/exe-watchdog.sh`
Expected: `BUILD_OK version=beta <Task-3-hash>-HHMM`, `SMOKE PASS`, `EXE TEST PASS`. If 18083/18084 are occupied: find the owner PID via netstat and `taskkill //F //PID` (never touch 8080/8081).

- [ ] **Step 5: Final Chinese acceptance report**

Report what was built, test/smoke/watchdog results, the commit list; user personally accepts the new exe and pushes. Never push.
