# Windows Quickstart

The new Proxy429 is a pure tray app — **no console window**. Status, logs, and config all live in the local web console.

## 1. Prepare the files

Put these files in one directory, e.g. `C:\RUN_Utility\Proxy429\`:

- `proxy429.exe`
- `config.json` (renamed from `config.example.json`, with your real API keys filled in)
- `windows-quickstart.md` (this file, for reference)

## 2. Configure `config.json`

Copy `config.example.json` to `config.json` and replace the placeholder API keys with your own:

- `upstream`: the default upstream, e.g. a Volcano/ARK address
- `routes[].api`: each routed model's upstream key
- `classifier_route.api`, `fast_route.api`, `multimodal_fallback.api`, `search_fallback.api`: the fallback routes' keys
- `search_fallback` supports search summary mode (`summary_mode: true`): the proxy builds the response itself in two steps (step1 search + step2 summary) without calling the main model; summary verbosity is controlled by `summary_level` (`low`/`mid`/`high`/`max`, default `low`) — see "Search summary mode" in `docs/DESIGN.md`.
- A `routes[]` entry can also set `"enhance_search": {...}` to enable **enhanced search**: when the main upstream supports search, search-tool-carrying requests don't call the main model; the route's own upstream runs the kimi summary mode instead. See "Enhanced search" in `docs/DESIGN.md`.

Quick reference for common fields:

| Field | Description |
|------|------|
| `listen` | listen address, default `127.0.0.1:8080` (the forwarding channel is always localhost-only) |
| `log_file` | log file path; set `"proxy.log"` first for easier troubleshooting |
| `ping_interval_s` | 429-retry SSE ping interval, default `5` |
| `max_retries` | maximum retries, default `5` |
| `retry_status_codes` | status codes that trigger a retry, default `[429, 500, 502, 503, 504]` |

The config can also be edited live on the web console's 「配置」 (Config) tab after startup.

### Multiple config files

Multiple configs can sit in the same directory (e.g. `config.json`, `config-work.json`), switchable anytime and effective immediately — in-flight requests aren't interrupted (they finish on the old config):

- **Tray**: right-click the icon -> the 「切换配置」 (switch config) submenu lists all `.json` files, the current one ticked; clicking switches instantly. Web-side switches/creates/deletes/renames refresh the tick and list instantly too. 「刷新列表」 (refresh list) rebuilds the menu manually after you drop config files into the directory directly.
- **Web**: switch via the config-area dropdown; 「新建」 (New) creates from `config.example.json` as a blank template and switches to it; 「重命名」 (Rename) / 「删除」 (Delete) require typing the file name (delete double-confirms; the currently active config can't be deleted).
- **Status page**: the current config file name shows on the right of the status-light row; each in-flight stream row carries a status light (🟢 forwarding / 🟡 waiting / ⚪ request).

The config directory follows the startup resolution order: `config.json` in the current directory takes priority, otherwise `%AppData%\proxy429\`. Keep all configs in that one directory.

## 3. Start

Double-click `proxy429.exe`; a status-light icon appears in the system tray:

- ⚪ grey = idle
- 🟡 yellow = request sent, awaiting upstream response
- 🟢 green = streaming forward

Right-click the tray icon -> **「查看日志」 (Open console)**; the browser opens:

```
http://127.0.0.1:8080/__logs
```

> No tray icon? Add `"log_file": "proxy.log"` to `config.json`, restart, and read the startup log in `proxy.log`.

## 4. Point Claude Code at the proxy

In the terminal that starts Claude Code:

```cmd
set ANTHROPIC_BASE_URL=http://127.0.0.1:8080
set ANTHROPIC_AUTH_TOKEN=your-api-key
claude
```

Notes:

- It must be the same terminal that starts `claude`.
- If `routes` already sets `api`, `ANTHROPIC_AUTH_TOKEN` can be any non-empty string; the proxy overrides it with the route's key.
- Upstreams that only accept `Authorization: Bearer` (like ARK) require `ANTHROPIC_AUTH_TOKEN`, not `ANTHROPIC_API_KEY`.

## 5. Quit

Right-click the tray icon -> **「退出代理」 (Quit proxy)**.

Closing the browser tab does **not** quit the proxy; it merely hides the web console.

## 6. Troubleshooting

| Symptom | Fix |
|------|------|
| No tray icon | Add `"log_file": "proxy.log"`, restart, read `proxy.log` |
| Web console won't open | Check whether the `listen` port is occupied; changing the port requires a proxy restart |
| Claude Code reports API error | Check the 429 retry logs on the web console's 「日志」 (Logs) tab; check `ANTHROPIC_BASE_URL` is set correctly |
| Want to keep logs | Set `"log_file": "proxy.log"` in the config; by default only 500 lines are kept in memory for the web page |

## 7. Rebuilding

In the project root (the level containing `go.mod`):

```bash
bash build.sh
```

The Windows artifact is `release/proxy429.exe`.

Full config documentation, routing rules, the 429 keepalive mechanism etc. are in `docs/DESIGN.md`.
