# Route Pattern Arrays — Design

Date: 2026-10-30
Status: approved by user (approach A)

## Goal

Let one `routes[]` entry match several model-name patterns (OR semantics) so a route
serving several model families does not have to be copy-pasted:

```json
{
  "pattern": ["*fable*", "*opus*"],
  "url": "$KIMI_URL",
  "api": "$KIMI_API",
  "model": "k3.1-pro"
}
```

must behave exactly like two adjacent entries, one with `"pattern": "*fable*"` and one
with `"pattern": "*opus*"`, every other field identical. This equivalence is the whole
spec: the implementation literally desugars the array into N copies at load time.

## Approach: load-time desugar (user-approved approach A)

`parseConfig` (main.go) already rewrites the raw top-level JSON tree before struct
decoding (config vars expansion). Pattern expansion hooks into the same phase, right
after `expandConfigVars(top)` and before the final `json.Marshal(top)` /
`json.Unmarshal(full, &c)`:

```go
if err := expandRoutePatterns(top); err != nil {
    return nil, nil, err
}
```

`expandRoutePatterns(top map[string]json.RawMessage) error` rewrites `top["routes"]`:

- Parse `top["routes"]` as `[]json.RawMessage`. If the key is absent or does not parse
  as an array, leave it untouched (the final struct decode reports its usual error).
- For each entry (1-based position `i` for messages):
  - Not an object, no `pattern` key, `pattern: null`, or `pattern` not starting with
    `[` after whitespace trimming → keep the entry's raw bytes untouched. This
    preserves today's behavior for single-string, missing, and null patterns exactly.
  - `pattern` is an array:
    - Parse as `[]json.RawMessage`. Unparseable → leave for the final decode error.
    - Empty array → error `route #N pattern array is empty` (strict, like vars:
      silently dropping a route would hide a typo).
    - Each member must decode as a JSON string, otherwise error
      `route #N pattern must be a string or an array of strings`.
    - Emit N copies of the entry: shallow-copy the object map, set `pattern` to the
      member's original raw bytes, marshal. Copies are adjacent, in array order, at
      the entry's original position in `routes`.

Everything downstream is untouched by design: the handler route loop
(main.go:~4834), `matchPassthroughResponsesRoute` (main.go:~3992), `matchRouteRule`
(responses.go:~160), `hasCatchAllRoute` (main.go:~4046), `isReservedRoutePattern`
checks, enum validation, cache anchors (`convAnchor = "route:" + rr.Pattern`), and all
log/warning messages keep operating on plain string patterns.

### Console Codex menu (one non-Go consumer, found during planning)

The Config tab derives the Codex `/model` menu in browser JavaScript from the editor's
raw JSON (`deriveCodexModels`, logview.go:~2065), independent of `parseConfig`. It
skips non-string patterns, so without a change array-pattern routes would silently
vanish from the menu. The JS must flatten arrays exactly like the Go desugar: each
array member contributes its own representative name, in array order (duplicates
deduped by name as today). The reserved-name warning in `genCodexCmds`
(logview.go:~2143) likewise checks every array member and names the offending member.

### Consequences of the copy semantics (intended, not accidental)

- Route priority: an array entry occupies its original slot; its copies match in array
  order (first copy wins within the entry), exactly like hand-written duplicates.
- A `"*"` inside an array counts as a catch-all for `hasCatchAllRoute`.
- A reserved name (`Fallback`, `fast_route`) inside an array disables that copy only.
- `convAnchor` differs per matched pattern (`route:*fable*` vs `route:*opus*`), same as
  the hand-written equivalent. Existing single-string configs keep byte-identical
  anchors.
- Validation and log messages number routes after expansion; every message already
  prints the pattern string, so the offending entry stays identifiable.
- In-memory `Config` differs from the file (already true with vars). The web console
  reads and writes raw file text, so array syntax survives on disk verbatim; the save
  probe runs `parseConfig`, so array errors are rejected at save time with the same
  messages as at startup. Tray hot reload goes through the same funnel.

### Interaction with config vars

Vars expansion runs first, so `$NAME` references inside pattern arrays (and inside
every copied field) are already resolved before the copies are made. `"pattern":
["$P1", "$P2"]` is valid.

## Error surface

| Input | Result |
|---|---|
| `"pattern": "a"` | unchanged |
| `"pattern": ["a","b"]` | two adjacent copies |
| `"pattern": []` | load/save error: `route #N pattern array is empty` |
| `"pattern": ["a", 5]` | load/save error: `route #N pattern must be a string or an array of strings` |
| `"pattern": null` / absent | unchanged (matches nothing, as today) |
| `"routes"` not an array | unchanged raw; final struct decode errors as today |

## Testing (TDD: failing tests first)

New `TestParseConfigPatternArray` in main_test.go, subtests:

1. Array expands in place: order preserved, other fields identical across copies,
   neighboring routes keep their positions.
2. Both patterns match through `matchRouteRule` and return the same upstream fields.
3. Empty array → exact error message.
4. Non-string member → exact error message.
5. Single string, missing, and null patterns pass through untouched.
6. `"*"` inside an array satisfies `hasCatchAllRoute`.
7. Reserved name inside an array disables only that copy (`matchRouteRule` skips it,
   sibling copy still matches).
8. Vars references inside a pattern array are resolved before expansion.

Smoke end-to-end: `test/smoke/config.json`'s `smoke-sf` route switches to
`"pattern": ["smoke-sf", "smoke-sf-alt"]`; the existing smoke requests still hit
`smoke-sf`, so a green run proves array expansion on the real startup path. One
comment line in smoke.sh's header records this.

Full house chain after implementation: `go vet ./... && go test -count=1 ./...`,
commit before build, `bash build.sh && bash test/smoke/smoke.sh && bash
test/smoke/exe-watchdog.sh`; user accepts the new exe and pushes.

## Documentation

- `docs/usage.md`: array form documented next to the existing pattern/wildcard text.
- `docs/DESIGN.md`: one bullet describing the desugar semantics.
- `logview.go`: console routing-help paragraphs (the "routes match in array order"
  pair, zh + en) gain the array form; the Codex menu JS flattens arrays (above).
- `config.example.json`: unchanged (single-pattern entries stay the norm; the docs
  carry the array demo).

## Out of scope

- No changes to any matching/routing code path.
- No new matching features (regex, comma-separated strings, etc.).
- No deduplication or merging of array entries with neighboring routes.
