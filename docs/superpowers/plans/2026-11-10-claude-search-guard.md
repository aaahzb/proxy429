# Claude Code Empty-Search Guard Implementation Plan

> **For agentic workers:** Execute inline with TDD. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Deterministically replace an all-empty Claude Code standalone-search response with a fixed no-results Anthropic SSE stream, controlled per route and per search fallback.

**Architecture:** Resolve a tri-state route option during existing search dispatch and arm the flight only after the marker is confirmed. A native-Anthropic-only buffering ResponseWriter inside `forward` preserves the existing upstream parsing/transform path, then releases either the buffered client bytes or a synthetic stream. Statistics continue to parse only the original upstream bytes.

**Tech Stack:** Go standard library (`net/http`, `encoding/json`, `bytes`), existing handler/flight/status-page code, existing shell build and smoke rigs.

## Global Constraints

- Public code comments, docs, examples, and commit messages are English; console remains bilingual.
- Never touch production 8080/8081 traffic; isolated verification uses 18083/18084.
- No automatic `git push`; the user pushes after personal acceptance.
- Git Author and Committer are only the user's existing identity; no co-author trailers.
- Preserve CRLF in `logview.go` and `docs/DESIGN.md`; all new files are UTF-8.
- Before commit: whole-repo CI-equivalent `gofmt`, vet, and uncached full tests must pass. Commit before build so the binary hash matches HEAD.
- Add only explicit files to git; `Temp/` remains untracked.

---

### Task 1: Lock detection and synthesis with failing unit tests

**Files:**
- Create: `search_guard_test.go`
- Create after RED: `search_guard.go`

**Interfaces:**
- `onlyEmptySearchResults(raw []byte) bool`
- `searchGuardUsage{input, cacheRead, cacheCreation, output int64}`
- `synthesizeSearchGuardSSE(id uint64, model string, usage searchGuardUsage) []byte`
- Constants `searchGuardInstruction` and `searchGuardFixedText` with the exact wording in the spec.

- [x] Write table tests proving: one/multiple `content: []` triggers; no result, non-empty, mixed, missing/non-array content, and malformed JSON do not.
- [x] Write a protocol test asserting the six event types in order, exact fixed text, original model, flight-specific ID, and real usage in `message_delta`.
- [x] Run `go test -count=1 . -run 'Test(SearchGuard|OnlyEmpty)'` and observe compile failure because the implementation does not exist.
- [x] Implement only the detector and synthesizer. Parse `data:` lines with `encoding/json`; do not substring-match `[]`.
- [x] Re-run the focused tests and confirm PASS.

### Task 2: Add per-route configuration and marker injection

**Files:**
- Modify: `main.go` (`RouteRule`, `SearchRoute`, search dispatch)
- Modify: `search_guard_test.go`
- Modify: `config.example.json`

**Interfaces:**
- `NoBullshitOnEmptyClaudeSearch *bool ` + "`json:\"noBullshitOnEmptyClaudeSearch,omitempty\"`" + ` on both route types.
- Local `searchGuardEnabled bool`, `flight.searchGuardArmed bool`, and `flight.searchGuard bool` (actual replacement).
- `resolveSearchGuard(route *RouteRule, fallback *SearchRoute) bool`: route's non-nil value wins; otherwise fallback's non-nil value; default false. For direct route service pass a nil fallback.

- [x] Add failing config-resolution tests for unset, fallback inheritance, route true, and explicit route false beating fallback true.
- [x] Add failing end-to-end assertions that guard-only activation appends the exact short instruction; no marker injects nothing.
- [x] Add the fields and resolver; resolve in both the `needSearch && sf != nil` branch and the ordinary matched-route branch. Leave default/classifier/fast paths off.
- [x] In the existing marker block, arm the flight and combine guard + optional summary instructions using one `injectAfterMarker` call. Keep `searchLevel` tied only to summary injection.
- [x] Add example boolean fields without changing any other configuration semantics.
- [x] Run focused tests; confirm RED before implementation and GREEN afterward.

### Task 3: Buffer and physically replace only qualifying streams

**Files:**
- Modify: `search_guard.go`
- Modify: `main.go` (`forward` only)
- Modify: `search_guard_test.go`

**Interfaces:**
- `searchGuardWriter` implements `http.ResponseWriter` and `http.Flusher` by buffering status and client-bound bytes; `Flush` is deliberately a no-op until finalization.
- `observeUpstream([]byte)` captures exact upstream bytes for detection.
- `finish(searchGuardUsage)` writes either the unchanged buffered client stream or the synthetic stream.

- [x] Add failing native handler tests using a mock stream with `content: []` followed by a distinctive fabricated answer. Assert the client receives only the fixed text and never the fabricated answer/result block.
- [x] Add the complementary tests: guard off, non-empty result, no marker, non-200, JSON, and Responses-origin flight all retain existing response behavior.
- [x] Assert real upstream usage is aggregated exactly once, downstream byte count equals the actual synthetic size, upstream recording retains the discarded answer, and downstream recording contains only the synthetic stream.
- [x] Instantiate the writer only for armed, native-Anthropic (`translated == ""`), 200 SSE forwarding. Observe bytes at the start of `writeAndCount`; force downstream tee while armed.
- [x] Finalize after stripper tail handling and before `forward` returns. On trigger delete `Content-Length`, reset downstream recording and byte count, write the synthetic stream, mark delivered, set `flight.searchGuard`, and log `[search-guard]`.
- [x] Run focused tests, then the full package tests.

### Task 4: Status badge, documentation, and configuration examples

**Files:**
- Modify: `main.go` (`finishedFlight` and `addFinished`)
- Modify: `logview.go` (CRLF)
- Modify: `search_guard_test.go`
- Modify: `docs/DESIGN.md` (CRLF)
- Modify: `docs/usage.md`

- [x] Add a failing recent-flight test for `searchGuard: true` and assert the served JS contains `[空搜]` plus the English replacement `[empty-search]`.
- [x] Add the archived field/copy/JSON output and append the red badge to the finished status cell, independently of `noStop`.
- [x] Document tri-state route/fallback resolution, exact trigger and fixed text, buffering tradeoff, scope exclusions, usage/tee semantics, log, and badge.
- [x] Verify CR count equals LF line count for every CRLF file touched and run gofmt on `tr -d '\r'` output for `logview.go`.

### Task 5: Full gate, commit, binaries, smoke, and real reproduction

**Files:**
- Explicitly stage only the source/test/docs/example/spec/plan files changed above.

- [ ] Run whole-repo `gofmt -l .` plus CR-stripped checks for CRLF Go files; any output blocks commit.
- [ ] Run `go vet ./...` and `go test -count=1 ./... > /tmp/full-test.log 2>&1`; print and require the real exit code.
- [ ] Review `git diff --check` and the complete diff; stage explicit paths only.
- [ ] Commit with one English conventional message and no trailers. Verify Author, Committer, and full message with `git show -s --format=fuller HEAD`.
- [ ] Run `bash build.sh`, then `bash test/smoke/smoke.sh` and `bash test/smoke/exe-watchdog.sh`, capturing real exit codes without pipe masking.
- [ ] On the isolated 18083/18084 configuration, enable guard only for the Kimi-serving pattern, rebuild/restart the isolated exe, and replay `Temp/exp/reqA.json`. Assert fixed text, no fabricated answer, complete terminal events, `[search-guard]`, and `[空搜]`.
- [ ] Stop only the isolated test process by its verified port owner. Report binary version and all verification results for user acceptance; do not push.
