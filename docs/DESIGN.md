# Proxy429 Design Document

> This document is updated alongside the code, recording for each mechanism *why* it works this way and what pitfalls were hit along the way.
> The project overview and quick start are in the root README.md.

A local Anthropic-compatible HTTP proxy whose job is to absorb upstream 429 / 5xx responses — automatically retrying with exponential backoff so Claude Code never notices.

## How it works

Every Claude Code request goes to the local proxy (`127.0.0.1:8080`) first, and the proxy forwards it verbatim to the upstream model provider:

- **Verbatim pass-through**: an Anthropic-compatible endpoint; request/response headers and bodies are forwarded as-is, with no parsing or rewriting. Exceptions: classifier requests get thinking turned off; route hits rewrite the model name / upstream / API key; fast-route hits remove the speed field and inject response headers (see "Classifier requests: automatic thinking-off" and "Routing").
- **Automatic retry (three triggers)**:
  - **Case 0**: network-layer errors (connection failure, first-byte timeout) -> retry. If the client disconnects (ctx cancelled), retrying stops immediately and the request ends — no more burning upstream quota for nothing.
  - **Case A**: upstream HTTP status is `429/500/502/503/504` → retry.
  - **Case B**: status is **200**, but the response body hides an error event (`event: error` / `rate_limit` / `overloaded` / `"type":"error"` etc.) → also retry. This matters — some upstreams (including some Anthropic-compatible services) don't return a 429 status when rate-limiting; they return 200 and stuff the error into the SSE stream. Looking only at the status code would miss it.
  - **Case C**: normal response → pass straight through; record first-token latency / streaming duration / output_tokens into the sliding window.
- **Total budget**: `total_budget_s` caps the total retry duration; past it, the proxy gives up and passes the last response through to Claude Code, so Claude Code doesn't hit its own timeout first.
- **Internal resend on first-byte timeout**: `upstream_header_timeout_s` (default 70s) bounds how long the proxy waits for the upstream's first byte. On timeout the request is considered stuck and is **resent internally** (no error reported to Claude Code); only when retries are exhausted is a 503 passed through for Claude Code to retry itself. Note this only bounds waiting for the first byte — it never cuts a streaming body (long outputs unaffected).
- **Streaming pass-through**: every chunk read is `Flush`ed, guaranteeing incremental SSE output instead of clumping. When sniffing for errors the proxy only peeks at the beginning of the response body; unconsumed bytes are pushed back and forwarding continues — no data lost.
- **Upstream compression disabled**: the proxy forces `Accept-Encoding: identity` so the response body is plaintext and error-string matching works; the cost is slightly more bandwidth.

## File structure

| File | Role |
|------|------|
| `main.go` | The proxy itself (including `resolveConfigPath` config-path resolution, `reloadConfig` hot reload, `logRing` in-memory log buffer) |
| `tray.go` | Cross-platform tray / menu bar (`fyne.io/systray`): menu (「查看日志」/Open console, 「切换配置」/Switch config submenu, 「退出代理」/Quit proxy — identical on all platforms), status-light icon, tooltip, status polling |
| `logview.go` | Web console (the only UI on every platform): mounted at `/__logs`, with Status/Logs/Config tabs; same-port local routes `/__logs/data`, `/__config`, `/__reload`, localhost-only |
| `main_test.go` and the other `*_test.go` files | Unit tests (SSE parsing / status sampling / tray light & tooltip / translation layers) |
| `config.json` | Runtime config (listen address, upstream, retry policy, classifier switches, etc.). Auto-generated from `//go:embed config.example.json` on first run when absent |
| `config.example.json` | The embedded config template used to generate `config.json` on first start |
| `go.mod` | Go module definition |
| `README.md` | Documentation |
| `test/mock_429.go` | Local mock server for testing (see "Local testing") |
| `test/config_test.json` | Config for local testing |
| `release/Proxy429.app` / `release/proxy429.exe` / `release/proxy429` | Build artifacts (`build.sh` outputs per host platform; darwin is packaged as `.app`, windows uses the GUI subsystem, linux is a bare binary; ignored by `.gitignore`, never committed) |

## Configuration (config.json)

The example below uses a Volcano ARK Coding Plan upstream:

```json
{
  "listen": "127.0.0.1:8080",
  "upstream": "https://ark.cn-beijing.volces.com/api/plan",
  "max_retries": 5,
  "base_delay_s": 0.5,
  "max_delay_s": 20,
  "total_budget_s": 120,
  "retry_status_codes": [429, 500, 502, 503, 504],
  "respect_retry_after": true,
  "upstream_header_timeout_s": 70,
  "log_request_detail": false,
  "recent_sample_window": 20,
  "convertAlltoStream": false,
  "responses_listen": "",
  "routes": []
}
```

- **listen**: local listen address:port — Claude Code connects here.
- **upstream**: the upstream ARK Anthropic-compatible Base URL. It already carries the `/api/plan` prefix; Claude Code's own `/v1/messages` gets appended, producing `https://ark.cn-beijing.volces.com/api/plan/v1/messages` (field-tested to return a 401 auth error, proving the path is correct). May be empty when `routes` contains a `pattern:"*"` catch-all (every model-bearing request is taken over by the catch-all route and the default upstream is never used); with an empty upstream and no catch-all, unrouted requests get a straight 502 and config load/reload logs a warning.
- **max_retries**: maximum retry count.
- **base_delay_s / max_delay_s**: exponential backoff's starting wait and cap (seconds).
- **total_budget_s**: total retry budget (seconds); give up past it.
- **retry_status_codes**: status codes that trigger a retry.
- **respect_retry_after**: whether to honor the upstream `Retry-After` header first.
- **(removed keys)**: the old top-level `classifier_thinking_disabled` / `classifier_max_tokens` / `translateNone2Low` were removed — `classifier_thinking` moved into `classifier_route`, and `translateNone2Low` became the per-route `convertOff2Low` (see below). A config still carrying any of the three **loads and runs normally** (the removed keys stay inert; their old behavior is not emulated) but raises non-fatal warnings: one startup-log line per key naming the replacement (`[config] WARNING: config key "classifier_thinking_disabled" was removed: set "classifier_thinking": "off" inside classifier_route instead (the key is inert; the proxy runs without its old behavior)` — visible in the console's Logs tab) and a **red tray icon** whose tooltip points to the console Docs (which describe the migration). Only the web console's save still refuses removed keys (HTTP 400, before anything is written), so they can't be saved back to disk from the editor.
- **upstream_header_timeout_s**: longest wait for the upstream's first byte (seconds). Past it the request is considered stuck and is **resent internally** (no error to Claude Code); only exhausted retries yield a 503 for Claude Code to retry itself. Default `70`. Using a first-byte timeout rather than a whole-request timeout only bounds "waiting for the response to start" without cutting long streaming outputs.
- **ping_interval_s**: interval (seconds) between SSE ping keepalives sent to the client during 429 retries. While the proxy is retrying, the client receives no upstream data, and a long silence triggers Claude Code's timeout with an API error; periodic pings keep the connection alive. Default `5`; `0` means default. See "429 retry keepalive".
- **log_request_detail**: whether to print each request's stream/tools/system prefixes (for diagnosing classifier fingerprints; default off).
- **recent_sample_window**: how many recent requests the Status tab's "first token" and "tok/s" cards sample (sliding window). Default `20`; larger is smoother, smaller is more responsive. Only normally-passed-through (case C) streams are counted.
- **convertAlltoStream**: global stream-ification switch (default `false`). When on, every non-streaming request (`stream:false` or omitted) is silently rewritten to streaming before going upstream — the web console shows tokens arriving live, with first-token latency and tok/s stats just like ordinary streaming requests. The caller notices nothing: once the upstream stream is fully collected, the proxy **rebuilds** a non-streaming JSON exactly as streamed (all content blocks reassembled as they appeared, including search-result `encrypted_content`) and returns it in one shot — the caller still gets the non-streaming response it expected. If the stream breaks midway (no `message_stop` seen), nothing has been written to the client and the proxy retries the whole request. Only applies to Anthropic Messages requests (`/v1/messages`); already-streaming requests and search summary mode are unaffected. See "Global stream-ification".
- **responses_listen**: OpenAI Responses API listen port (empty = disabled; the config template demos `127.0.0.1:8081`). Set it and the proxy opens an extra Responses API endpoint (`/v1/responses`) at that address, translating Responses-protocol requests into Anthropic Messages through the main pipeline (routing / retries / web monitoring all apply), and translating responses back to the Responses protocol. For Codex CLI and other Responses-only tools to reach Anthropic upstreams. Takes effect on save+reload (the listener starts/stops dynamically with the config). See "Responses API listener".
- **routes**: model routing rules; each request's model name is wildcard-matched against `pattern`, and a hit reroutes to the given upstream (URL/API/model swap). Unset or empty array means no routing — everything goes to the default `upstream`. See "Routing".
- **classifier_route**: dedicated route for classifier requests (an object, sibling of `routes`). Requests matching the classifier (safety check) are routed to the given `url`/`api`/`model` regardless of their original model; unset means classifier requests still follow `routes` by model (compatible). See "Routing".
- **fast_route**: dedicated route for fast-mode requests (an object, sibling of `routes`). Non-classifier requests carrying `"speed":"fast"` are routed to the given `url`/`api`/`model`; unset means no intervention (compatible). See "Routing".
- **multimodal_fallback**: multimodal fallback route (an object, sibling of `routes`). When a request carries an image but hits a `text_only` text-only model, it automatically falls back to the given `url`/`api`/`model`; unset means no fallback (passed through to the text-only model, for the upstream to deal with). See "Routing".
- **search_fallback**: search fallback route (an object, sibling of `routes`). When a request carries a search tool but hits a `no_search` upstream that doesn't support search, it automatically falls back to the given `url`/`api`/`model`; unset means no fallback. See "Routing".
- **log_file**: log file path (optional). **Default empty**: logs go only to the in-memory ring buffer (`logRing`, 500 lines, polled by the web console's Logs tab) + stderr; under the windowsgui subsystem or with no terminal, stderr is a no-op, i.e. nothing hits disk. A non-empty value additionally appends to this file — handy for keeping logs around, or for copy-reading in console-less scenarios like RemoteApp. Changing it requires a proxy restart (the web Config tab's save+reload does not reopen the log file).

## Usage

### 1. Start the proxy

The binaries `build.sh` produces live under `release/` (see "Rebuilding"); double-click or start from a shell — after startup it lives in the tray / menu bar and **no terminal window needs to stay open**:

```powershell
# Windows (GUI subsystem, no console window)
.\release\proxy429.exe
```
```bash
# macOS (.app menu-bar app, no Dock icon)
open release/Proxy429.app
# Linux
./release/proxy429
```

The startup log (containing `proxy started vc639d56-1606: listening http://127.0.0.1:8080 -> forwarding to https://ark.cn-beijing.volces.com/api/plan (max retries 5, classifier thinking-off=true)`, where `v` is followed by the version = git short hash + build HHMM) goes to the in-memory ring buffer and can be read on the web console's Logs tab; it's also written to `log_file` when that is non-empty.

> By default `resolveConfigPath` resolves the config path as: `-config` flag > `./config.json` in the current directory (if present) > `os.UserConfigDir()/proxy429/config.json` (macOS `~/Library/Application Support/proxy429/`, Linux `~/.config/proxy429/`, Windows `%AppData%/proxy429/`). On first run without a config, one is generated from the embedded `config.example.json`. For local mock testing use `-config test/config_test.json` — see "Local testing" below.
>
> After startup a status-light icon appears: in the menu bar on macOS (`.app` packaging, `LSUIElement=true`, no Dock icon), in the system tray on Windows/Linux. The right-click menu is identical across platforms: 「查看日志」 (Open console — opens the web console in a browser), 「切换配置」 (Switch config — a submenu listing the config files in the config directory, the current one ticked, plus 「刷新列表」/Refresh list), and 「退出代理」 (Quit proxy). Windows builds use the GUI subsystem (`-H=windowsgui`): no console window on launch, pure tray operation.

### 2. Point Claude Code at the proxy

In a separate terminal. **ARK requires `ANTHROPIC_AUTH_TOKEN`** (it becomes `Authorization: Bearer xxx`, the only header ARK accepts; using `ANTHROPIC_API_KEY` gets you 401s):

PowerShell:
```powershell
$env:ANTHROPIC_BASE_URL = "http://127.0.0.1:8080"
$env:ANTHROPIC_AUTH_TOKEN = "the API Key from your ARK console"
claude
```

CMD:
```cmd
set ANTHROPIC_BASE_URL=http://127.0.0.1:8080
set ANTHROPIC_AUTH_TOKEN=your-api-key
claude
```

macOS / Linux (bash/zsh):
```bash
export ANTHROPIC_BASE_URL="http://127.0.0.1:8080"
export ANTHROPIC_AUTH_TOKEN="the API Key from your ARK console"
claude
```

### Rebuilding (after changing main.go)

Use the root `build.sh` — it injects the version (git short hash + build HHMM) into the binary and outputs the artifact for the host platform into `release/`:

- **darwin** -> `release/Proxy429.app` (`LSUIElement=true` menu-bar app, no Dock icon) + ad-hoc codesign (first launch needs Finder right-click "Open" to pass Gatekeeper)
- **windows** -> `release/proxy429.exe` (linker `-H=windowsgui`, GUI subsystem, no console window, pure tray operation)
- **linux** -> `release/proxy429`

It also copies `docs/usage.md` (when present) into `release/`.

```bash
bash build.sh
```

> Don't run bare `go build` — the version would be `dev`, the web console/startup log would show `vdev`, and you couldn't tell which build is running. `build.sh` is essentially equivalent to:
>
> ```bash
> # macOS (tray via cgo, needs clang — macOS ships it)
> CGO_ENABLED=1 go build -buildvcs=false -ldflags "-X main.Version=$(git rev-parse --short HEAD)-$(date +%H%M)" -o release/proxy429 .
> # Windows (tray is pure Go, no C compiler; -H=windowsgui selects the GUI subsystem, no console)
> CGO_ENABLED=0 go build -buildvcs=false -ldflags "-X main.Version=$(git rev-parse --short HEAD)-$(date +%H%M) -H=windowsgui" -o release/proxy429.exe .
> # Linux (tray via D-Bus, pure Go)
> CGO_ENABLED=0 go build -buildvcs=false -ldflags "-X main.Version=$(git rev-parse --short HEAD)-$(date +%H%M)" -o release/proxy429 .
> ```
>
> **Cross-platform and cgo**: of the tray library `fyne.io/systray`, only macOS needs cgo (Cocoa/AppKit, satisfied by the macOS-bundled clang); Windows and Linux are pure Go (Linux via D-Bus) and cross-compile directly without a C toolchain. I.e. `GOOS=windows CGO_ENABLED=0 go build` and `GOOS=linux CGO_ENABLED=0 go build` produce those platforms' binaries right on macOS.

## Key passing (ARK-specific)

The proxy passes headers through verbatim. ARK's Anthropic-compatible endpoint **only accepts the `Authorization: Bearer` header** (verified via the 401 error code `AuthN_MissOrInvalidAuthorizationHeader`), which corresponds to Claude Code's `ANTHROPIC_AUTH_TOKEN` environment variable. **Do not use `ANTHROPIC_API_KEY`** (it becomes the `x-api-key` header, which ARK doesn't accept — perpetual 401s).

## Troubleshooting: the proxy "does nothing" / no retry logs visible

Current versions have **full request logging**: every incoming request prints a `[request]` line, every upstream response prints `[attempt N] upstream response status: X`, and retries print `[retry] status X; waiting Y before retry (budget left Z)`. So locating the problem is one glance:

**Start the proxy, run `claude` in another terminal, then watch the web console's 「日志」 (Logs) tab (tray right-click 「查看日志」 / "Open console", or open `http://127.0.0.1:8080/__logs` in a browser):**

- **Seeing `[request] #N POST /v1/messages model=xxx (body=N bytes) from ...`** → the request entered the proxy and the proxy is working. Read on to the upstream status lines:
  - `[attempt N] upstream response status: 429` followed by `[retry] status 429; waiting ... before retry (budget left ...)` → retrying normally; if it still ends in 429, retries were exhausted — raise `max_retries` / `total_budget_s` in `config.json`.
  - `[attempt N] upstream response status: 200` followed by `[error] status 200 but response body contains an error: ...` → the upstream stuffed the rate-limit error inside a 200 body, and the proxy is retrying that too (case B).
  - `[attempt N] upstream response status: 401` → wrong auth method; switch to `ANTHROPIC_AUTH_TOKEN` (see below).
- **No `[request]` lines at all** → **Claude Code isn't going through the proxy at all**; its 429s come straight from ARK. This is the most common cause of "the proxy does nothing" — fix it per below.

- **HEAD probes** (Claude Code sends `HEAD /`, `HEAD /api/hello` at startup to probe connectivity): the proxy answers 200 directly — no flight created, nothing forwarded upstream, no `[request]` log. Not seeing these in the log or stream list is normal and doesn't mean the proxy isn't working.

### Common reasons Claude Code misses the proxy

1. **The environment variable wasn't set in the terminal that runs `claude`.** You must `$env:ANTHROPIC_BASE_URL=...` in the same PowerShell window before `claude`; a different window doesn't have it.
2. **`~/.claude/settings.json` pins an `ANTHROPIC_BASE_URL` straight at ARK** (common if you followed ARK's official tutorial). Shell environment variables override this file, but if you didn't set them in the shell, claude goes straight to ARK. Either delete/change it to the proxy address, or set the env var in the shell every time.
3. **claude is logged into an Anthropic account.** `claude /logout`, then authenticate via the `ANTHROPIC_AUTH_TOKEN` environment variable.
4. **How to verify**: before running `claude`, run `echo $env:ANTHROPIC_BASE_URL` (PowerShell) or `echo %ANTHROPIC_BASE_URL%` (CMD) in the same terminal — it must print `http://127.0.0.1:8080`.

### Don't pick the wrong auth

ARK only accepts the `Authorization: Bearer` header → you must use `ANTHROPIC_AUTH_TOKEN`, **not** `ANTHROPIC_API_KEY` (which becomes the `x-api-key` header; ARK returns 401 with error code `AuthN_MissOrInvalidAuthorizationHeader`). The proxy passes both through fine — but ARK only takes Bearer.

## Classifier requests: automatic thinking-off

Before running Bash, Claude Code does a "safety classification" with the model. If that classification request carries thinking, the model ponders for a long time (possibly 30s+), hits Claude Code's timeout, and reports `glm-5.2 is temporarily unavailable, auto mode cannot determine the safety of Bash` — every Bash call blocked.

With `"classifier_thinking": "off"` set on `classifier_route`, the proxy **recognizes this classifier request and force-disables its thinking**, so classification returns in 2-3 seconds:

- **Recognition**: strict match on the system-prompt prefix only (default `You are a security monitor`) — normal conversation is never collateral. A `bytes.Contains` pre-screen means normal requests skip JSON parsing entirely; overhead is negligible.
- **Rewrite**: on a hit (and only with `"classifier_thinking": "off"` on `classifier_route`; unset = the request's thinking passes through untouched), sets `thinking:{type:"disabled"}` + `reasoning_effort:"none"` + deletes `reasoning` (three fields as belt-and-braces, covering both Anthropic and OpenAI formats). `max_tokens` is never touched.
- **Key order preserved**: the rewrite uses `json.Decoder` streaming to locate the target fields' byte positions in the original body, then does textual replacement — only `thinking`/`reasoning_effort`/`reasoning` are touched; every other byte (key order, spacing, formatting) is preserved as-is, with no wholesale `Unmarshal`+`Marshal` (that would make Go reorder all keys alphabetically and could hurt upstream cache hits). Fields absent from the original body (like `reasoning_effort`) are appended before the closing `}`.
- **Content-Length**: the rewritten body is longer, and the proxy recomputes the length from the new body (`copyHeaders` skips the original Content-Length) — no truncation.

### Confirming it works

1. Run `claude`, trigger a Bash operation, and check the web console's 「日志」 tab for a `[rewrite] classifier signature hit; thinking disabled` line. Present → the classifier went through the proxy and got rewritten.
2. If there's no `[rewrite]` line but Bash is still slow/failing: set `log_request_detail` to `true` in `config.json`, trigger again, and look at what the non-streaming request's `sys=` prefix actually is in the `[detail]` log. The classifier prefix is hardcoded as `You are a security monitor` (the `classifierSystemPrefix` constant in main.go); if a Claude Code upgrade changes the prefix, edit this constant and recompile.
3. If ARK's glm-5.2 doesn't honor `thinking:{type:"disabled"}` (rewritten but classification still slow), there's currently no perfect fix — all three fields are already sent together and extras are ignored by the upstream. Start by watching whether Bash still reports unavailable after the `[rewrite]` line.

> Note: this only cures "classifier too slow because of thinking, hitting the timeout". If the classifier returns 429/503 (rate limit), that's the retry logic above — the two mechanisms are independent.

ARK's Base URL carries the `/api/plan` prefix, but since Claude Code brings its own `/v1/messages` path, the proxy's `Upstream + r.URL.Path` join yields exactly `…/api/plan/v1/messages` — ARK's correct endpoint (field-tested). So **as long as the upstream expects the standard Anthropic path `/v1/messages` appended to its Base URL, the current join logic needs no change**.

Only when an upstream expects a path that isn't simply "Base URL + `/v1/...`" (say `/anthropic/messages` instead of `/anthropic/v1/messages`) does the join logic in `main.go` need editing.

## Routing

Routes requests to different upstreams by request model name (swapping URL + API key + model name), with multiple rules and `*` wildcards. **When `routes` is unset (or an empty array), nothing is routed at all — every request goes to the default `upstream`, behaving exactly as if the feature didn't exist.**

Config example (reroute `claude-opus*` requests to DeepSeek, renaming the model to `deepseek-V4-pro`):

```json
"routes": [
  {
    "pattern": "claude-opus*",
    "url": "https://api.deepseek.com",
    "api": "sk-deepseek-xxx",
    "model": "deepseek-V4-pro",
    "text_only": true
  }
]
```

- **pattern**: model-name wildcard; only `*` is supported (matches any length of any characters, including empty). `claude-opus*` hits `claude-opus-4-8`/`claude-opus`; `*opus` matches a suffix; `claude-*` matches a prefix; `a*b*c` requires b in the middle. No `*` means exact match. Rules match in array order, **first hit wins** — there is no "more specific first" sorting, and a wide wildcard intercepts a narrow one: with `*opus*` written before `*opus-4*`, `claude-opus-4-8` hits `*opus*` first and `*opus-4*` never fires; put more specific patterns earlier.
- **url**: target upstream Base URL (overrides the default `upstream`). Claude Code's `/v1/messages` is appended, with the same join rule as the default upstream.
- **api**: target API key, set as the `Authorization: Bearer` header. On a hit, the client's original `Authorization` and `x-api-key` are **deleted** (avoiding leaking an ARK token to DeepSeek etc.) before the new key is set. Empty = pass through the client's original token.
- **model**: the target model name to substitute (rewrites the request body's `"model"` field value; Content-Length is recomputed for the length change). Empty = don't change the model.
- **text_only**: boolean marking the target model as **text-only**. When `true`, a request carrying an image automatically falls back to `multimodal_fallback` (see "Image routing"). Unset or `false` = no fallback.
- **no_search**: boolean marking the target upstream as **not supporting search**. When `true`, a request carrying a search tool automatically falls back to `search_fallback` (see "Search routing"). Unset or `false` = no fallback.
- **enhance_search** (optional object; omitted = disabled): setting `{}` or any sub-field enables **enhanced search** — when the main upstream supports search (`no_search` not `true`), requests carrying a search tool don't call the main model; instead the route's own `url`/`api`/`model` runs the kimi summary mode (see "Enhanced search"). Sub-fields: `summary_thinking` (default `false`) — whether step 2's summary thinks; `summary_level` (default `low`) — summary verbosity `low`/`mid`/`high`/`max`, same levels as `search_fallback.summary_level`.
- **url_response_api** (implemented, not yet field-tested — deliberately left out of the usage docs): string, the Base URL of a native Responses API upstream. Once set, Responses-listener requests hitting this route skip translation entirely, and the Responses original goes straight to that upstream. Documentation will be completed after field verification.
- **thinking**: string declaring the **target model's** thinking shape (omitted or `"auto"` = look up the built-in mapping table by the client-sent model name). **Only effective for Responses-port translated streams** — Anthropic-port clients send Anthropic format directly and the proxy never rewrites their thinking fields (the one exception: a `convertOff2Low:"all"` route quietly upgrading an explicit thinking-off — see the next bullet). Why it exists: translation decides `adaptive+effort` vs `enabled+budget_tokens` using the client model name (route model rewriting happens after translation), so an alias named `claude-fable-5` that actually routes to Kimi would be misjudged as adaptive and sent to the target upstream. Set `"budget"` to force classic `enabled+budget_tokens` (DeepSeek/Kimi etc.); set `"adaptive"` to force `adaptive+effort` levels (when the client name isn't in the mapping table but the target really is an adaptive model). Any other value fails config load with an error.
- **convertOff2Low**: `"translate"` / `"all"` (unset = off; any other value fails config load). With it, an **explicit thinking-off** request (`thinking.type:"disabled"`, or `reasoning.effort` none/off/disabled at the translation port) is quietly sent upstream as **low thinking** — adaptive models get `{"type":"adaptive"}` + `output_config.effort:"low"`, budget models `{"type":"enabled","budget_tokens":2048}` capped at half of max_tokens (abandoned, staying off, when the 1024 floor doesn't fit) — and thinking blocks are **stripped from the response**, so the client still sees a thinking-off reply (usage stays truthful); the status page shows a two-tone `[off->low]` badge (`off` in the downstream vocabulary's color, `low` in Anthropic orange). `"translate"` = only the Responses translation port (the route is pre-matched by the client-sent model name; the Codex menu's `fast_route` name takes `fast_route`'s own value); `"all"` adds the Anthropic native port (the finally effective route's value — after an image/search fallback swap, the fallback route's value applies; a fallback route's value never reaches translation-port requests, since translation precedes the swap). Also mountable on `fast_route` / `multimodal_fallback` / `search_fallback` (not on `classifier_route` — classifier thinking is governed by `classifier_thinking`). Motivation: Kimi's docs state thinking-off requests route to the K2.8 Preview no-thinking model — keeping low on avoids that downgrade. If the upstream rejects thinking (a 200 in-stream error mentioning thinking), one automatic retry goes out with thinking genuinely off (nothing to strip; the badge truthfully shows `off`). Logs: `[off->low] #N explicit thinking-off quietly upgraded to low thinking (body X->Y bytes); thinking blocks stripped on return`; on the budget guard `[off->low] #N upgrade abandoned: max_tokens too small for the 1024-token budget floor; request stays thinking-off`; at stream end `[off->low] #N stripped N thinking block(s) from the response (client sees thinking-off)`.
- **allowNoThinkBlock4Anthropic**: top-level bool (unset/nil = **true**; Responses translation port only — the native port never checks history). Governs what happens when a tool continuation's history has no replayable signed thinking block and the request would otherwise enable thinking. **true** (default, try-first): the requested thinking mode is sent as-is and a one-shot fallback is armed — if the upstream really rejects it (real Anthropic 400s thinking over such a history; the rejection surfaces either as an HTTP 400 JSON error or a 200 in-stream `"type":"error"`, both caught by the same site), the handler retries once with thinking off without burning the backoff budget, logging `[fallback] #N upstream rejected thinking over a no-thinking-block history (HTTP 400); reverted to thinking-off and retried`; the stealth-upgrade rejection at the same site logs `[fallback] #N upstream rejected the low-thinking upgrade (HTTP 200); reverted to thinking-off and retried`. **false** (cc-switch): thinking preemptively off over such histories (adaptive models that can disable get `thinking:"disabled"`; budget models send no thinking field) — false also wins over convertOff2Low's history try-on door. Explicit thinking-off requests never take this door (the convertOff2Low stealth upgrade still governs those); models that can't disable thinking (fable-5/mythos-5) still error 400; requests that wouldn't enable thinking anyway don't arm the fallback (no wasted round trip).

A hit logs a `[route]` line, e.g. `[route] #1 claude-opus-4-8 -> https://api.deepseek.com (model claude-opus-4-8 -> deepseek-V4-pro)`; the `[request]` line still shows the pre-route original model name. Retries after a route hit keep going to the same target upstream (URL/API/model unchanged). The web console's 「配置」 (Config) tab re-reads `routes` on save+reload — hot-effective.

> Routing only changes the three items URL/API/model; apart from `convertOff2Low`'s explicit-off upgrade (previous bullet) it doesn't rewrite other request-body fields like thinking or messages (the `thinking` route parameter only decides the thinking shape when the Responses translation constructs a new request body — see above); the classifier's thinking-off logic runs before routing, and the two don't interfere.

**Response model write-back**: after a route rewrites the request model, the `"model"` field value in the upstream's SSE response (which may be the upstream's actual model ID, e.g. `glm-5-2-260617`, not necessarily matching the `glm-5.2` in the request) is **located by field position and replaced with the original model name** — no string matching involved. This way Claude Code always sees the model it sent, and never fails with `Session model ... could not be restored` on restart because it saved an upstream model name. The rewrite logs `[rewrite] #N response stream model rewritten back <upstream actual model> -> <original model>`, showing the model name the upstream really returned. This applies to model routes, classifier routes, and fast routes alike.

### Classifier route (classifier_route)

Model-name routing above is for splitting normal conversation traffic. There's also a routing mode **only for classifier requests**: regardless of the request's original model, as long as it matches the classifier (Claude Code's safety-check request before tool calls, matched by system-prefix), it's routed to the designated upstream. Good for dumping these lightweight safety checks onto a cheap model to save main-model quota.

Config (sibling of `routes`; an object, not an array):

```json
"classifier_route": {
  "url": "https://api.deepseek.com",
  "api": "sk-deepseek-xxx",
  "model": "deepseek-v4-flash",
  "classifier_thinking": "off"
}
```

- **url / api / model**: same meanings as the like-named fields in `routes`. With `url` empty the classifier is **not** rerouted (it follows `routes`/the default upstream as if no classifier route existed) — handy for configuring `classifier_thinking` alone.
- **classifier_thinking**: `"off"` = rewrite classifier requests to thinking-off (the feature in "Classifier requests: automatic thinking-off" above); unset = the request's thinking passes through untouched. Only `"off"` is supported; any other value fails config load with an error.
- **Priority**: on a classifier hit with `classifier_route` configured, the original model is **ignored** — the classifier route is taken and `routes` is not consulted. On a classifier hit **without** `classifier_route`, it falls back to model-based `routes` matching (compatible with old behavior).
- **Independence**: the rerouting (url/api/model) and the thinking-off rewrite (`classifier_thinking`) switch independently within the same object — either works without the other.
- A hit logs `[route] #N classifier <original model> -> <url> (model <original> -> <target>)`, tagged classifier to distinguish it from ordinary model routes. Also hot-reloaded via the web console.

> The classifier route's judgment (system-prefix match) uses the same recognition logic as the `[rewrite]` thinking-off rewrite, but the classifier route only decides and never rewrites the body — the two switch independently.

### Fast route (fast_route)

> **Prerequisite**: Claude Code's `/fast` only supports the official Anthropic API by default. When using it through a third-party proxy, you must first set `penguinModeOrgEnabled: true` to enable it — manually add `"penguinModeOrgEnabled": true` in `~/.claude.json`.

Claude Code's `/fast` mode adds a `"speed":"fast"` field to the request body and an `Anthropic-Beta: fast-mode-2026-02-01` request header. When the fast route detects this field, non-classifier requests are uniformly sent to the designated upstream.

```json
"fast_route": {
  "url": "https://api.deepseek.com",
  "api": "sk-deepseek-your-key",
  "model": "deepseek-v4-pro"
}
```

- **Trigger**: the request body contains `"speed":"fast"` and it's not a classifier request.
- **Rewrite behavior**: on a hit, **remove** the `"speed":"fast"` field (the upstream doesn't support it), **delete** the `Anthropic-Beta` request header (the upstream doesn't know it), and **inject fake fast rate-limit headers** into the response (`anthropic-fast-output-tokens-remaining: 999999` etc.) so Claude Code believes fast mode is available.
- **Priority**: classifier route > **fast route** > model route. A classifier request never takes the fast route, even with `"speed":"fast"`.
- **Unconfigured**: nothing happens — `"speed":"fast"` and the `Anthropic-Beta` header pass through to the upstream as-is.
- A hit logs `[route] #N fast <original model> -> <url> (model <original> -> <target>)`, tagged fast. Also hot-reloaded via the web console.

### Image routing (multimodal_fallback)

Some upstream models are text-only (e.g. DeepSeek-V4) and error out on image-bearing requests. Image routing solves this: mark the text-only target rule with `text_only: true`, configure a multimodal-capable fallback upstream, and when the proxy detects a request carrying an image that hits a text-only model, it automatically reroutes to the fallback upstream.

Config (sibling of `routes`; an object):

```json
"routes": [
  {
    "pattern": "claude-opus*",
    "url": "https://api.deepseek.com",
    "api": "sk-deepseek-xxx",
    "model": "deepseek-V4-pro",
    "text_only": true
  }
],
"multimodal_fallback": {
  "url": "https://your-multimodal-upstream.example.com",
  "api": "sk-mm-xxx",
  "model": "kimi-vl",
  "no_search": false
}
```

- **text_only** (inside a `routes` entry): marks that entry's target model as text-only.
- **url / api / model** (inside `multimodal_fallback`): the fallback multimodal upstream; same meanings as the like-named fields in `routes`.
- **Trigger**: the request body contains an Anthropic image content block (`{"type":"image",...}`) **and** the hit `routes` rule has `text_only: true` **and** `multimodal_fallback` is configured **and** the request carries no search tool (search-bearing goes to `search_fallback`). All four at once.
- **Rewrite behavior**: on fallback, URL/API switch to `multimodal_fallback`'s values and the request body `"model"` is rewritten to the fallback model name. The response's `"model"` is still **written back to the original model name** (same as ordinary routing — see "Response model write-back" above), so Claude Code still sees the model it sent.
- **No-fallback cases**: no image in the request; the hit rule has `text_only` false/unset (the target model handles multimodal itself); `text_only: true` set but no `multimodal_fallback` configured (degrades to passing through to the text-only model for the upstream to deal with); the request carries a search tool (goes to `search_fallback` instead). Classifier and fast routes don't participate in image fallback.
- A hit logs `[route] #N image-fallback <original model> -> <fallback url> (model <original> -> <fallback model>)`, tagged image-fallback. Also hot-reloaded via the web console.

### Search routing (search_fallback)

Some upstreams don't support search (e.g. Volcano), others do (e.g. DeepSeek, Kimi). Mark searchless target rules with `no_search: true` and configure a search-capable fallback upstream; when the proxy detects a request carrying a search tool that hits a searchless upstream, it automatically reroutes to the fallback.

"Carrying a search tool" specifically means the request `tools` contains an **Anthropic server-side** `web_search_*` tool (e.g. `web_search_20250305`, executed by the upstream). **The client-side `WebSearch` tool is deliberately not recognized** — Claude Code includes its definition in every request, so it can't distinguish "really searching" from "just defined" and would misjudge every conversation as a search request. So this fallback mainly serves clients that use server-side search, like Claude Desktop; Claude Code CLI performs searches itself and doesn't pass through this fallback.

Config (sibling of `routes`; an object):

```json
"routes": [
  {
    "pattern": "claude-opus*",
    "url": "https://volces.example.com",
    "api": "sk-volc-xxx",
    "model": "ark-opus",
    "no_search": true
  }
],
"search_fallback": {
  "url": "https://api.deepseek.com",
  "api": "sk-deepseek-xxx",
  "model": "deepseek-search",
  "text_only": true
}
```

- **no_search** (inside a `routes` entry): marks that entry's target upstream as not supporting search.
- **url / api / model** (inside `search_fallback`): the fallback search-capable upstream; same meanings as the like-named fields in `routes`.
- **summary_mode** (inside `search_fallback`, optional, default `false`): `true` enables search summary mode (next section); `false` keeps the old behavior (the whole request is transferred to the fallback upstream).
- **summary_thinking** (inside `search_fallback`, optional, default `false`): whether step 2's summary thinks under `summary_mode`.
- **summary_level** (inside `search_fallback`, optional, default `low`): summary verbosity under `summary_mode`. `low` = short summary (`max_tokens=2048`); `mid` = medium detail with key facts and data points (`4096`); `high` = thorough, with all data points/citations/context (`8192`); `max` = on top of `high`, steps/methods/code/formulas must be restated completely and verbatim (`16384`; `full` is a synonym alias).
- **Trigger**: the request carries a search tool **and** the hit `routes` rule has `no_search: true` **and** `search_fallback` is configured.
- **Rewrite behavior**: same as image fallback — URL/API/model rewritten, response model written back to the original name.
- A hit logs `[route] #N search-fallback <original model> -> <fallback url> (model <original> -> <fallback model>)`, tagged search-fallback. Also hot-reloaded via the web console.

#### Search summary mode (summary_mode)

Default (`summary_mode` unset or `false`): on hitting a `no_search` upstream, **the whole request transfers** to the `search_fallback` upstream (which supports search itself and answers directly). The answer comes from the fallback model, not the main one.

`summary_mode: true` takes a different path: **the main model stays**, the search upstream serves as a "search + summarize attendant", and the proxy builds a Kimi-format response in two steps returned straight to the client — the main ARK is never called:

1. **Step 1, search**: send the original request (the `web_search` tool, model swapped to `search_fallback.model`) to the search upstream, non-streaming, getting back `server_tool_use` + `web_search_tool_result` (titles/URLs).
2. **Step 2, summarize**: feed step 1's results back to **the same search upstream** as context, streaming a plaintext summary of each result. Verbosity is controlled by `summary_level` (`low`/`mid`/`high`/`max`, default `low`), with `max_tokens` growing per level (2048/4096/8192/16384); the `max` level restates steps/methods/code/formulas completely and verbatim. Optional `summary_thinking: true` turns thinking on for better quality.
3. **Assemble and return**: build an SSE stream of `server_tool_use` -> `web_search_tool_result` -> summary text in Kimi format and return it to the client.

This keeps Claude Desktop's dropdown search list (which reads the `web_search_tool_result` titles/URLs) and the model's answer (which reads the summary text) both working, with summaries more detailed than the upstream's built-in ones. If step1/step2 fails, it automatically degrades to transferring the whole request to `search_fallback` (the old behavior) — no error, no interruption.

Applies when: the search upstream supports `web_search` but its built-in summaries aren't detailed enough, or you want the summary format under your control. Note the search upstream must return standard `server_tool_use` + `web_search_tool_result` blocks (Kimi can; DeepSeek depends on the API).

```json
"search_fallback": {
  "url": "https://api.kimi.com/coding/",
  "api": "sk-kimi-xxx",
  "model": "kimi-for-coding",
  "summary_mode": true,
  "summary_thinking": false,
  "summary_level": "low"
}
```

- A hit logs `[route] #N search-summary mode <original model> -> <fallback url> (model <original> -> <fallback model>)` and `[searchsum] #N ...` lines.

### Enhanced search (enhance_search inside routes)

`search_fallback.summary_mode` above handles the fallback scenario "the main upstream doesn't support search". **Enhanced search** handles the reverse: the main upstream **does** support search, but you don't want its built-in search — you want the proxy to answer with the kimi summary mode (step1 search + step2 summary).

Add an `enhance_search` object inside a `routes` entry (omitted = disabled):

- **Trigger**: the request carries a search tool **and** the hit `routes` rule doesn't have `no_search` as `true` (i.e. it supports search) **and** that rule has `enhance_search` configured.
- **Behavior**: the main model isn't called; **the route's own `url`/`api`/`model`** runs the kimi summary mode (same step1+step2+self-built response as above), with summary parameters from that route's `enhance_search.summary_level`/`enhance_search.summary_thinking`.
- **Versus `search_fallback.summary_mode`**: the latter is the fallback when `no_search:true`, using the upstream configured in `search_fallback`; the former is a search-capable main actively switching to summaries, using the upstream configured on the `routes` entry itself. Mutually exclusive: `no_search:true` goes `search_fallback`; `no_search:false` + `enhance_search` goes enhanced search.
- **Degradation**: on step1/step2 failure, degrades to transferring the whole request to that route's upstream (the main passes through normally).

```json
"routes": [
  {
    "pattern": "claude-haiku*",
    "url": "https://api.deepseek.com",
    "api": "sk-deepseek-xxx",
    "model": "deepseek-V4-pro",
    "enhance_search": {
      "summary_thinking": false,
      "summary_level": "mid"
    }
  }
]
```

- A hit logs `[route] #N enhanced search <original model> -> <route url> (model <route model>)`.

### Image + search in the same request

If a request carries both an image and a search tool, the proxy **treats it as search**: the whole request goes `search_fallback` (two-step summary under `summary_mode`), regardless of the image. There's no evidence a "multimodal + search" combination occurs in practice, so no dedicated "sees images and searches" fallback is sought; if the search upstream doesn't accept images, sending one over may make the upstream error, and the proxy passes that through.

- Search request -> always `search_fallback` (image or not).
- Pure image request (no search) -> `multimodal_fallback`; unconfigured = pass through to the original route (if the upstream rejects images, the proxy passes the error through).

> Capability fallbacks only trigger in the model-route branch; classifier and fast routes don't participate. With neither fallback configured, behavior reverts to the original.

## Global stream-ification (convertAlltoStream)

**Non-streaming requests** like the classifier and search step1 show no live token flow on the web console and get no first-token/tok-s stats — the upstream returns one whole JSON non-streamed, the proxy passes it through as-is, and the page only sees "arrived all at once".

With `convertAlltoStream` (top-level config, default `false`) on, the proxy silently rewrites **all** non-streaming requests to streaming before sending them upstream:

- **Request side**: for `/v1/messages` requests with `stream:false` (or omitted), the body's `stream` field is rewritten to `true` before going upstream (streaming field location + textual replacement; no other field touched).
- **Web monitoring**: as the upstream answers with SSE, the proxy tees while receiving into the in-flight streams page — live token flow, first-token latency, streaming duration, and tok/s stats exactly like ordinary streaming requests.
- **Return side (client notices nothing)**: the proxy collects the entire stream (until `message_stop`), then **rebuilds** the non-streaming message JSON exactly as streamed — all content blocks (text, thinking, `server_tool_use`, `web_search_tool_result` including `encrypted_content`, `tool_use` arguments, etc.) reassembled as they appeared in the stream — and returns it in one shot as `application/json`. The caller never knows its non-streaming request was stream-ified.
- **model write-back**: for requests whose model was route-rewritten, the rebuilt JSON writes the model field back to the client's original model (consistent with streaming pass-through).
- **Reliability**: if the stream breaks midway (no `message_stop` seen), **no byte has been written to the client**, and the proxy resends the whole request per the retry cadence (resending the streaming request, collecting the stream once more); during retry waits **no SSE ping keepalive is sent** (it would pollute the non-streaming response — waits are silent). Exhausted retries pass through a 502.
- **Scope**: only applies to Anthropic Messages requests (`/v1/messages`); already-streaming requests and search summary mode (self-built responses) are unaffected. If the upstream doesn't answer with a stream (returns plain JSON), it's passed straight through.

## Responses API listener (responses_listen)

`responses_listen` (top-level config; empty = disabled, config-template demo value `127.0.0.1:8081`) opens an extra **OpenAI Responses API** endpoint at the given address, connecting Responses-only tools (Codex CLI etc.) to any Anthropic upstream:

```json
"responses_listen": "127.0.0.1:8081"
```

- **How to connect**: point the tool at `http://127.0.0.1:8081/v1`, POSTing per the Responses protocol to `/v1/responses` (`/responses` also accepted). The request's model name participates in main-pipeline route matching as usual — add a matching pattern (e.g. `gpt-5*`) in `routes` to choose which Anthropic upstream it goes to and what model it's rewritten to.
- **Codex CLI setup**: one click does it — below the web console's 「配置」 (Config) tab, two terminal commands for Windows / macOS·Linux are **generated live** from the editor contents above (same format as DeepSeek's docs: the address comes from `responses_listen`; one representative name per `routes` pattern is written into Codex's `/model` menu — a pure `*` catch-all route's representative is fixed as `Fallback`; no `pattern` may be literally named `Fallback` or `fast_route` — reserved names; a colliding route neither takes effect nor enters the menu, and the web page warns in red; with `fast_route` configured and carrying a `model`, the menu gains a `fast_route` entry that takes the fast lane when selected; the dropdown selection becomes the default model; the script is baked and served by the proxy itself, zero-interaction), then paste into the corresponding terminal and hit enter. Or run the interactive `codex-setup.ps1` (Windows) / `codex-setup.sh` (macOS/Linux) from the repo root (modeled on DeepSeek's official script: backup, then surgical config.toml edits, model catalog written, choose 9 to restore). Manual setup: edit `~/.codex/config.toml` (Windows: `%USERPROFILE%\.codex\config.toml`) — top-level `model_provider = "proxy429"`, `model = "gpt-5-codex"` (participates in route matching and thinking lookup), `preferred_auth_method = "apikey"` and `forced_login_method = "api"` (no official-account login), plus a `[model_providers.proxy429]` section: `base_url = "http://127.0.0.1:8081/v1"`, `wire_api = "responses"`, `experimental_bearer_token = "any placeholder"` (the proxy doesn't validate the token; the real key is injected by the route's `api`). Same structure as what cc-switch writes when taking over Codex; restart Codex afterwards (config.toml isn't hot-loaded). Watch the system-proxy trap: with a Windows system proxy on, Codex uses it and ignores its exception list — requests to `127.0.0.1` get hijacked to the proxy server and 503 (with zero log on this proxy's side). The Windows one-click script already writes a user-level `NO_PROXY` (including `127.0.0.1`) to bypass this; the macOS/Linux scripts only check and suggest `export NO_PROXY=...`; for manual setup run `setx NO_PROXY "localhost,127.0.0.1,::1"` yourself and restart Codex. Multi-model switching: add `[profiles.name]` sections each setting `model` (start with `codex --profile name`) or ad-hoc `codex --model name`; the proxy routes by model name and needs no changes. A step-by-step tutorial is in `docs/usage.md`, "Getting Codex CLI on the proxy".
- **Translation**: `instructions`/system messages → `system`; the flat `input[]` is re-nested into Anthropic messages (`function_call` merged into assistant tool_use, consecutive `function_call_output`s merged into one user's tool_result; incomplete tool turns are dropped wholesale, a leading non-user message gets an auto-inserted leading user); `max_output_tokens` → `max_tokens` (default 32000).
- **thinking mapping** (identical to cc-switch 3.20.0's thinking_optimizer): `reasoning.effort` takes one of two paths by model class — adaptive models (fable-5/mythos-5/mythos-preview/sonnet-5/opus-4-8/4-7/4-6/sonnet-4-6, substring match) translate to `thinking:{"type":"adaptive"}` + `output_config.effort` (low/medium/high/max), of which fable-5/mythos-5/mythos-preview/sonnet-5 default on even without effort; fable-5/mythos-5 can't disable thinking — an explicit `effort:"none"` translates to adaptive + `effort:"low"`. Other models translate to `thinking:{"type":"enabled","budget_tokens":N}` (low 2048 / medium 8192 / high 16384 / xhigh·max·ultra 24576, capped at half of max_tokens, not opened under 1024). A tool-continuation turn missing signed-thinking replay, or thinking conflicting with a forced tool_choice, degrades/errors per cc-switch's same rules. The lookup uses the client-sent model name (before route rewriting). The full mapping table is in `docs/usage.md`, "Responses translation mapping".
- **Tool system** (aligned with cc-switch 3.20.0): function tools and the `web_search` managed tool → Anthropic tools (web_search maps to `web_search_20250305` — cc-switch drops it instead); `custom` freeform tools (like Codex's apply_patch) → wrapped into a `{"input": string}` JSON Schema with the original tool definition inlined into the description, responses unpacked back into `custom_tool_call` (streaming via the `custom_tool_call_input.done` event); `namespace` (MCP) tools → sub-tools flattened to `ns__name` (truncated plus a sha256 suffix past 64 bytes), responses restored to function_calls carrying the `namespace` field; `tool_search` → fixed proxy tool. Image media in tool results (MCP image blocks, JSON-string nesting, whole-string data URLs) is automatically stripped into native Anthropic image blocks instead of being stringified; `input_file` → document block (attachments in unrecognized shapes — blob:/file: local URLs, file_id cloud references, etc. — serialize into text as fallback, never silently dropped); `tool_choice` mapped in all shapes (required/auto/none/function/custom/tool_search; unknown shapes degrade to auto); the `pages:""` quirk Anthropic models attach to Read tool calls is auto-cleaned.
- **Responses**: Anthropic content blocks translate back into Responses events/objects in real time — text → message items (`output_text.delta`), thinking → reasoning items (summary text + `encrypted_content`), tool_use → function_call/custom_tool_call/tool_search_call items (identity restored via the tool registry), search results → `web_search_call` items; usage merged (cache reads count into `input_tokens_details.cached_tokens`, cache writes into `cache_write_tokens`). A client with `stream:true` gets the SSE event stream; `stream:false` gets a one-shot JSON. In responses from Kimi (k3-256k etc.) when the request carries the web_search tool, search-echo lines starting with `Search results for query: ` (together with the query that follows) are deleted whole by the translation layer — the same rules apply to what's forwarded to the client and to history replayed upstream, preventing query echoes from accumulating in the context and inducing repeated same-kind searches; text blocks stripped to empty are dropped wholesale, and empty search structures (no-argument server_tool_use, empty web_search_tool_result, etc.) are neither produced nor replayed in either direction; `web_search_call` call items replayed from client history (even with query/sources) never go upstream either — their ids are proxy-minted (`ws_` + response id), never registered in the upstream search registry, so going upstream must 400 and drag the envelope-restored real search pair into being fail-soft stripped along (production evidence: a follow-up just 33 seconds after the search blocks were born was already rejected); the sole replay carrier of search content is the search envelope. When the model imitates history by gluing several bare preambles onto the body's start, the preambles are still stripped bare, keeping the body. Stripping is deterministic plain-text rules applied unconditionally per request per message (echo-free text isn't touched by a single byte), the request body stays consistent turn over turn, and prefix caching is unaffected. The client UI no longer piles up search-echo lines; real searches still translate as `web_search_call` items with result links (sources) as usual.
- **Thinking-block envelopes**: Anthropic signed thinking blocks are base64 self-encapsulated into the reasoning item's `encrypted_content` (prefix `p429-ant-thinking-v1:`); when the client replays history next turn they're restored into thinking blocks sent upstream — the thinking chain across multi-turn tool calls isn't lost, and it's self-contained, not relying on the upstream to decrypt.
- **Search-block envelopes**: one search's paired `server_tool_use` + `web_search_tool_result` blocks (including all `encrypted_content` bodies) are likewise self-encapsulated into a reasoning item (prefix `p429-ant-search-v2:`) sent to the client with the response; at sealing time the call block's id is normalized to the result block's `srvtoolu_` id — Kimi streaming search gives the call block a `tool_`-prefixed id that the search registry never registered, so replaying it verbatim must 400 with `tool_call_id is not found` (controlled experiment: the same body with the id rewritten returned 200); non-streaming search has the two ids naturally consistent and is unaffected. When the client replays it verbatim next turn, it's restored into complete Anthropic search blocks going upstream — the model reads last search's body directly from them, and follow-ups no longer re-search the same keyword (the `web_search_call` call items in client history are display-only and not replayed — see the previous paragraph). The envelope carries attribution info (upstream url + key hash + the model name at sealing time), and the whole payload is XOR-obfuscated with a mask derived from the api key (prefix `p429-ant-search-v2:`; obfuscation, not encryption, for performance — no plaintext url/key info lies in client-side history; a wrong key XORs into non-JSON and is naturally skipped, so changing keys automatically invalidates old envelopes): when this request's route prediction and the envelope aren't same-origin, restoration is skipped (it couldn't be de-obfuscated anyway — saves tokens); a different model doesn't block — field-tested decrypting fine cross-model on the same endpoint and key. The envelope carries its sealing moment, but the proxy sets no fixed TTL ceiling — field tests showed a search id sealed 1.7 hours prior still alive, and a fixed ceiling would kill live envelopes. Instead there's a per-conversation adaptive watermark: when after restoration the upstream reports `400 tool_call_id` (the search id registry has a TTL; an old conversation's search blocks may have expired), the proxy automatically strips the replayed search blocks and retries immediately (not consuming the retry budget, the 400 not passed to the client) — an imperceptible degradation to "not restored", and the model re-searches if needed; at the same time, the oldest sealing moment among the stripped envelopes is recorded as that conversation's "watermark" (monotonically rising), and from then on envelopes no newer than the watermark are not restored at all (proactively stripped — no 400 hit, no retry spent) — the registry expires by age, so same-age and older ones are certainly dead, and the watermark converges on the true TTL with every fallback. Old ts-less envelopes (first v2) count as oldest: stripped when a watermark exists, optimistically restored without one. Streams whose blocks were stripped show a red `[strip N]` on the status page (Chinese UI: `[剥N]`; N = total replayed search blocks stripped this stream, watermark strips and 400-fallback strips combined): in-flight streams carry it on the model column, recent-finished streams after the cache-hit rate (e.g. `81%[strip 2]`); how many were watermark strips versus 400-fallback strips is broken down in the `[strip]`/`[fallback]` log lines. Empty searches (no query) get no envelope.
- **Pipeline reuse**: the translation layer hands the request internally to the main `/v1/messages` handler, and upstream traffic is always streaming — routing (pattern/classifier/fast/multimodal/search fallback), 429 retry keepalive, and the web console's in-flight monitoring and statistics all apply as usual.
- **Native passthrough (`url_response_api` on a routes entry)** (implemented, not yet field-tested — deliberately left out of the usage docs): when the hit route carries this field, Responses-port requests skip translation and the original goes straight to the native Responses upstream named by the field. Documentation will be completed after field verification.

**Data source of the status page's 「看请求体」/「看返回体」 (view request/response body)**: request and response bodies are recorded per link side, and the viewer's 「链路」 (link) button switches sides. The default 代理↔上游 (proxy↔upstream) side: the request body is what the proxy actually sent upstream (for translated streams, the translated Anthropic body; for rewritten native streams, the rewritten body), and the response body is the raw stream from upstream. The 下游↔代理 (downstream↔proxy) side: the request body is what the client sent the proxy verbatim, and the response body is what the client actually received. Only streams whose two sides differ are stored twice — Responses translated streams always differ (one body per protocol); for convertAlltoStream-rebuilt JSON streams, the downstream-side response body is the rebuilt one-shot JSON; native streams only get a separately stored downstream-side request body when the request body was rewritten (classifier thinking-off, route model rewrite, etc.). Switching to a side with no record automatically falls back to the other side, with a hint beside the button (the endpoint truthfully reports the actual side via the `X-Proxy429-Side` response header). Both observation points sit on the proxy's boundary: what the user wants to know is "what did the client send/receive" and "what did the proxy actually send to/receive from upstream".

- **Limits**: changes take effect on save+reload (the listener starts/stops dynamically, unlike the main `listen` which needs a restart); localhost-only, same as the main port; a busy port only warns and disables the listener without affecting the main proxy. Not supported: computer-use-style computer-operation tools (no corresponding client or upstream; cc-switch doesn't support them either), `store:true` server-side state, and `/v1/models` listing.

## 429 retry keepalive (SSE ping)

When the proxy auto-retries on 429/5xx, it sends no bytes to Claude Code during the retries (no successful response is connected yet). A streaming Claude Code request that receives no data for a long time triggers the client timeout, reports "API error", and starts its own retry 0/10 — while the proxy is still retrying on its side, the two working independently.

To avoid this, on the **first retry** the proxy sends the client a `200` + SSE stream header, and during backoff waits sends an Anthropic-standard `event: ping` keepalive every `ping_interval_s` seconds. Receiving pings, Claude Code considers the connection alive and doesn't time out. After a successful retry, the upstream's normal SSE stream (with `message_start` etc. following the pings; the client ignores pings) picks up seamlessly.

- **Interruptible backoff**: retry waits use `select` to watch for client disconnect; on client timeout/cancel the proxy stops retrying immediately (the old `time.Sleep` wasn't interruptible — the client was gone and it kept sleeping, burning upstream quota).
- **Fallback on exhausted retries**: after the `200` keepalive header has been sent, the status code can no longer be changed to pass a 429 through, so an SSE `event: error` (`overloaded_error`) is sent instead for Claude Code to recognize the error. Ordinary 429s are retried successfully by the proxy for now and never trigger this; only persistent rate-limiting that exhausts retries lands here.
- **Zero impact on normal requests**: without 429s no ping is ever sent; the original pass-through logic runs without a single extra byte.
- Logs: `[keepalive] #N retrying (status 429); SSE ping keepalive on` / `[keepalive] #N client disconnected; stopping retries`.

> This mechanism assumes Claude Code's timeout is a "no-data timeout" (resettable by pings). If Claude Code still times out with ping keepalive on in practice, it's a different timeout type and needs further investigation.

## Web console

The proxy's **only UI** is a browser-based web console, identical across platforms (no more terminal split-screen TUI). It's mounted at the `/__logs` path on the proxy's own port, localhost-only (`isLocalRequest` restricts to `127.0.0.1`/`::1`/`localhost`; remote requests get 403, so even with `listen` on `0.0.0.0` exposing the port to the LAN, no logs/config leak). Three tabs:

```
Status cards (example; actually rendered as a web page):
v c639d56-1606  active 3 | waiting 1 | bytesForward 2.3KB | rate 12KB/s
cacheRead 0 | input 24 | output 80 | retries 0 | classifiers 0
avgFirstByte 1.23s | tps 45.6

In-flight streams:
#  model      stage       bytes   status
1  glm-5.2    forwarding  4.2KB   200
2  glm-5.2    awaiting    0B      -
3  glm-5.2    awaiting    0B      -
```

- **状态 (Status)**: status cards + the in-flight streams table. Card fields: `active`/`waiting` (in progress / awaiting first byte), `bytesForward` (total forwarded bytes), `rate` (bytes per second, computed from the delta between polls), `cacheRead`/`input`/`output` (cumulative tokens parsed from SSE `usage`), `retries` (cumulative retries), `classifiers` (cumulative requests matching the classifier fingerprint — counted whether or not rerouted / thinking-off; hover the card for the hit/thinking-off pair, click for an enlarged breakdown), `classifierNoThink` (of those, how many actually got thinking-off rewrites), `avgFirstByte` (mean first-token latency over the last `recent_sample_window` requests), `tps` (weighted token throughput). In-flight table columns: `#`/model/stage/bytes/status. The first column's status light (⚪ request / 🟡 awaiting first byte / 🟢 forwarding) shows the seconds the current color has lasted, zeroed only when the color changes (routing and repeated retries inside a yellow light don't re-zero); while waiting for the first byte inside a yellow light, between the light and the total duration there's also `[attempt N: Xs]` (Chinese UI: `[尝试N: Xs]`) = how long the current attempt has waited (re-counted per attempt; during retry backoff it shows `[backing off]` / `[退避中]`), and the yellow light's total = sum of all attempts + backoffs. The recent-finished streams table's 「缓存年龄」 (cache age) column shows, per session+route, how long ago that session's most recent cache write on that route was (m:ss counting up; the upstream cache's true TTL is dynamic — this column no longer guesses a countdown, it only says how long ago this cache was written; for whether it's still usable, judge against the measured ranges in the 「缓存命中」 popup) — the newest stream of a session+route shows an age, older rows with the same key show `-` after refresh, and streams without a session identifier (count_tokens probes, bare API calls without metadata) always show `-`; age counts from the stream's start, and the session identifier comes from client-request fields (Claude Code's `session_id` inside `metadata.user_id`, Codex's `prompt_cache_key`) — the proxy only reads, never modifies; a 499 stream disconnected by the downstream during the yellow-light (awaiting first byte) stage doesn't refresh the cache (this row shows `-`, the anchor stays on the previous same-key stream); a 499 disconnected during the green-light (streaming) stage refreshes as usual (this row becomes the new anchor); while a same-session same-route in-flight stream is producing output (green forwarding), the cache was actually just refreshed, so that key's latest finished row freezes as `[m:ss]` (inside the brackets, the old anchor's age at refresh time, unchanging; the column header's question mark has a hover explanation), and the new anchor takes effect when that stream finishes; clicking the 「缓存命中」 (cache hit) card opens a popup whose bottom 「实测缓存时间」 (measured cache time) table lists each upstream's measured cache survival time (model, URL, lower bound ≥, ≥ formed, upper bound <, < formed, observation count; grouped by URL+model name, other parameters ignored: when two adjacent same-session streams see the latter at ≥95% hit rate, it records one lower-bound observation "lived at least the interval" (max kept); when an earlier stream hit and a later one falls below 50%, it records one upper-bound observation "didn't live the interval" (min kept) — strict zeroing isn't required, residual hits on common prefixes like the system prompt don't count as alive; when the two sides contradict (the upstream's cache time changed mid-way or was evicted early), the newer observation wins, the refuted side is discarded and re-measured, and its observation count zeroes and restarts; the "≥ formed"/"< formed" columns = how long ago each bound's value was established (m:ss counting up) — a bound's value changing (including discard-and-re-measure) restarts its clock, merely adding supporting observations with the value unchanged doesn't reset it, and a bound with no observations shows - along with the bound; measurements are display-only, purely in-memory, cleared on restart or 「清空统计」/clear-stats); a session+route's latest finished stream whose start was within the last 5 minutes isn't pushed off the list by "keep N finished streams" (row count may exceed N). The in-flight and recent-finished tables' 「API」 column merges protocol origin and thinking value into one cell: the name = protocol origin (orange `[Anthropic]` = native Anthropic port, purple `[translate]` = Responses port translated), and the `[value]` after the name = the shortest form of the thinking config **actually sent upstream** (the final state after translation mapping, classifier thinking-off, and all other proxy rewrites), **its color = the vocabulary** (thinking targets the upstream: upstreams always receive Anthropic format, so a purple name is followed by an orange value) — orange = Anthropic thinking (either direct-as-is or after translation mapping); `off` = thinking disabled or effort none/off (shown as `关` in the Chinese UI), `on N` = enabled+budget_tokens N (shown as `开 N` in the Chinese UI), `adaptive` = adaptive without a level, `low`/`high`/`max` etc. level words = adaptive effort, no `[value]` = the request carried no thinking field; e.g. Codex sending effort high translated to an Anthropic upstream shows `[translate][on 16384]` (purple name, orange value). When search summary mode triggers, step1/step2 appear as independent sub-streams in the in-flight table (model column labeled `search-step1·<model>`/`summary-step2·<model>` — language-independent labels), and move into 「最近完成的流」 (recently finished streams) when done, clickable to inspect the passthrough content. In the finished-stream status column, non-200 statuses display bracketed (e.g. `[499]`, `[400]`) so abnormal streams stand out; a stream that exhausted retries/budget gets a fallback error event passed downstream (overloaded_error) and shows `[retries exhausted]` (Chinese UI: `[重试尽]`); a stream that had backoff retries gets a gold `[retried Nx]` appended after the status (N = retry count, e.g. `200[retried 2x]`; also attached with `[retries exhausted]`, so you can check against `max_retries`); 499 = the connection ended before the upstream finished sending its response (most commonly the downstream actively cancelled — the cancellation propagates into an upstream disconnect; upstream providers' dashboards also record 499, so both sides agree; the nginx convention "client closed request"); when the downstream disconnects only after the upstream finished in full (Codex closing the connection right after response.completed), it's still recorded as 200, consistent with the upstream dashboard. Both in-flight streams (growing live as forwarding proceeds) and recent-finished streams list the tools called in that stream's response after the model column as `[Read*1][Edit*3]` (in gold) — server-side tools like `web_search` count too, in first-appearance order; N calls of the same name collapse to `*N` (the raw count), a single call shows `*1`, and a single call with an empty argument object shows `*0` — e.g. Kimi empty-search no-arg calls, telling empty searches from real ones at a glance. Clicking an in-flight/finished row replays that stream: the view button cycles four states — request body → 输出[渲染] (rendered: the SSE event stream parsed into readable text, the default landing) → 输出[流式原始] (raw stream: verbatim SSE) → 输出[非流式] (non-stream: the event stream folded into the final JSON structure — Anthropic streams accumulate onto the message_start skeleton, Responses streams take the response.completed object; an incomplete stream gets a partial-snapshot note; 「交互式JSON」 in this state trees the assembled object) — then back to request body; the request-body state replays the request body that caused the stream (JSON auto-prettified, non-well-formed JSON shown verbatim); request and response bodies are both recorded per link side, switchable via the 「链路」 (link) button (default 代理↔上游 side: the request body actually sent upstream and the raw stream back from it; the other side 下游↔代理: what the client sent/actually received; identical sides aren't double-stored, and switching to a side with no record falls back automatically with a hint beside the button). Browsing always caps at the first 256KB; with 「储存完整结构体」 (store full payloads; default off, resets on restart) ticked, newly started requests additionally record the complete request body and output (uncapped, in memory), and the viewer gains 「下载请求体/下载输出」 (download request body / output) buttons for the complete files (saved JSON-prettified, non-JSON verbatim); unticking immediately clears the stored full copies and the download buttons vanish. The 「交互式JSON」 (interactive JSON) button is always available (the full-store switch is not required): it renders the content currently in view as a key-collapsible JSON tree (all collapsed by default, click a key line to lazily expand; SSE event-stream outputs are parsed into an event array first, and in the non-stream state the assembled final object is treed; when the side's full copy is recorded the tree fetches it, otherwise it trees the on-screen first-256KB copy), and it follows link-side switches. Streams with incomplete data are never silently treated as complete: 「下载请求体」 is greyed when only a truncated request body remains (hover for why), 「下载输出」 is greyed without a full output copy, and interactive JSON refuses only a truncated request body (unparseable as complete JSON) — outputs are treed from whatever copy is on screen. Responses translated streams always differ between sides and are always double-stored (the downstream side holds the client's Responses original and the actual reply; the upstream side holds the translated Anthropic body and the upstream's raw SSE). The 「清空统计」 (clear stats) button clears the cumulative stats above and the recent-finished list (in-flight streams and the # numbering are not cleared, to avoid colliding with in-flight numbers; the in-memory log has its own 「清空日志」/clear-log button).
- **日志 (Logs)**: the last 500 log lines (`logRing` in-memory ring buffer), auto-scrolls to the bottom with sticky scrolling (scrolling up manually pauses following; returning to the bottom resumes). No paging keys / wheel conflicts — pure native browser scrolling.
- **配置 (Config)**: the config-file editor. Loads the current `config.json` contents (`GET /__config` returns `{path, content, exists}`); saving `POST /__config` first `json.Unmarshal`s into `Config` to validate JSON legality — **illegal JSON gets a straight 400 and is never written to disk** (avoiding a broken config on disk that would fail the next startup); only legal JSON is written and then `reloadConfig()` hot-applies; there's also a 「仅重载」 (reload only) button whose `POST /__reload` just calls `reloadConfig` without touching the file.

The page polls `/__logs/data` every 500ms (returning recent logs + full status counters + the in-flight stream list as JSON). **Closing the browser tab merely hides it — the proxy keeps running unaffected.**

**Status card field meanings**:
- **v-version**: the exe's version = git short hash + build HHMM (e.g. `c639d56-1606`), for confirming which exe is running. Only injected when built with `build.sh`; a bare `go build` shows `dev`.
- **active / waiting**: requests currently in progress (active = sent upstream, forwarding or awaiting retry; waiting = in the first-byte stage).
- **bytesForward / rate**: cumulative forwarded bytes + a once-per-second byte rate. It grows with every SSE chunk forwarded — the direct feel of "tokens pouring out".
- **cacheRead / input / output**: cumulative tokens parsed from SSE `usage` (`cache_creation` cache writes are also tracked; ARK's `usage` has no `cache_creation_input_tokens`, counted as 0). The 「缓存命中」 (cache hit) card = cache_read / (input + cache_read + cache_creation), consistent with Claude Code's cache-hit algorithm: cache writes don't count as hits but do count into total input — omitting them would inflate the hit rate. Click the card for a per-upstream-model breakdown (including write volume). **Streams disconnected midway (client Esc, network drop) don't count into the aggregates**: they only received `message_start`'s estimated usage (Kimi field tests show `start.input` includes cache_read with `start.cr=0`, the real split arriving only in `message_delta`), so counting them would book the whole context as uncached input and visibly drag down the hit rate; such streams roll back the deltas already counted, and the log marks `[interrupted]`.
- **retries**: cumulative retry count since startup (+1 per retry; includes all three triggers — status 429/5xx, first-byte timeout/network error, and 200-body errors; giving up on budget exhaustion isn't counted).
- **classifiers**: requests matching the classifier (safety-check) fingerprint since startup — counted whether or not classifier_route reroutes and whether or not thinking is turned off. Hovering the Status page's 「分类器」 (classifier) card shows the two numbers (total hits and thinking-off rewrites); clicking opens a breakdown popup (with the thinking-off share), same interaction as the 「重试」 (retries) card.
- **classifierNoThink**: of those, how many actually got the thinking-off rewrite (only happens with `"classifier_thinking": "off"` on `classifier_route`; requests already in thinking-off shape produce no rewrite and don't count).
- **avgFirstByte**: mean first-token latency (seconds) over the last `recent_sample_window` requests — from sending the request to the upstream's first body data byte. Only normally-passed-through (case C) streams count; a stream enters the window as soon as its first byte arrives, no need to wait for it to finish.
- **tps**: weighted token throughput over the last `recent_sample_window` requests — Σoutput_tokens / Σstreaming duration (first byte to last byte). Weighted rather than simply averaged, so short requests don't skew the overall throughput.

> **About token realtimeness**: ARK (and standard Anthropic) only sends `usage` once, in the `message_delta` event at the **end** of the stream; the mid-stream `content_block_delta` / `thinking_delta` carry no token counts. So token fields stay constant during a stream and update to their accurate values at the end. For a live feel of "pouring out", watch `bytesForward`/`rate` and the bytes in the in-flight table — they jump as forwarding proceeds.

Implementation notes:

- Logs uniformly go to the in-memory ring buffer `logRing` (500 lines) for the web Logs tab to poll; also written to stderr (a no-op under the windowsgui subsystem or without a terminal — nothing hits disk). A non-empty `log_file` additionally appends to the file.
- `reloadConfig()` returns an error, so the web Config tab can display the failure back to the user on save+reload failure; behavior is unchanged (re-read config, atomic swap, clear cumulative stats, no port re-bind).
- All endpoints go through the `isLocalRequest` guard: `GET /__config`, `POST /__config` (validate + write + reload), `POST /__reload` (reload only), `GET /__logs` (HTML page), `GET /__logs/data` (status + log JSON).

## System tray / menu bar (cross-platform)

After startup a status-light icon appears: in the menu bar on macOS (`.app` packaging, `LSUIElement=true`, no Dock icon), in the system tray on Windows/Linux. Features:

- **Right-click the icon**: the menu is **identical across platforms** — 「查看日志」 (Open console), 「切换配置」 (Switch config: a submenu listing every `.json` in the config directory, the current one ticked, click to switch and hot-reload; 「刷新列表」/Refresh list rebuilds it after files are dropped into the directory manually), and 「退出代理」 (Quit proxy). No more "show window" / "open config file" — config editing and reload live in the web console.
- **Open console**: opens the web console `http://<listen>/__logs` in the default browser (Status/Logs/Config tabs, see above); **closing the browser tab merely hides it — the proxy keeps running unaffected**. Localhost-only (non-`127.0.0.1`/`::1`/`localhost` requests get 403).
- **Status-light icon**: **red** (config problem: the running config contains removed keys — the console Logs tab names the exact keys; overrides the traffic colors) / **grey** (idle, no requests) / **yellow** (sent upstream, awaiting first byte) / **green** (streaming forward). With parallel requests the higher priority shows (green > yellow > grey). State is checked every 200ms; any change in state/active/waiting updates the icon and hover text.
- **Hover tooltip**: hovering the icon shows multi-line text with current counts. Idle: `Proxy429` / `idle`; with requests, one line per state: `active N` / `waiting N`, both shown when coexisting. Count changes also refresh, so you can see live how many streams are running. With a config problem (red icon), a warning line (「⚠ 配置含已删除的键，详见控制台文档」) is prepended.

Platform differences:

| Platform | Tray location | Tray implementation | cgo |
|------|----------|----------|-----|
| Windows | system tray | `fyne.io/systray` (pure Go syscall) | no |
| macOS | menu bar | `fyne.io/systray` (Cocoa/AppKit) | yes (needs clang, bundled with macOS) |
| Linux | status area | `fyne.io/systray` (D-Bus StatusNotifier) | no |

> Proxy features are identical across the three platforms (retries, routing, classifier, keepalive, web-console stats); the only difference is tray location (macOS menu bar vs Windows/Linux system tray). Windows builds use the GUI subsystem (`-H=windowsgui`): no console window on launch, pure tray operation; logs via the web console or `log_file`.

Implementation notes:

- Cross-platform tray via `fyne.io/systray`: one set of code in `tray.go` handles menu/status light/tooltip/status polling; `systray.Run` occupies the main thread (macOS requires the UI event loop on the main thread), and the HTTP service runs concurrently in a goroutine.
- The status-light icon is generated in pure Go (no cgo/no GDI): a 32x32 anti-aliased solid circle, encoded as PNG for macOS/Linux and wrapped as a BMP-entry ICO for Windows (`LoadImageW` certainly supports it).

## Local testing

The `test/` directory holds files for local re-testing without ARK:

- `test/mock_429.go`: a mock server switching among three upstream behaviors via `?mode=` — `429` (status-code retry), `bodyerr` (200 + in-body error retry), `ok` (normal pass-through, with `usage` and `message_delta`, so the web console's Status tab shows tokens ticking). It also prints the received `thinking`/`reasoning_effort`/`max_tokens` for verifying the classifier rewrite.
- `test/config_test.json`: test config pointing at the local mock, with small retry intervals and `Retry-After` disabled for fast iteration.

Re-test flow (run from the project root):

```bash
# Terminal 1: start the mock (listens on 9099)
go run ./test

# Terminal 2: start the proxy pointed at the mock (Windows: .\proxy429.exe)
./proxy429 -config test/config_test.json

# Terminal 3: fire requests for the three cases
curl -i -X POST "http://127.0.0.1:8081/v1/messages?mode=429" -d '{"model":"t","messages":[{"role":"user","content":"hi"}]}'
curl -i -X POST "http://127.0.0.1:8081/v1/messages?mode=bodyerr" -d '{"model":"t","messages":[{"role":"user","content":"hi"}]}'
curl -i -X POST "http://127.0.0.1:8081/v1/messages?mode=ok" -d '{"model":"t","messages":[{"role":"user","content":"hi"}]}'
```

Confirm behavior via the `[rewrite]`/`[attempt N]`/`[done]` lines on the web console's Logs tab. When done, stop the proxy (tray menu 「退出代理」; or `Ctrl+C` on macOS/Linux, `taskkill /F /IM proxy429.exe` on Windows) and stop the mock.

## Observing rate limits

The log prints every retry (`[attempt N] upstream response status: 429` followed by `[retry] status 429; waiting ... before retry (budget left ...)`) — run it for a while and you'll see exactly how fond this provider is of 429s.
