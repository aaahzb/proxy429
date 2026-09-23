# Proxy429 Usage Guide

A local Anthropic-compatible HTTP proxy: absorbs upstream 429 / 5xx with automatic exponential-backoff retries, transparent to Claude Code.
**Identical across platforms**: macOS / Windows / Linux behave exactly the same — after startup it runs tray-only (no terminal window), and status, logs, and config all live in one local web console.

---

## Which file do I run on macOS?

**Double-click `Proxy429.app`.** It's a menu-bar app (no Dock icon, no terminal window); closing Finder doesn't affect it.

- **Gatekeeper blocks the first launch**: **right-click -> Open** once in Finder (ad-hoc signature, not a developer certificate).
- After startup a status light appears in the menu bar: ⚪ grey (idle) / 🟡 yellow (sent, awaiting reply) / 🟢 green (streaming).
- Click the menu-bar icon -> **「查看日志」 (Open console)**: the default browser opens the web console `http://127.0.0.1:8080/__logs`.
- **Quitting**: menu-bar icon -> **「退出代理」 (Quit proxy)**. Closing the browser tab does **not** quit the proxy (it merely hides the console).

> On Windows double-click `proxy429.exe` (no console window pops up); right-click the system-tray icon -> 「查看日志」 ("Open console" in the English UI). On Linux run `./proxy429`, same system tray. The web console is identical on all three platforms.

---

## Where is the config file?

A default config (with placeholder keys — replace with your own) is generated on first launch:

| Platform | Config path |
|------|---------|
| macOS | `~/Library/Application Support/proxy429/config.json` |
| Windows | `%AppData%\proxy429\config.json` |
| Linux | `~/.config/proxy429/config.json` |

If the startup directory has a `./config.json` it takes priority (handy for debugging in a project directory). You can also pass `-config <path>`.

**Editing the config is best done on the web console's 「配置」 (Config) tab** — JSON is validated before saving (broken JSON never hits disk), and clicking 「保存并重载」 (Save & reload) takes effect immediately, no restart needed.

---

## Getting Claude Code on the proxy

In a separate terminal:

```bash
export ANTHROPIC_BASE_URL="http://127.0.0.1:8080"
export ANTHROPIC_AUTH_TOKEN="your API key"
claude
```

> If `routes` entries set an `api` field, the proxy uses it to **override** the client token — `ANTHROPIC_AUTH_TOKEN` can then be any non-empty string.
> **Upstreams that only accept `Authorization: Bearer` (like ARK) require `ANTHROPIC_AUTH_TOKEN`** (not `ANTHROPIC_API_KEY`, or you get 401).

---

## Getting Codex CLI on the proxy (the Responses listener port)

Codex CLI only speaks the OpenAI Responses protocol; point it at this proxy's `responses_listen` port. Prerequisite: `responses_listen` is enabled in the config (the template demos `127.0.0.1:8081`; changes take effect on save+reload — the listener starts/stops dynamically with the config). When the Config tab detects the current config has no `responses_listen`, it greys out the script-generation area below and asks 「要为本配置增加 Responses API 功能吗」 ("add Responses API support to this config?") — clicking 「是，添加并保存」 (Yes, add and save) automatically inserts a `responses_listen` line after `listen` and saves, starting the listener right away (it never hard-generates a script for a default address without the config, avoiding pointing Codex at a port occupied by another program).

### One-click setup (recommended)

**Web-console-generated commands (least effort)**: below the 「配置」 (Config) tab is a 「Codex 一键配置」 (Codex one-click setup) area — it **generates live** two terminal commands from the editor contents above (the address comes from `responses_listen`; with that unset the area is greyed with the 「是，添加并保存」 button to add-and-save in one click), in the same format as DeepSeek's docs. Next to the default-model dropdown are two inputs, 「上下文窗口」 (context window) and 「压缩阈值%」 (compaction threshold %) (defaults 262144 / 95), baked into the script with the command and written into every model-catalog entry:

- Windows: paste `irm 'http://<this proxy>/__codexsetup.ps1?...' | iex` into a PowerShell window and hit enter
- macOS / Linux: paste `bash <(curl -fsSL 'http://<this proxy>/__codexsetup.sh?...')` into a terminal and hit enter

The script is **baked and served live by this proxy** from the page's selected default model and model catalog (the full command as actually shown in the console carries all parameters); restart Codex after running it. After changing `responses_listen` and clicking 「保存并重载」, the listener takes effect at the new address and the two commands follow the editor automatically — copy and run again. Re-running the same command on an already-set-up machine takes a fast path: it only switches the default model, refreshes the model catalog, and syncs the `proxy429` provider block's name/base_url to the latest (Chinese names written by older script versions get upgraded to English this way); nothing else is touched.

The model dropdown lists **one representative name per `routes` pattern** (when `route.model` itself matches the pattern, the real name is used; otherwise the pattern with `*` stripped — this proxy's `*` can match zero characters, so that name always hits; a pure `*` catch-all route is fixed as `Fallback` — it catches any model name, and showing a real model name in the menu would mislead; hence `Fallback` is a reserved name: no other route's `pattern` may be literally `Fallback` (a colliding route neither takes effect nor enters the menu, and the web page warns in red; wildcards like `Fall*` are unaffected). Also, with `fast_route` configured and carrying a `model`, the menu gains a literal `fast_route` entry that takes the fast lane when selected (equivalent to sending `speed:"fast"`); `fast_route` is likewise a reserved name — `pattern` may not be literally `fast_route`. The script writes **all** these names into Codex's `/model` menu (`model_catalog_json` points at `~/.codex/proxy429-models.json`); the dropdown selection becomes the default `model` in `config.toml`; afterwards `/model` inside Codex switches anytime, taking the route of the corresponding pattern.

**Repo scripts (interactive)**: `codex-setup.ps1` (Windows) and `codex-setup.sh` (macOS/Linux) at the repo root are the interactive versions (menus for model/address/restore; the script UI is all-English, avoiding some systems' Chinese encoding issues) with the same effect (structure modeled on DeepSeek's official Codex install script: backup first, surgical TOML rewrites, atomic write after validation):

```powershell
# Windows PowerShell
powershell -ExecutionPolicy Bypass -File .\codex-setup.ps1
```

```bash
# macOS / Linux
bash ./codex-setup.sh
```

Pick a model from the menu (`gpt-5-codex` / `claude-fable-5` / custom model name); option 9 restores. What the script does:

- Writes top-level `model`, `model_provider = "proxy429"`, `preferred_auth_method = "apikey"` + `forced_login_method = "api"` (these two lines put Codex in API-key mode, **no official-account login required**), `model_reasoning_effort = "high"`, `model_catalog_json`; appends the `[model_providers.proxy429]` section (the token is a placeholder, the proxy doesn't validate it).
- Generates the `~/.codex/proxy429-models.json` model catalog so the `/model` menu lists models and switches thinking levels.
- Performs surgery on an existing config.toml rather than overwriting: clears stale keys that would fight the proxy (`service_tier`, `openai_base_url`, context-window overrides, etc., reported one by one), fixes other providers' `wire_api = "chat"` (which stops Codex from starting), and keeps everything else like `[profiles.*]` untouched.
- Automatically backs up to `~/.codex/backup-proxy429/` before changing (with a manifest of changes); option 9 restores verbatim.

**Restart Codex** after installing (config.toml isn't hot-loaded). For the write details, see manual setup below.

### Manual setup

Edit `~/.codex/config.toml` (on Windows `%USERPROFILE%\.codex\config.toml`; create it if absent):

```toml
model_provider = "proxy429"
model = "gpt-5-codex"            # the name that participates in proxy route matching
preferred_auth_method = "apikey" # these two lines mean API-key mode, no official-account login
forced_login_method = "api"

[model_providers.proxy429]
name = "Proxy429 Local Proxy"
base_url = "http://127.0.0.1:8081/v1"
wire_api = "responses"
experimental_bearer_token = "proxy429"
```

This structure is identical to what cc-switch writes when taking over Codex (`model_providers` + `wire_api = "responses"`), just with base_url swapped from its routing port to this proxy's. Key points:

- **base_url must carry `/v1`, `wire_api` must be `"responses"`**: Codex POSTs to `http://127.0.0.1:8081/v1/responses`, exactly this proxy's listen path (`/responses` is also accepted).
- **The token is just a placeholder**: the proxy doesn't validate it; the real upstream key is injected by the hit route's `api` field. The two lines `preferred_auth_method`/`forced_login_method` above make Codex start straight in API-key mode (same as DeepSeek's official script), never prompting for an official-account login.
- **The model name works twice**: it participates both in the proxy's `routes` wildcard matching (the template's demo `gpt-5*` route -> DeepSeek; change as needed) and in the thinking-level lookup — to use adaptive thinking (see "Responses translation mapping"), fill `model` with the target model name directly (e.g. `claude-fable-5`); routing still matches and forwards as usual.
- **Thinking level**: add a line `model_reasoning_effort = "high"` to config.toml (or adjust via `/model` in a Codex session); it arrives as `reasoning.effort` and is translated per the mapping table into adaptive effort or budget_tokens.
- **Restart Codex after changes**: `config.toml` is read at Codex process start, not hot-loaded.
- **503 with zero proxy-side logs = system-proxy hijack**: Codex reads the system proxy (Windows "Internet Options" / macOS network settings) but **ignores its exception list** — with a system proxy on, requests to `127.0.0.1` get sent to the proxy server (whose loopback has no such proxy), reporting `unexpected status 503 ... url: http://127.0.0.1:8081/v1/responses` with no request visible in this proxy's log. The only effective bypass is the `NO_PROXY` environment variable containing `127.0.0.1` — the Windows one-click script already detects this at install time and writes a user-level `NO_PROXY = localhost,127.0.0.1,::1` (only loopback bypasses the proxy; other traffic unaffected); the macOS one-click script writes `NO_PROXY`/`no_proxy` into the launchd environment (`launchctl setenv`, effective for GUI apps and new terminal windows) and installs the login item `~/Library/LaunchAgents/com.proxy429.noproxy.plist` so it survives logout/restart (choose "restore" in the script or delete that file to undo); the Linux one-click script only checks and suggests `export NO_PROXY="localhost,127.0.0.1,::1"` without changing system settings; manual-setup or old-script users run `setx NO_PROXY "localhost,127.0.0.1,::1"` themselves (Windows), then restart Codex (if that doesn't help, log out and back in once).

Verify: start `codex` and say something; the proxy web console's 「状态」 (Status) tab should show an in-flight stream whose API column reads `[translate]`, with a matching `[route]` line in 「日志」 (Logs).

### Multi-model setup

**Zero changes on the proxy side** — `routes` already wildcard-splits by model name; whatever model name Codex sends takes the corresponding route. Three ways on the Codex side:

- **profiles (recommended; configure the ones you use)**: add profile sections to config.toml, each setting `model` (inheriting the top-level provider, no repeated address):

  ```toml
  [profiles.deepseek]
  model = "gpt-5-codex"            # hits the template's gpt-5* route

  [profiles.fable]
  model = "claude-fable-5"         # adaptive thinking lookup applies
  model_reasoning_effort = "high"
  ```

  Choose at startup: `codex --profile fable` (without a profile, the top-level default `model` is used).
- **Ad-hoc**: `codex --model claude-fable-5` — any name works; the proxy routes by name.
- **Listing custom models in the `/model` menu**: needs a model-catalog file — add `model_catalog_json = "my-models.json"` at config.toml top level and place that file under `~/.codex/` with contents `{"models": [...]}` (cc-switch generates just such a `cc-switch-model-catalog.json` and writes this key; copy entry fields from the file it generates). Not configuring it doesn't affect use; the `/model` menu just won't list custom models.

---

## The web console's three tabs

- **状态 (Status)**: real-time stat cards (active / waiting / bytes out / rate / cache hit / input / output / retries / classifiers / first-token latency / tok/s; 「分类器」 (classifier) = requests matching the safety-classifier fingerprint — counted whether or not rerouted or thinking-off; hover the card for the breakdown, including 「关思考」 (thinking off) = how many of them actually got thinking-off rewrites; click for an enlarged popup) + the in-flight streams list (each request's model, stage, bytes, status code; beside the first column's status light ⚪ request / 🟡 awaiting first byte / 🟢 forwarding sits the seconds the current color has lasted, zeroed only when the color changes; while awaiting the first byte inside a yellow light, between the light and the total duration there's also `[attempt N: Xs]` (Chinese UI: `[尝试N: Xs]`) = how long the current attempt has waited (re-counted per attempt; during retry backoff it shows `[backing off]` / `[退避中]`)). Refreshes every 0.5s. Cache hit rate = cache_read / (input + cache_read + cache_creation), consistent with Claude Code's cache-hit algorithm; click the 「缓存命中」 (cache hit) card for a breakdown grouped by upstream model (including cache-write volume). Streams disconnected midway (Esc-interrupted generation etc.) don't count into the aggregates — they can't get the real usage split, and counting them would pollute the hit rate; the log marks `[interrupted]` and the finished stream's status code is recorded as 499 (same convention as upstream providers' dashboards: the connection ended before the upstream finished sending its response — most commonly the downstream actively cancelled, the cancellation propagating into an upstream disconnect; when the downstream disconnects only after the upstream finished in full, like Codex closing right after receiving, it's still recorded as 200). Non-200 statuses in the status column display bracketed (e.g. `[499]`, `[400]`) so abnormal streams stand out; a stream that exhausted retries/budget gets a fallback error event passed downstream (overloaded_error) and the status column shows `[retries exhausted]` (Chinese UI: `[重试尽]`); a stream that had backoff retries gets a gold `[retried Nx]` appended after the status (N = retry count, e.g. `200[retried 2x]`; also attached with `[retries exhausted]`, so you can check against max_retries). Both in-flight streams (growing live as forwarding proceeds) and recent-finished streams list the tools called in that stream's response after the model column as `[Read*1][Edit*3]` (in gold) — server-side tools like `web_search` count too, in first-appearance order; N calls of the same name collapse to `*N` (the raw count), a single call shows `*1`, and a single call with an empty argument object shows `*0` (e.g. Kimi empty-search no-arg calls — telling empty searches from real ones at a glance). Streams that had replayed search blocks stripped additionally show a red `[strip N]` (Chinese UI: `[剥N]`; N = total stripped): in-flight streams carry it on the model column, recent-finished streams after the cache-hit rate (e.g. `81%[strip 2]`); how many were conversation-watermark strips versus upstream-400 fallback strips is broken down in the `[strip]`/`[fallback]` log lines. Streams whose model column carries a `[count_tokens]` prefix are Claude Code's token-counting probes (`/v1/messages/count_tokens`, for context-length estimation); their raw content being just `{"input_tokens":N}` is normal, and they don't count into any token statistics. Clicking an in-flight/recent-finished row replays that stream: the view button cycles three states — request body → 输出[流] (output stream, the default landing; 「显示解析/显示原始」 toggles parsed/raw) → 输出[整理] (assembled output: the SSE event stream folded into the final JSON structure — Anthropic streams accumulate onto the message_start skeleton, Responses streams take the response.completed object; an incomplete stream gets a partial-snapshot note) — then back to request body; the request-body state replays the request body that caused the stream (JSON auto-prettified, non-well-formed JSON shown verbatim); request and response bodies are both recorded per link side, switchable via the 「链路」 (link) button (default 代理↔上游 (proxy↔upstream) side: the request body actually sent upstream and the raw stream back from it; the other side 下游↔代理 (downstream↔proxy): what the client sent / actually received; streams with identical sides aren't double-stored, and switching to a side with no record falls back automatically with a hint beside the button). Browsing always caps at the first 256KB; with 「储存完整结构体」 (store full payloads; default off, resets on restart) ticked, newly started requests additionally record the complete request body and output (uncapped, in memory), and the viewer gains 「下载请求体/下载输出」 (download request body / output) buttons for the complete files (saved JSON-prettified, non-JSON verbatim), plus an 「交互式JSON」 (interactive JSON) button — rendering the request body/output as a key-collapsible JSON tree (all collapsed by default, click a key line to lazily expand; SSE event-stream outputs are parsed into an event array first); unticking immediately clears the stored full copies and the download/interactive buttons vanish. Streams with incomplete data are never silently treated as complete: 「下载请求体」 is greyed when only a truncated request body remains (hover for why), 「下载输出」 is greyed without a full output copy, and interactive JSON outright refuses both kinds. Responses translated streams always differ between sides and are always double-stored (the downstream side holds the client's Responses original and the actual reply; the upstream side holds the translated Anthropic body and the upstream's raw SSE). The recent-finished streams table additionally has a 「缓存年龄」 (cache age) column: per session+route, how long ago that session's most recent cache write on that route was (`m:ss` counting up; the upstream cache's true TTL is dynamic — this column no longer guesses a countdown, it only says how long ago this cache was written; for whether it's still usable, judge against the measured ranges in the 「缓存命中」 popup). The newest stream of a session+route shows an age; older rows with the same key show `-` after refresh; streams without a session identifier always show `-`; the age counts from the stream's start, and a "session+route latest" whose start was within the last 5 minutes isn't pushed off the list by "keep N finished streams" (row count may exceed N). The 「API」 column of the in-flight and recent-finished tables merges protocol origin and thinking value into one cell: the name = protocol origin (orange `[Anthropic]` = native Anthropic port, purple `[translate]` = Responses port translated), and the `[value]` after the name = the shortest form of the thinking config **actually sent upstream** (the final state after translation mapping, classifier thinking-off, and all other proxy rewrites), **its color = the vocabulary** (thinking targets the upstream: upstreams always receive Anthropic format, so a purple name is followed by an orange value) — orange = Anthropic thinking (direct-as-is or after translation mapping); `关`/`off` = thinking disabled or effort none/off, `开 N`/`on N` = enabled+budget_tokens N, `adaptive` = adaptive without a level, `low`/`high`/`max` etc. level words = adaptive effort, no `[value]` = the request carried no thinking field; e.g. Codex sending effort high translated to an Anthropic upstream shows `[translate][开 16384]` on the Chinese UI (purple name, orange value; the English UI shows `on 16384`). A 499 stream disconnected by the downstream during the yellow-light (awaiting first byte) stage doesn't refresh the cache (this row shows `-`, the anchor stays on the previous same-key stream); a 499 disconnected during the green-light (streaming) stage refreshes as usual; while a same-session same-route in-flight stream is producing output, that key's latest finished row freezes as `[m:ss]` (inside the brackets, the old anchor's age at refresh time, unchanging; the column header's question mark has a hover explanation), and the new anchor takes effect when that stream finishes. Clicking the 「缓存命中」 card opens a popup whose bottom 「实测缓存时间」 (measured cache time) table lists each upstream's measured cache survival time (seven columns: model, URL, lower bound ≥, ≥ formed, upper bound <, < formed, observation count): grouped by URL+model name (other parameters ignored); when two adjacent same-session streams see the latter at ≥95% hit rate, it records one lower-bound observation "lived at least the interval" (max kept); when an earlier stream hit and a later one falls below 50%, it records one upper-bound observation "didn't live the interval" (min kept; strict zeroing isn't required — residual hits on common prefixes like the system prompt don't count as alive); streams with under 1024 total input tokens aren't observed; when the two sides contradict (the upstream's cache time changed mid-way or was evicted early), the newer observation wins, the refuted side is discarded and re-measured, and its observation count zeroes and restarts; the "≥ formed"/"< formed" columns = how long ago each bound's value was established (`m:ss` counting up) — a bound's value changing (including discard-and-re-measure) restarts its clock, merely adding supporting observations with the value unchanged doesn't reset it, and a bound with no observations shows - along with the bound; measurements are display-only, purely in-memory, cleared on restart or 「清空统计」 (clear stats). The 「清空统计」 button clears the cumulative stats above and the recent-finished list (in-flight streams and the # numbering are not cleared; the in-memory log has its own 「清空日志」 (clear log) button).
- **日志 (Logs)**: live-scrolling log + the last 500 lines of history, wheel-scrolls through history, auto-scrolls to bottom. Every request logs its routing, e.g. `[route] #1 claude-opus-... -> https://... (model ... -> kimi-for-coding)`, `[route] #2 search-fallback ... -> https://... (model ... -> deepseek-v4-flash)`.
- **配置 (Config)**: edit `config.json` contents directly; **保存并重载** (save & reload — validate + write to disk + take effect) / **仅重载** (reload only — just re-reads from disk).

The management endpoints (`/__logs`, `/__config`, `/__reload`) are **always localhost-only** (non-`127.0.0.1`/`::1`/`localhost` requests get 403) — even with the proxy's `listen` on `0.0.0.0`, no logs/config leak.

### Access control

Both the forwarding channel and the management endpoints are **always localhost-only** — even if `listen` / `responses_listen` is mistakenly set to `0.0.0.0` listening on all interfaces, non-local requests are all blocked with 403; LAN devices can't use the proxy or see logs/config. No switch, nothing to configure.

---

## Routing config (splitting by model / capability)

`routes` **wildcard-matches** the model name Claude Code sends (`*` matches any length of any characters) and splits requests to different upstreams; a hit can change `url` / `api` / `model`:

```json
"routes": [
  { "pattern": "*opus*",   "url": "https://upstream-a", "api": "sk-aaa", "model": "strong-model" },
  { "pattern": "*haiku*",  "url": "https://upstream-b", "api": "sk-bbb", "model": "cheap-model" },
  { "pattern": "*",        "url": "https://upstream-a", "api": "sk-aaa", "model": "strong-model" }
]
```

Matches in array order, first hit wins — a wide wildcard intercepts a narrow one, so more specific patterns go earlier (e.g. `*opus-4*` before `*opus*`), with the `*` catch-all last.

**Capability fallbacks** (when a request needs a capability the hit upstream doesn't support, automatically reroute to the fallback upstream):

- `text_only: true` — the model is text-only; requests **carrying an image** reroute to `multimodal_fallback` (unconfigured = pass through to the original model).
- `no_search: true` — the upstream doesn't support search; requests **carrying a search tool** reroute to `search_fallback` (unconfigured = pass through to the original model).
- `classifier_route` — dedicated route for classifier (safety-check) requests; dump them onto a cheap model to save main-model quota.
- `fast_route` — dedicated route for `"speed":"fast"` requests.

**Search summary mode** (`search_fallback`-only): with `"summary_mode": true`, search requests no longer transfer wholesale to the fallback upstream; instead the proxy builds a Kimi-format response in two steps and returns it: step1 searches for `web_search_tool_result` (titles/URLs), step2 streams a detailed summary, assembled into a standard SSE stream returned to the client (both the dropdown list and the model's answer work), never calling the main model. Summary verbosity is controlled by `"summary_level"`: `low` (default, brief) / `mid` (medium) / `high` (thorough) / `max` (on top of high, steps/methods/code/formulas restated completely and verbatim); `"summary_thinking": true` turns thinking on for step2. Step1/step2 failures automatically degrade to wholesale transfer. When search triggers, the status page shows two sub-streams whose model columns read `search-step1·<model>` / `summary-step2·<model>`. See "Search summary mode" in `docs/DESIGN.md`.

**Enhanced search** (`"enhance_search": {...}` inside a `routes` entry): the reverse scenario — the main upstream supports search (`no_search` not `true`), but you don't want its built-in search and want the proxy to answer with the kimi summary mode. Search-tool-carrying requests don't call the main model; **the route's own `url`/`api`/`model`** runs step1 search + step2 summary + self-built response, with parameters from that route's `enhance_search.summary_level`/`summary_thinking`. Mutually exclusive with `search_fallback.summary_mode` (`no_search:true` goes search_fallback; `no_search:false` + `enhance_search` goes enhanced search). See "Enhanced search" in `docs/DESIGN.md`.

All four fallback fields are optional; configure as needed. Full field documentation is in `docs/DESIGN.md` and `config.example.json`.

**Global stream-ification** (top-level `"convertAlltoStream": true`): silently rewrites **all non-streaming requests** (`stream:false` or omitted) to streaming before sending upstream. The benefit: originally one-shot requests like the classifier and search step1 also show live token flow on the web console, with first-token latency and tok/s stats. The caller notices nothing — after collecting the full stream, the proxy rebuilds a standard non-streaming JSON and returns it in one shot; Claude Code still gets the non-streaming response it expected. A stream breaking midway auto-retries the whole request; route-rewritten models are written back to the original model. See "Global stream-ification" in `docs/DESIGN.md`.

**Responses API listener** (top-level `"responses_listen": "127.0.0.1:8081"`; empty = disabled; the config template demos it on): opens an extra OpenAI Responses API endpoint, connecting Responses-only tools like Codex CLI to Anthropic upstreams — requests are translated to Anthropic through the main pipeline (routing/retries/web monitoring as usual), responses translated back to Responses (SSE event stream or one-shot JSON). The tool's model name participates in route matching as usual; add a `gpt-5*` route to pick the upstream. Changes take effect on save+reload (the listener starts/stops dynamically). Additionally, the translation layer automatically deletes Kimi (k3-256k etc.) response lines starting with `Search results for query: ` (together with the query that follows; the same rules apply to what's forwarded to the client and to history replayed upstream, preventing echoes from accumulating in the context and inducing repeated same-kind searches); text blocks stripped to empty and empty search structures like no-arg empty calls or empty results never appear either; when the model imitates history by gluing several bare preambles onto the body's start, the preambles are still stripped bare, keeping the body. The Codex UI no longer piles up such echo lines; real searches still show with result links. See "Responses API listener" in `docs/DESIGN.md`.

**Quietly upgrading thinking-off requests to low** (per-route `"convertOff2Low"`: `"translate"` = Responses translation port only, `"all"` = translation port + Anthropic native port; set on `routes[]` / `fast_route` / `multimodal_fallback` / `search_fallback` entries, unset = off; passthrough streams are never rewritten): when the client sends a thinking-off request (`reasoning.effort` of none/off/disabled at the translation port, `thinking.type:"disabled"` at the native port), the proxy quietly upgrades to low thinking upstream — adaptive models get `{"type":"adaptive"}` + `output_config.effort:"low"`, budget models get `{"type":"enabled","budget_tokens":2048}` (capped at half of max_tokens; if the 1024 floor doesn't fit, the upgrade is abandoned and thinking stays off) — and thinking blocks are stripped on the way back, so the client still sees a thinking-off response with token usage truthfully passed through. Motivation: Kimi's docs say "with thinking off, requests are routed to the K2.8 Preview no-thinking version: K3-series and K2.8 Preview requests with thinking off are all handled by K2.8 Preview (no thinking)" — keeping low on avoids the K3 downgrade routing. Tool-continuation turns (whose last turn has no signed thinking block to replay) are likewise upgraded. Which route's value applies: at the translation port the route is pre-matched by the client-sent model name (the Codex menu's `fast_route` name takes `fast_route`'s own value); at the native port the finally effective route's value applies — after an image/search fallback swap, the fallback route's (a fallback route's value therefore never reaches translation-port requests). The other door — a tool continuation whose history can't be replayed (no replayable signed thinking block before the trailing tool_result) — is governed by the top-level `"allowNoThinkBlock4Anthropic"` (default **true**): true = the requested thinking mode is sent upstream as-is, and only if the upstream really rejects thinking over such a history (real Anthropic 400s it) does the proxy retry once with thinking off, without burning the backoff budget; thinking blocks are **not** stripped on this path (the client asked for thinking anyway, the blocks come back with their signature, and history heals next turn). false = cc-switch mode: thinking preemptively off over such histories (adaptive models that can disable get `thinking:disabled`; budget models send no thinking) — false also wins over this convertOff2Low history door, while explicit thinking-off requests never take this door (the stealth upgrade above still governs those). With convertOff2Low set AND the toggle left at true, the try-on path sends at the client-requested level (none given/unrecognized -> low as floor — common with Codex, whose effort list has no none level at all, low being the minimum — since thinking-off would trigger the same K2.8 routing). If the upstream rejects a low upgrade (a 200 in-stream error mentioning thinking), thinking is switched back to disabled for one automatic retry without burning the backoff budget; when a forced `tool_choice` is mutually exclusive with thinking, the upgrade is abandoned and the tool constraint kept. The status page's API column shows a two-tone badge `[off->low]` (`off` in `[translate]` purple = downstream vocabulary, `low` in Anthropic orange = upstream vocabulary; the history fallback truthfully shows the requested level, no badge), and after a fallback it truthfully shows `off` again.

**Data source of the status page's 「看请求体/看返回体」 (view request/response body)**: request and response bodies are recorded per link side; the viewer's 「链路」 (link) button switches sides. Default 代理↔上游 (proxy↔upstream) side: the request body is what the proxy actually sent upstream (for translated streams, the translated Anthropic body; for rewritten native streams, the rewritten body), the response body is the raw stream back from upstream. 下游↔代理 (downstream↔proxy) side: the request body is the client's original, the response body is what the client actually received. Only streams whose two sides differ are double-stored — Responses translated streams always differ (one body per protocol); for convertAlltoStream-rebuilt JSON streams, the downstream-side response body is the rebuilt one-shot JSON; native streams only get a separately stored downstream-side request body when the request body was rewritten (classifier thinking-off, route model rewrite, etc.). Switching to a side with no record automatically falls back to the other side with a hint beside the button (the endpoint truthfully reports the actual side via the `X-Proxy429-Side` response header). Both observation points sit on the proxy's boundary: what the user wants to know is "what did the client send/receive" and "what did the proxy actually send to/receive from upstream".

---

## Responses translation mapping

The `responses_listen` port translates the OpenAI Responses protocol into the Anthropic Messages protocol. Translation rules are identical to cc-switch 3.20.0; this page summarizes all mappings, for checking what a client's (Codex CLI etc.) payload ultimately becomes.

### Model classification table (thinking behavior is looked up by the client-sent model name)

Match rule: model name lowercased, `.` and `_` replaced with `-`, then substring-matched (e.g. `anthropic/claude-opus-4.8` and `claude_opus_4_7` both hit `opus-4-8` / `opus-4-7`). **Note the lookup uses the client-sent model name, before route rewriting** — if a route rewrites `gpt-5-codex` into `claude-fable-5`, to make the table apply, fill the client's model with the target model name directly (routing still matches and forwards); conversely, when the client name hits the table but the **route's target model** has different capabilities (e.g. an alias named `claude-fable-5` that actually routes to budget-only Kimi), set `"thinking": "budget"` (forces `enabled+budget_tokens`) or `"adaptive"` (forces `adaptive+effort` levels) on that route entry to override the lookup; omitting it or `"auto"` keeps the table. This parameter only applies to Responses translated streams (passthrough streams aren't translated, and the Anthropic port never rewrites the client's thinking fields); any other value fails config load with an error.

| Class | Model-name substrings | Meaning |
|---|---|---|
| adaptive models | `fable-5`, `mythos-5`, `mythos-preview`, `sonnet-5`, `opus-4-8`, `opus-4-7`, `opus-4-6`, `sonnet-4-6` | thinking is expressed as `{"type":"adaptive"}` + `output_config.effort`, not budget_tokens |
| default-on adaptive | `fable-5`, `mythos-5`, `mythos-preview`, `sonnet-5` | adaptive turns on even without a client reasoning parameter (other adaptive models need an effort to turn on) |
| thinking can't be disabled | `fable-5`, `mythos-5` | reject `thinking:disabled`: an explicit client `effort:"none"` becomes adaptive + `effort:"low"`; conflicts with a forced tool_choice or history missing signed thinking get a straight error |

Models not in the table (like `gpt-5-codex`, `deepseek-*`, `kimi-*`, `claude-sonnet-4-5` and earlier) take the budget_tokens path below.

### reasoning.effort mapping

Client `reasoning.effort` takes one of two paths by model class (unrecognized values count as absent):

| effort | non-adaptive models -> `thinking.budget_tokens` | adaptive models -> `output_config.effort` |
|---|---|---|
| `minimal` / `low` | 2048 | `low` |
| `medium` | 8192 | `medium` |
| `high` | 16384 | `high` |
| `xhigh` / `max` / `ultra` | 24576 | `max` |
| `none` / `off` / `disabled` | off (`thinking:disabled`) | models that can disable: `thinking:disabled`; those that can't (fable-5/mythos-5): still adaptive + `effort:"low"` |
| absent | off | default-on-adaptive models: on (no output_config); the rest: off |

budget_tokens has two more protections: capped at half of `max_tokens` (leaving room for the visible answer), and not enabled if the capped value is under 1024. `max_output_tokens` -> `max_tokens`, default 32000.

### Thinking vs. history and tool choice

- **Tool-continuation validation**: with thinking on, if the last turn is a tool result (tool_result), its immediately preceding assistant message must carry a replayable signed thinking block (the client replaying the reasoning envelope satisfies this automatically). When unsatisfied: models that can't disable thinking error 400. Otherwise the top-level `"allowNoThinkBlock4Anthropic"` decides — **true** (default, try-first): the requested thinking mode is sent as-is, and only a real upstream rejection (a 400, or a 200 in-stream error) triggers one automatic retry with thinking off (no backoff budget burned); **false** (cc-switch): adaptive models that can disable switch this request to `thinking:disabled`, other models don't enable thinking this request (even with an effort).
- **Forced tool_choice conflict**: with thinking on, Anthropic rejects `tool_choice` of `required`/a named tool. Handling: models that can't disable thinking error 400; other models turn thinking off for this request (restoring temperature/top_p passthrough) and keep tool_choice as-is.
- With thinking on, `temperature`/`top_p` are not passed through (consistent with Anthropic's mutual-exclusion rules).

### Tool and input-item mapping

| Responses side | Anthropic side | Notes |
|---|---|---|
| `function` tools | `tools[]` (name/description/input_schema) | |
| string-form tools / `custom` tools | wrapped into a `{"input": string}` single-parameter schema, original definition inlined into the description | the model produces `custom_tool_call`, item id prefix `ctc_`; no arguments mid-stream, the original text sent in one shot at the end |
| `namespace` (MCP) tools | sub-tools flattened to `ns__name`; over 64 bytes truncated + `__` + 8-byte hash | restored to the `namespace` field on replay |
| `tool_search` | fixed proxy tool (query/limit) | produces `tool_search_call` (execution=client) |
| `web_search` / `web_search_preview` | `web_search_20250305` | cc-switch drops it; this proxy keeps the mapping |
| `input_file` (file_url / file_data) | `document` block (url / base64 source) | default `application/pdf`; unrecognized shapes like file_id references serialize into text as fallback, never silently dropped |
| `input_image` (data URL / http URL) | `image` block (base64 / url source) | unrecognized shapes like blob:/file: serialize into text as fallback, never silently dropped |
| `function_call_output` and other tool results | `tool_result` block | images in results are stripped into native image blocks + a text placeholder marker; overlong base64 truncated to `[cc-switch: omitted N bytes]`; the `[cc-switch:tool-result-error]` marker -> `is_error` |
| `reasoning.encrypted_content` (this proxy's envelope) | restored signed thinking block | prefix `p429-ant-thinking-v1:`; foreign/garbled envelopes are dropped |
| `reasoning.encrypted_content` (search envelope) | restored paired `server_tool_use` + `web_search_tool_result` search blocks | prefix `p429-ant-search-v2:`; the envelope carries upstream url+key hash attribution (the model name is also recorded for troubleshooting), and the whole payload is XOR-obfuscated with a mask derived from the api key — no plaintext url/key info lies in client-side history; a wrong key can't de-obfuscate and is naturally skipped (changing keys automatically invalidates old envelopes); a different-source url is likewise skipped (can't be de-obfuscated, saves tokens); a different model doesn't block (field-tested decrypting fine); at sealing time the call block's id was already normalized to the result block's registered id (the `tool_` id Kimi streaming gives isn't recognized by the registry, and replaying it verbatim must 400); the envelope carries its sealing moment — no fixed TTL ceiling (field tests show an id sealed 1.7 hours prior still alive); when after restoration the upstream still reports the search id expired (old conversations' search blocks have a TTL), the proxy automatically strips the replayed search blocks and retries once, imperceptibly degrading to a fresh search, and records the oldest stripped envelope's sealing moment as that conversation's watermark (monotonically rising); from then on envelopes no newer than the watermark are not restored at all (proactively stripped, never hitting 400; the registry expires by age, so same-age and older ones are certainly dead); ts-less old envelopes count as oldest: stripped when a watermark exists, optimistically restored without one |
| `web_search_call` (history-replayed call items) | not sent upstream (dropped) | purely a client display item: the id is proxy-minted, never registered in the upstream search registry, and going upstream must 400, dragging the envelope-restored real search pair into being stripped along (production evidence); search content is replayed only by the search envelope above |
| `parallel_tool_calls: false` | `tool_choice.disable_parallel_tool_use` | `{"type":"auto"}` is filled in when there's no tool_choice |

usage mapping: `input_tokens`, `output_tokens` translated verbatim; `cached_tokens` -> `cache_read_input_tokens`; `cache_write_tokens` -> `cache_creation_input_tokens` (a like-named compatibility alias is also kept); `reasoning_tokens` -> `thinking_tokens`.

---

## Troubleshooting

- **Claude Code isn't going through the proxy**: you must `export` in the same terminal that runs `claude`, before running it; `echo $ANTHROPIC_BASE_URL` should print `http://127.0.0.1:8080`.
- **Port occupied**: changing `listen` on the 「配置」 tab and saving+reloading does **not** re-bind the port (listening is bound only at startup); quit and restart the proxy.
- **The proxy won't start / the web console won't open**: most likely a `listen` port conflict or a config error. The web page is unusable in this case — edit the config file at the path above directly, fix `listen`, and restart. To troubleshoot startup errors, temporarily set `"log_file": "proxy.log"` to put logs on disk.
- **No retry logs visible**: the 「日志」 tab shows `[attempt N] upstream response status: 429` -> `[retry] status 429; waiting ... before retry (budget left ...)`.
- **Keeping logs**: set `"log_file": "proxy.log"` in the config; by default logs stay only in memory (a 500-line ring buffer for the web page), never hitting disk.
- **No window visible after startup**: that's expected — the program runs tray-only; click 「查看日志」 ("Open console") on the menu-bar/tray icon to see status.

---

## Rebuilding for other platforms

Go 1.26+ required. In the project root (the level containing `go.mod`):

```bash
bash build.sh                                  # host platform: .app on macOS, .exe on Windows, bare binary on Linux
GOOS=darwin  GOARCH=amd64 bash build.sh        # Intel Mac (still produces .app)
GOOS=windows GOARCH=amd64 bash build.sh        # Windows x64
GOOS=linux   GOARCH=amd64 bash build.sh        # Linux x64
```

Artifacts land in `release/` (binary + this guide). macOS needs cgo (the tray uses Cocoa; the system-bundled clang suffices); Windows / Linux are pure Go, no C compiler needed.

---

Full config documentation, routing, the classifier, the 429 keepalive mechanism etc. are in `docs/DESIGN.md`.
