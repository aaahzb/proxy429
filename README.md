# Proxy429

[English](#english) | [中文](#中文)

---

## English

A local LLM API proxy that keeps AI coding agents (Claude Code, Codex CLI,
and other OpenAI/Anthropic-compatible clients) unaffected by flaky model
providers.

> [!TIP]
> **👀 Curious what your coding agent is *actually* up to?** Open the
> console and watch it think, live: in-flight streams render as readable
> text (or raw SSE) while tokens arrive; finished streams keep their full
> request/response bodies as an interactive JSON tree, per link side.
> System prompts, tool schemas, cache hits, retries — an open kitchen,
> nothing hidden. The most literal way there is to learn how Claude Code /
> Codex CLI really work.

> [!TIP]
> **💰 Running Claude Code / Codex on a pricey third-party plan? Stop
> spending it on safety checks.** Both tools fire small background calls
> you never see — safety classifiers, guardian approvals. First-party
> Anthropic plans already get the classifier free; a third-party endpoint
> just sees an ordinary request and bills it against your premium model.
> Point `classifier_route` at a cheap model (add
> `classifier_thinking: "off"` for instant, thinking-free answers) and
> that chatter never touches your main quota again. The status page's
> classifier counter shows how much traffic gets diverted.

- **Automatic retry with exponential backoff** on 429/5xx — including
  rate-limit errors hidden inside HTTP-200 SSE streams — with SSE
  keep-alive so the client never times out mid-retry.
- **Model routing**: wildcard routing across providers, dedicated routes
  for safety-classifier and fast-mode requests, multimodal and web-search
  fallbacks.
- **Protocol translation**: OpenAI Responses ↔ Anthropic Messages — tool
  calling, reasoning-mode mapping, byte-level key-order preservation so
  provider prompt caches stay valid.
- **Search envelope**: web-search results sealed into an encrypted
  envelope in the response; follow-ups re-read the previous results
  instead of searching again (translation path).
- **Silent low-thinking upgrade**: explicit thinking-off requests quietly
  go upstream as low thinking and return stripped of thinking blocks —
  dodging Kimi's documented "thinking off downgrades K3 to the K2.8
  no-thinking model" trap, and the client never notices.
- **Live observability**: a web console streaming in-flight requests live
  with token and cache-hit telemetry; request/response bodies recorded
  per link side (proxy↔upstream / client↔proxy) for replay. Bilingual
  中文/English UI.
- **Activity heatmap**: a GitHub-style rolling mosaic of the last 48
  hours, three color encodings (requests / tokens / output), persisted
  to disk across restarts.
- **Model hijacking**: route any model name (e.g., `fable`) to any
  upstream model; the tray right-click menu switches routes on the fly.
- **Cross-platform tray app** (Windows / macOS / Linux), single binary,
  ~40% test code.

### Motivation

I use AI coding agents all day. Running Claude Code against Volcano ARK's
GLM endpoint, I kept hitting 429 throttling — the built-in retry was slow
and capped, but I noticed that manually disconnecting and reconnecting
usually resumed the stream right away. The failures were transient;
somebody just needed to retry patiently. So I built this proxy to do
exactly that.

Naturally, the proxy itself is built with the same AI agents: I define the
problems and judge the results as a heavy daily user; the agents write the
code. I'm not a Go developer — this is vibe coding: a tool for AI agents,
built by AI agents, directed by someone who depends on them all day.

### Screenshots

![Status page: per-direction upstream byte counters, token and cache-hit telemetry, and the activity heatmap overlay (last 48 hours, 15-minute cells)](docs/screenshot-status.jpg)

<details>
<summary>Stream viewer</summary>

![Stream viewer: in-flight streams rendered live as readable text or raw SSE, finished streams as an interactive JSON tree — per link side, with download options](docs/screenshot-stream.jpg)

</details>

<details>
<summary>Cache-hit details</summary>

![Cache-hit details: hit rate per real upstream model, plus measured cache lifetimes](docs/screenshot-cache.jpg)

</details>

<details>
<summary>Request log page</summary>

![Log page: per-request routing, retries, first-token and throughput timing](docs/screenshot-logs.jpg)

</details>

<details>
<summary>Config page</summary>

![Config page: in-browser JSON profile editing, with one-click Codex setup commands generated from the live config](docs/screenshot-config.jpg)

</details>

### Download

Prebuilt binaries are on the [Releases](https://github.com/aaahzb/proxy429/releases) page:

| Platform | File |
|---|---|
| Windows x64 | `Proxy429-windows-x64.exe` |
| Linux x64 | `Proxy429-linux-x64.tar.gz` (tar.gz keeps the executable bit) |
| macOS Apple Silicon (arm64) | `Proxy429-macOS-AppleSilicon-arm64.dmg` |

> [!WARNING]
> **macOS: "Apple cannot check Proxy429 for malicious software" on first
> launch.** The app is ad-hoc signed — no Apple Developer ID certificate,
> no Apple notarization — so Gatekeeper blocks the first launch of every
> downloaded copy. Two ways past it:
> 1. **System Settings → Privacy & Security**, scroll to the Security
>    section, click **Open Anyway** next to the Proxy429 entry, and
>    confirm. (On macOS 14 and earlier, right-click → **Open** works too.)
> 2. Or remove the quarantine flag once in Terminal:
>    `xattr -d com.apple.quarantine /Applications/Proxy429.app`
>
> Either is a one-time fix per install, but the warning returns after
> every update that replaces the app. Windows SmartScreen ("Windows
> protected your PC") is the same story for the unsigned exe: **More info
> → Run anyway**.

### Quick Start

1. Build with `build.sh` (outputs `release/proxy429`, or
   `release/Proxy429.app` on macOS).
2. Run it — a config file is generated on first launch.
3. Point your agent at `http://127.0.0.1:8080` (Claude Code) or the
   Responses endpoint (Codex CLI). Details in the docs below.

### Documentation

Full design documentation: [docs/DESIGN.md](docs/DESIGN.md) — retry
semantics, routing, protocol translation, cache-preserving rewrites,
observability. Also: [Usage guide](docs/usage.md) ·
[Windows quickstart](docs/windows-quickstart.md) ·
[Autostart setup](docs/autostart-setup.md)

### Acknowledgments

- [cc-switch](https://github.com/farion1231/cc-switch) (MIT, © 2025 Jason
  Young) — the Responses translation test suites are ported from its Rust
  tests; the thinking-mode mapping is aligned with cc-switch 3.20.0; the
  `[cc-switch: omitted N bytes]` marker is kept for client compatibility.

### License

[MIT](LICENSE) © 2026 aaahzb

---

## 中文

一个本地 LLM API 代理：夹在 Claude Code / Codex 等 AI 编程 agent 和上游模型
提供商之间，自动吃掉 429/5xx 限流（指数退避重试，含藏在 200 流里的错误），
支持模型路由、双协议翻译，带实时网页监控台。

> [!TIP]
> **👀 好奇你的编程 agent 到底在干嘛？** 打开控制台，看它现场直播
> 「思考」：在途流随 token 到达实时渲染成可读文本（可切原始 SSE），
> 完成的流保留完整请求/响应体、展开成交互式 JSON 树，按链路侧随便翻。
> 系统提示词、工具定义、缓存命中、重试——明厨亮灶，一览无余。学习
> Claude Code / Codex 工作原理最直观的方式，没有之一。

> [!TIP]
> **💰 用 Claude Code / Codex 跑第三方贵价套餐？别把额度浪费在安全
> 检查上。** 这两个工具会在你看不见的地方持续发小请求——安全分类器、
> guardian 审批。官方订阅的分类器已经免费，第三方端点却没有这种待遇：
> 它只看到一个普通请求，照样计在高级模型头上。配上 `classifier_route`
> 就有了——指向便宜模型（再加 `classifier_thinking: "off"` 让它无思考
> 秒回），这类后台消耗从此不碰主模型额度。状态页「分类器」计数卡如实
> 显示这类调用的真实频率，分流了多少一目了然。

功能亮点：
- **429/5xx 自动重试**：指数退避，连藏在 200 流里的限流错误也照抓；
  SSE 保活让客户端在重试期间永不超时。
- **模型路由**：通配符跨提供商路由，分类器/fast 模式专用路由，
  多模态与联网搜索兜底。
- **双协议翻译**：OpenAI Responses ↔ Anthropic Messages 全互转——
  工具调用、思考模式映射、字节级保序让上游缓存保持有效。
- **搜索信封**：搜索结果封进加密信封随响应返回，追问直接复读上次
  结果、不再重复搜索（翻译场景）。
- **关思考悄悄升级 low**：显式关思考的请求悄悄用 low 档发上游、回传
  剥掉思考块——规避 Kimi「关思考即降级 K2.8 无思考版」的暗坑，
  下游完全无感。
- **实时可观测性**：网页控制台直播在途流，token 与缓存命中统计、
  逐请求诊断；请求/响应体按链路侧分存（代理↔上游 / 客户端↔代理）
  随时回放。控制台中英双语。
- **活跃度热图**：GitHub 风格马赛克热图滚动覆盖最近 48 小时，
  请求数 / token 三种口径切换，数据落盘、重启不丢。
- **模型劫持**：任意模型名路由到任意上游模型，托盘右键一键切换。
- **跨平台托盘应用**：Windows / macOS / Linux，单二进制，
  ~40% 测试代码。

### 为什么有这个

我整天用 AI 编程 agent 干活。在火山方舟的 GLM 上跑 Claude Code 时老被
429 打断——自带重试又慢又有次数限制，但我发现手动断开重连**大多数时候**
立刻就能恢复：失败是暂时的，只是缺个有耐心重试的东西。于是有了这个代理。

这个代理本身也是用同样的 AI agent 造的：我定义问题、作为重度用户验收，
agent 写代码。我不是 Go 开发者，纯 vibe coding——为 AI agent 造的工具，
由 AI agent 造，由天天靠它们干活的人指挥。

### 截图

![状态页：分方向上游字节计数、token 与缓存命中统计，以及活跃度热图弹窗（近 48 小时、15 分钟一格）](docs/screenshot-status.jpg)

<details>
<summary>流查看器</summary>

![流查看器：在途流实时渲染为可读文本或原始 SSE，完成的流展开为交互式 JSON 树，按链路侧查看、可下载](docs/screenshot-stream.jpg)

</details>

<details>
<summary>缓存命中明细</summary>

![缓存命中明细：按真实上游模型统计命中率，并实测缓存存活时间](docs/screenshot-cache.jpg)

</details>

<details>
<summary>请求日志页</summary>

![日志页：每个请求的路由、重试、首字与吞吐耗时](docs/screenshot-logs.jpg)

</details>

<details>
<summary>配置页</summary>

![配置页：浏览器内编辑 JSON 配置，并按当前配置实时生成 Codex 一键配置命令](docs/screenshot-config.jpg)

</details>

### 下载

各平台预编译包见 [Releases](https://github.com/aaahzb/proxy429/releases) 页：

| 平台 | 文件 |
|---|---|
| Windows x64 | `Proxy429-windows-x64.exe` |
| Linux x64 | `Proxy429-linux-x64.tar.gz`（tar.gz 保留可执行权限） |
| macOS Apple Silicon（arm64） | `Proxy429-macOS-AppleSilicon-arm64.dmg` |

> [!WARNING]
> **macOS 首次启动报「无法验证是否包含恶意软件」**：应用为 ad-hoc 自签名
> （没有 Apple 开发者证书、未做 Apple 公证），Gatekeeper 会拦截所有下载
> 来源的首次启动。两种解法任选：
> 1. **系统设置 → 隐私与安全性**，拉到「安全性」区域，点 Proxy429 旁的
>    **「仍要打开」**并确认（macOS 14 及更早也可以右键 → 打开）。
> 2. 或在终端执行一次：
>    `xattr -d com.apple.quarantine /Applications/Proxy429.app`
>
> 两种都只需操作一次，但覆盖更新后警告会重新出现，需要再来一遍。Windows 的
> SmartScreen（「Windows 已保护你的电脑」）是同一性质的未签名提示：点
> 「更多信息 → 仍要运行」。

### 文档

完整设计文档见 [docs/DESIGN.md](docs/DESIGN.md)（重试语义、路由、协议翻译、
保缓存改写、可观测性；英文撰写）。另有英文文档：[使用说明](docs/usage.md) ·
[Windows 快速部署](docs/windows-quickstart.md) ·
[开机自启](docs/autostart-setup.md)

### 致谢与协议

- [cc-switch](https://github.com/farion1231/cc-switch)（MIT, © 2025 Jason
  Young）：Responses 翻译测试集移植自其 Rust 测试；thinking 映射与其
  3.20.0 对齐；协议标记为兼容沿用。
- 本项目以 [MIT](LICENSE) © 2026 aaahzb 开源。
