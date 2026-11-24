<div align="center">

# Proxy429

**See What Your AI Agents Are Actually Doing.**  
**实时看见 AI Agent 正在做什么。**

Inspect live model requests and responses, explore captured payloads, and monitor token usage, cache hits, retries, and latency in a local web console.

在本地网页控制台中实时查看模型请求与响应，检查捕获的载荷，并监控 Token、缓存命中、重试和延迟。

[![CI](https://github.com/aaahzb/proxy429/actions/workflows/ci.yml/badge.svg)](https://github.com/aaahzb/proxy429/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/aaahzb/proxy429)](https://github.com/aaahzb/proxy429/releases)
[![License](https://img.shields.io/github/license/aaahzb/proxy429)](LICENSE)

[English](#english) | [中文](#中文)

</div>

---

## English

Proxy429 is a local LLM API proxy for Claude Code, Codex CLI, and compatible OpenAI/Anthropic API clients. It retries requests through transient upstream failures, routes and translates between API formats, and shows the traffic between your agent and its model provider in a local web console.

![Proxy429 status page showing live traffic statistics and activity](docs/screenshot-status.jpg)

### Quick Start

1. **Download and launch** the release for your operating system. Proxy429 creates its configuration file on first launch.
2. **Configure your client** to use the local proxy:
   - Claude Code: `http://127.0.0.1:8080`
   - Codex CLI (Responses API): `http://127.0.0.1:8081`

   The console's configuration page can generate setup commands for Codex based on the current configuration.
3. **Inspect traffic** at `http://127.0.0.1:8080/__logs`.

For detailed setup and configuration, see the [Usage Guide](docs/usage.md).

### See what your agent sends and receives

Open the local console while your agent is working:

- **Inspect live streams.** Watch in-flight output arrive in real time, rendered as readable text or raw SSE.
- **Explore full payloads.** Inspect completed request and response bodies in an interactive JSON tree, with per-link-side views.
- **Examine model context.** Review the captured payload sent upstream, including message content and other request fields present in the body.
- **Download for deeper debugging.** Save request and output payloads for offline inspection.
- **Understand runtime behavior.** See token and byte counts, cache-hit rates, retries, first-token latency (TTFT), and throughput.
- **Review recent activity.** Use the rolling 48-hour activity heatmap to see request, token, or output activity over time.

The console answers practical questions. What did the agent send to the model? What came back? How much context and output were involved? Did the request retry, hit cache, or take unusually long?

> **Subagent note:** Proxy429 exposes captured request/response payloads. Whether a payload belongs to a particular subagent, and how parent/child calls relate to one another, depends on the information available in the captured requests. Do not treat this as automatic parent-child tracing unless that relationship is explicitly identifiable.

### Why use Proxy429?

- **Automatic retries.** Retry configured upstream status codes with exponential backoff, including common 429 and 5xx failures. Rate-limit errors that arrive inside HTTP 200 SSE streams also trigger retries.
- **Flexible routing.** Route model patterns to upstream providers. Configure dedicated routes for supported request types such as classifier, fast-mode, multimodal, or web-search fallback requests.
- **Protocol translation.** Translate between the OpenAI Responses and Anthropic Messages API formats, including tool calls and reasoning-mode mapping.
- **Cache visibility.** Inspect cache-hit statistics by upstream model, and review measured cache lifetimes.
- **Model remapping.** Route a client-facing model name to a different upstream model, and switch routes from the tray menu.
- **Search follow-ups.** In the protocol-translation path, carry search results in an encrypted response envelope for reuse by follow-up requests.
- **Empty-search guard.** Optionally replace all-empty Claude Code standalone-search responses with a fixed no-results stream on selected routes. This stops a fabricated answer from prior knowledge.
- **Shared route defaults.** A top-level `lazyApply` object fills supported per-route fields into every route-like object that omits them: behavior fields (search summary and thinking, empty-search guard, thinking shape, capability flags) and the connection trio `url` / `api` / `model`. Write the upstream once and a route can shrink to `{"pattern": "*sonnet*", "effort": "high"}`. Explicit local values and per-object `noLazyApply` exceptions still win.
- **Thinking behavior.** Configured thinking-mode handling adapts requests and responses for supported upstreams. A route-level `effort` field pins adaptive-thinking requests to a fixed level (low/mid/high/max) whatever the client asked for; the console's model column shows the configured level as `[level]` before the target model.
- **Cross-platform tray app.** Windows, macOS, and Linux builds, with a bilingual Chinese/English web console.

See [Design and Architecture](docs/DESIGN.md) for implementation details and behavior.

### Download

Download prebuilt binaries from [GitHub Releases](https://github.com/aaahzb/proxy429/releases).

| Platform | Release asset |
|---|---|
| Windows x64 | `Proxy429-windows-x64.exe` |
| Linux x64 | `Proxy429-linux-x64.tar.gz` |
| macOS Apple Silicon (arm64) | `Proxy429-macOS-AppleSilicon-arm64.dmg` |

> [!WARNING]
> **macOS first launch:** Proxy429 is ad-hoc signed and is not notarized with an Apple Developer ID. Gatekeeper may block the first launch.
>
> - Open **System Settings → Privacy & Security**, find the Proxy429 message, choose **Open Anyway**, and confirm. On macOS 14 and earlier, right-clicking the app and choosing **Open** may also work.
> - Alternatively, remove the quarantine attribute in Terminal with `xattr -d com.apple.quarantine /Applications/Proxy429.app` (adjust the path if the app is installed elsewhere).
>
> The warning may return after an update replaces the app. On Windows, SmartScreen may also warn because the executable is unsigned. Review the source and release before choosing **More info → Run anyway**.

### Screenshots

<details>
<summary><strong>Stream viewer.</strong> Inspect live streams and completed request/response bodies</summary>

![Stream viewer showing in-flight streams as readable text or raw SSE and completed streams as an interactive JSON tree](docs/screenshot-stream.jpg)

</details>

<details>
<summary><strong>Cache-hit details.</strong> Review cache-hit rates and cache lifetimes</summary>

![Cache-hit details showing hit rates by upstream model and measured cache lifetimes](docs/screenshot-cache.jpg)

</details>

<details>
<summary><strong>Request log.</strong> Review routing, retries, and timing</summary>

![Request log showing per-request routing, retries, first-token latency, and throughput timing](docs/screenshot-logs.jpg)

</details>

<details>
<summary><strong>Configuration.</strong> Edit profiles and generate client setup commands</summary>

![Configuration page showing JSON profile editing and generated Codex setup commands](docs/screenshot-config.jpg)

</details>

### Documentation

- [Usage Guide](docs/usage.md)
- [Windows Quick Start](docs/windows-quickstart.md)
- [Autostart Setup](docs/autostart-setup.md)
- [Design and Architecture](docs/DESIGN.md)

### Build from Source

Run `bash build.sh`. Build artifacts are written to `release/`.

The code is organized by subsystem: `cmd/proxy429` is the entry point; `internal/proxy` holds the proxy core (forwarding, routing, search guard, tray); `internal/logview` the web console; and `internal/{config,translate,recorder,platform,jsonutil}` the supporting packages. See the file-structure table in [Design and Architecture](docs/DESIGN.md).

### Motivation

I built Proxy429 after repeatedly hitting transient 429 throttling while using Claude Code with a GLM endpoint on Volcano Engine ARK. Manually reconnecting often let the work continue, which suggested that a patient retrying proxy could make these failures less disruptive.

Proxy429 is also built with AI coding agents. I define the problems and evaluate the results as a daily user, while the agents help implement the code.

### Acknowledgments

- [cc-switch](https://github.com/farion1231/cc-switch) (MIT, © 2025 Jason Young). Parts of the Responses translation test suite are ported from its Rust tests. Thinking-mode mapping follows cc-switch 3.20.0, and the `[cc-switch: omitted N bytes]` marker is retained for compatibility.

### Contributing

Issues and pull requests are welcome. When reporting a bug, include the stream ID shown in the console, so the captured request and response can be inspected.

### License

[MIT](LICENSE) © 2026 aaahzb

---

## 中文

Proxy429 是一个面向 Claude Code、Codex CLI 及兼容 OpenAI/Anthropic API 客户端的本地 LLM API 代理。它能扛过上游的临时故障并自动重试，在 API 格式之间做路由与翻译，并在本地网页控制台中展示你的 Agent 与模型提供商之间的流量。

![Proxy429 状态页：上游字节计数、token 与缓存命中统计，以及近 48 小时活跃度热图](docs/screenshot-status.jpg)

### 快速上手

1. **下载并运行：** 从下方 Release 页面下载对应操作系统的版本。首次运行时，Proxy429 会自动生成配置文件。
2. **配置客户端**使用本地代理：
   - Claude Code：`http://127.0.0.1:8080`
   - Codex CLI（Responses API）：`http://127.0.0.1:8081`

   控制台的配置页面可以根据当前配置生成 Codex 设置命令。
3. **查看请求流量：** 打开 `http://127.0.0.1:8080/__logs`。

完整配置方法请参阅[使用指南](docs/usage.md)。

### 看看你的 Agent 实际在收发什么

Agent 工作时打开本地控制台：

- **实时流。** 实时查看在途输出，可选用可读文本或原始 SSE 渲染。
- **完整载荷。** 在交互式 JSON 树中检查完成的请求体与响应体，并可分别查看请求侧与响应侧。
- **模型上下文。** 查看实际发往上游的捕获载荷，包括消息内容和请求体中的其他字段。
- **下载排查。** 保存请求与输出载荷，供离线检查。
- **运行指标。** 查看 token 和字节计数、缓存命中率、重试、首 token 延迟（TTFT）与吞吐。
- **近期活跃度。** 用滚动的 48 小时活跃度热图查看请求、token 或输出量随时间的变化。

控制台能回答这些实际问题：Agent 发给模型的到底是什么？回来的又是什么？上下文和输出有多大？这次请求是否重试、命中缓存或耗时异常？

> **子代理提示：** Proxy429 暴露的是捕获到的请求/响应载荷。某个载荷是否属于特定子代理、父子调用如何关联，取决于捕获请求里可用的信息。除非这种关系可以被明确识别，否则不要把它当作自动的父子链路追踪。

### 为什么使用 Proxy429？

- **自动重试。** 对配置的上游状态码做指数退避重试，覆盖常见的 429 和 5xx 故障。HTTP 200 SSE 流内部的限流错误同样会触发重试。
- **灵活路由。** 按模型名模式把请求路由到不同上游，并可为分类器、fast 模式、多模态、联网搜索兜底等请求类型配置专用路由。
- **协议翻译。** 在 OpenAI Responses 与 Anthropic Messages 两种 API 格式之间翻译，包括工具调用和思考模式映射。
- **缓存可见。** 按上游模型查看缓存命中统计，并查看实测的缓存存活时间。
- **模型名映射。** 把客户端使用的模型名路由到指定的上游模型，也可以通过托盘菜单切换路由。
- **搜索结果复用。** 在协议翻译路径中，把搜索结果放进加密响应信封，供后续请求复用。
- **空搜索硬保障。** 可按路由把 Claude Code 独立搜索子代理的全空搜索响应替换为固定的无结果流，阻止上游凭空硬答。
- **共享路由默认值。** 顶层 `lazyApply` 把支持的字段填充进每个省略了这些字段的类路由对象：既包括行为字段（搜索摘要与思考、空搜守卫、思考形态、能力开关），也包括连接三件套 `url` / `api` / `model`。上游只写一次，路由可以简化成 `{"pattern": "*sonnet*", "effort": "high"}`。显式本地值和逐对象的 `noLazyApply` 例外仍然优先。
- **思考模式。** 按配置的思考模式处理，对支持的上游改写请求与响应，例如把明确关闭思考的请求映射为上游 low 档并移除返回的思考块。路由级 `effort` 字段可把 adaptive 思考请求钉到固定档位（low/mid/high/max），无视客户端原本的档位；状态页模型列会把配置的档位显示在目标模型前，如 `[high] deepseek-flash`。
- **跨平台托盘应用。** Windows、macOS、Linux 三平台的构建，以及中英双语的网页控制台。

实现细节和行为说明见[设计与架构文档](docs/DESIGN.md)。

### 下载

各平台预编译版本见 [GitHub Releases](https://github.com/aaahzb/proxy429/releases)。

| 平台 | Release 文件 |
|---|---|
| Windows x64 | `Proxy429-windows-x64.exe` |
| Linux x64 | `Proxy429-linux-x64.tar.gz` |
| macOS Apple Silicon（arm64） | `Proxy429-macOS-AppleSilicon-arm64.dmg` |

> [!WARNING]
> **macOS 首次启动提示无法验证应用：** Proxy429 使用 ad-hoc 签名，没有 Apple Developer ID 证书，也未进行 Apple 公证，因此 Gatekeeper 可能拦截首次启动。
>
> - 打开**系统设置 → 隐私与安全性**，找到 Proxy429 的提示，点击**仍要打开**并确认。macOS 14 及更早版本也可以尝试右键应用并选择**打开**。
> - 或者在终端执行 `xattr -d com.apple.quarantine /Applications/Proxy429.app`（如果应用安装在其他位置，请调整路径）。
>
> 更新替换应用后，警告可能再次出现。Windows 版本也可能因可执行文件未签名而触发 SmartScreen；选择“更多信息 → 仍要运行”前，请先确认源码和 Release 的可信度。

### 截图

<details>
<summary><strong>流查看器。</strong> 查看实时流与完整请求/响应体</summary>

![流查看器：在途流可显示为可读文本或原始 SSE，完成的流可展开为交互式 JSON 树](docs/screenshot-stream.jpg)

</details>

<details>
<summary><strong>缓存命中明细。</strong> 查看各上游模型的命中率和缓存存活时间</summary>

![缓存命中明细：按上游模型展示命中率与实测缓存存活时间](docs/screenshot-cache.jpg)

</details>

<details>
<summary><strong>请求日志。</strong> 检查路由、重试和耗时</summary>

![请求日志：展示每个请求的路由、重试、首 token 延迟和吞吐耗时](docs/screenshot-logs.jpg)

</details>

<details>
<summary><strong>配置页面。</strong> 编辑配置并生成客户端设置命令</summary>

![配置页面：在浏览器中编辑 JSON 配置，并生成 Codex 设置命令](docs/screenshot-config.jpg)

</details>

### 文档

- [使用指南](docs/usage.md)
- [Windows 快速上手](docs/windows-quickstart.md)
- [开机自启](docs/autostart-setup.md)
- [设计与架构](docs/DESIGN.md)（英文）

### 从源码构建

运行 `bash build.sh`，构建产物会输出到 `release/`。

代码按子系统组织：`cmd/proxy429` 是入口；`internal/proxy` 是代理核心（转发、路由、空搜守卫、托盘）；`internal/logview` 是网页控制台；`internal/{config,translate,recorder,platform,jsonutil}` 为支撑包。完整文件结构表见[设计与架构文档](docs/DESIGN.md)。

### 项目缘起

我在火山引擎 ARK 的 GLM 端点上使用 Claude Code 时，经常遇到临时性的 429 限流。手动断开并重连往往就能继续工作，这让我意识到：一个能够耐心重试的代理，可以减少这类故障对工作流的干扰。

Proxy429 本身也是借助 AI 编程 Agent 开发的：我负责定义问题，并以日常重度用户的身份验收结果，Agent 协助完成代码实现。

### 致谢

- [cc-switch](https://github.com/farion1231/cc-switch)（MIT，© 2025 Jason Young）：部分 Responses 翻译测试移植自其 Rust 测试；思考模式映射参考 cc-switch 3.20.0，并为兼容性保留 `[cc-switch: omitted N bytes]` 标记。

### 参与贡献

欢迎提交 Issue 和 Pull Request。报 bug 时附上控制台里的流 ID，可直接查看捕获的请求与响应。

### 许可证

本项目采用 [MIT 许可证](LICENSE)，© 2026 aaahzb。
