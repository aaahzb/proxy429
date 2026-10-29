# Config String Variables (vars + `$VAR` references)

Date: 2026-10-29
Status: approved (verbal), pending written-spec review

## Context

Upstream API keys (and base URLs) repeat across many config fields: every route
entry that points at the same upstream duplicates its `api` value, so rotating
one key means editing N places and hoping none is missed. The user asked for
config-level string variables: define a value once, reference it everywhere.

## Goals

- A config file can define named string variables and reference them from any
  string value in that file.
- Rotating a key becomes a single edit in one place.
- Strict failure on typos: a reference to an undefined variable is a config
  error (startup refusal, console save-probe refusal), never silently sent
  upstream as a literal `$NAME`.

## Non-goals

- No environment-variable fallback (decided: vars live in the config file only;
  each config file carries its own vars, so config switching stays
  self-contained).
- No escape mechanism for a literal `$NAME`-shaped text inside a value (none is
  needed in this config domain: keys, URLs, model names, patterns).
- No changes to any existing config key's structure or meaning; the only new
  top-level key is `vars`.

## Syntax

```json
{
  "vars": { "KIMI_KEY": "sk-adfaf123", "KIMI_BASE": "https://api.moonshot.cn" },
  "routes": [ { "pattern": "kimi*", "url": "$KIMI_BASE", "api": "$KIMI_KEY" } ],
  "search_fallback": { "url": "$KIMI_BASE/anthropic", "api": "$KIMI_KEY" }
}
```

- Reference forms: `$NAME` and `${NAME}`.
- Name charset: ASCII letters, digits, underscore; must start with a letter or
  underscore. Case-sensitive.
- `$` not followed by a well-formed reference (e.g. `"price: $5"`, `"a$"`) is
  literal text: no expansion, no error.
- `${` without a well-formed close is likewise literal.
- A well-formed reference to an undefined name is a hard error.

## Semantics

- Substitution happens in `parseConfig`, on the decoded JSON tree, before
  `json.Unmarshal` into `Config` and before enum validation. `parseConfig` is
  the single funnel for startup load, tray reload, and the web console save
  probe, so one hook covers every path.
- The walk expands JSON string **values** only; object keys are never touched.
  Scope is uniform: every string value (`api`, `url`, `model`, anything) is
  expandable. No per-field allowlist.
- Nesting: values inside `vars` are expanded with the same mapping, one pass,
  so a var may reference another var. Single-pass expansion makes reference
  cycles impossible by construction.
- Empty string is a legal var value and expands to `""` (consistent with the
  existing meaning of an empty `api`: pass the client's token through).
- A `vars` member whose value is not a string (number, object, ...) is a parse
  error naming that member.
- Configs without a `vars` section behave exactly as before (no `$` in the tree,
  nothing to expand).

## Error handling

- Undefined variable: parse fails with a path-annotated message, e.g.
  `undefined variable "KIMI_KET" in routes[0].api`. Surfaced identically at
  startup (config load refusal) and at the console save probe (HTTP 400 shown
  in the console message area).
- Non-string `vars` value: parse error naming the member (e.g.
  `vars.KIMI_KEY must be a string`).
- Web console: the config tab edits raw file text, so `$KIMI_KEY` references
  persist verbatim on disk and across save round-trips. No handler serializes
  the runtime `Config` back to the client, so resolved secret values never
  appear in any console response.

## Runtime surface

- `Config` gains `Vars map[string]string \`json:"vars,omitempty"\`` (inert
  after load; kept for documentation and validation).
- Custom scanner (~30 lines), not `os.Expand`: `os.Expand`'s mapping cannot
  report undefined names. The scanner follows `os.Expand` conventions for
  literal `$` handling and adds path-aware error reporting.

## Testing (TDD)

Unit tests on `parseConfig`:

1. Basic substitution in `routes[0].api`.
2. Braced `${NAME}` form.
3. Substitution in `url`; the same var referenced from two places.
4. Var referencing another var (single-level nesting).
5. Undefined var: error names the variable and the config path.
6. Non-string `vars` member: error names the member.
7. Literal preservation: `"$5"`, `"ends$"`, `"a $ b"` pass through unchanged.
8. Empty var value expands to `""`.
9. Existing suite stays green (configs without `vars` untouched).

Integration:

- `config.example.json` gains a `vars` example (self-contained: defines and
  references within the example), keeping `config_example_test.go` green.
- Smoke: switch one `api` in the smoke-generated config to a `$VAR` reference
  so the real binary proves end-to-end resolution.

## Docs

- `docs/DESIGN.md` and `docs/usage.md`: document the `vars` key and `$NAME` /
  `${NAME}` syntax with one example.
- Console config help (`logview.go`, bilingual): one short `vars` mention next
  to the other config-key descriptions.
