# Claude Code Empty-Search Guard Design

Date: 2026-11-10

## Problem

A Claude Code standalone search sub-agent request contains the marker `Perform a web search for the query:`. Some upstreams can return a syntactically complete Anthropic SSE response whose `web_search_tool_result.content` is `[]`, then nevertheless answer from prior knowledge. Production flight #1222 and isolated experiment A reproduced this: the response contained one empty search result, subsequent thinking/text, `message_delta`, and `message_stop`.

Prompt-only experiment B reduced the hard answer but did not provide a deterministic contract. The proxy must therefore enforce the outcome at the response boundary.

## Approved behavior

Add optional `claudeSearchGuard` to both `routes[]` and `search_fallback`. It is disabled when unset. It affects only native Anthropic-port requests carrying the exact standalone-search marker.

For an affected request:

1. Append this instruction after the marker, independently of (and in addition to) any configured summary instruction:

   > If the web search returns no results, state only that no results were found for this query. Do not answer from your own knowledge, and do not suggest alternative keywords.

2. Buffer a 200 `text/event-stream` response before releasing it downstream.
3. Parse every `content_block_start` event whose block type is `web_search_tool_result`. Trigger only when at least one such block exists and every such block has an exact empty-array `content: []`. A non-empty result, mixed empty/non-empty results, absent results, missing/non-array content, or malformed JSON is passed through conservatively.
4. On trigger, discard all client-bound upstream bytes, regardless of any thinking or prose following the empty result. Send one newly synthesized, complete Anthropic SSE stream containing only this text:

   > No results were found for this search query. Retry with different or broader keywords; do not fabricate an answer from prior knowledge.

The fixed text asks the Claude Code harness to retry but deliberately supplies no concrete replacement keywords; the main agent has the broader context.

## Per-route resolution

The Go fields are tri-state pointers so JSON `false` differs from an unset field:

```go
ClaudeSearchGuard *bool `json:"claudeSearchGuard,omitempty"`
```

- A search-capable route serving its own request uses its own value; unset means off.
- When a matched `no_search` route diverts to `search_fallback`, an explicitly set route value wins, including `false`; an unset route value inherits `search_fallback`'s value.
- `enhance_search` uses the owning route's value.
- Default upstream, classifier, fast route, and requests without a route match do not implicitly inherit `search_fallback`.
- Pattern-array desugaring copies the field like every other route field.

This allows a good upstream to remain unbuffered while a bad upstream is guarded, including different policies for routes sharing one fallback.

## Scope and compatibility

- Only native Anthropic streaming pass-through is guarded. Responses translation/native passthrough, non-SSE JSON, non-200 responses, unmarked requests, and `convertAlltoStream` collection are unchanged on the response path.
- A guarded response with real search results is byte-identical downstream to the existing transformed stream. Buffering intentionally delays its release until upstream EOF.
- Existing retries and keepalive pings remain valid: a synthesized stream may follow already-sent SSE ping events.
- No global switch is added.

## Synthesized protocol and accounting

The replacement contains, in order: `message_start`, text `content_block_start`, one `content_block_delta`, `content_block_stop`, `message_delta` with usage, and `message_stop`, with matching `event:` and `data:` lines. Its message model is the client's original model and its ID incorporates the flight ID.

Usage in the synthesized events reflects the real upstream usage, not an estimate of the fixed sentence. The existing forward loop parses the original upstream stream exactly once for proxy statistics. The synthetic stream is not fed through that parser again.

The upstream-side flight recording retains the exact discarded stream for diagnosis. The downstream-side recording contains only the actual synthetic bytes. The flight byte count is reset to the synthetic payload size. `message_stop` marks the synthesized response delivered.

A trigger logs `[search-guard]`. Finished-flight JSON adds `searchGuard: true`; the status column appends a red `[空搜]` badge, translated to `[empty-search]` in the English console. Existing `[无stop]` remains an independent upstream-terminal-event diagnosis.

## Tests and acceptance

- Unit tests cover exact empty-array detection, mixed/non-empty/malformed cases, and synthetic event/usage/model correctness.
- End-to-end tests cover route-direct and fallback activation, explicit-false override, guard-off compatibility, no-marker bypass, non-empty pass-through, hard-answer removal, one-time usage accounting, and recent-flight reporting.
- Full verification is `gofmt` (CI-equivalent whole repository), `go vet ./...`, `go test -count=1 ./...`, commit, `bash build.sh`, smoke, and exe watchdog.
- The isolated 18083/18084 instance then repeats experiment request A with the guard enabled. Acceptance requires the fixed short response instead of the reproduced 342 KB hard answer, a `[search-guard]` log, and a `[空搜]` status badge. Production 8080/8081 is not touched.
