# Windows 快速部署说明

新版 Proxy429 是纯托盘应用,**没有控制台黑窗口**。状态、日志、配置都在本地网页控制台里看。

## 1. 准备文件

把下面文件放到一个目录,例如 `C:\RUN_Utility\Proxy429\`:

- `proxy429.exe`
- `config.json`(由 `config.example.json` 改名并填入真实 API key)
- `Windows快速部署说明.md`(本文件,供参考)

## 2. 配置 `config.json`

复制 `config.example.json` 为 `config.json`,把里面的占位 API key 换成你自己的:

- `upstream`: 默认上游,例如火山/ARK 地址
- `routes[].api`: 各路由模型对应上游的 key
- `classifier_route.api`、`fast_route.api`、`multimodal_fallback.api`、`search_fallback.api`: 各兜底路由的 key
- `search_fallback` 支持搜索摘要模式（`summary_mode: true`）:代理分两步自建响应(step1 搜索 + step2 摘要),不调用主力;摘要详细程度用 `summary_level`(`low`/`mid`/`high`/`max`,默认 `low`)控制,详见 README.md「搜索摘要模式」。
- `routes[]` 内也可配 `"enhance_search": {...}` 启用**增强搜索**:主力支持搜索时,带搜索工具的请求不调主力,改用该 route 自己的上游走 kimi 摘要模式。详见 README.md「增强搜索」。

常用字段速查:

| 字段 | 说明 |
|------|------|
| `listen` | 监听地址,默认 `127.0.0.1:8080` |
| `allow_remote` | 是否允许内网访问,默认 `false` |
| `log_file` | 日志文件路径,建议先设 `"proxy.log"` 方便排错 |
| `ping_interval_s` | 429 重试 SSE ping 间隔,默认 `5` |
| `max_retries` | 最大重试次数,默认 `5` |
| `retry_status_codes` | 触发重试的状态码,默认 `[429, 500, 502, 503, 504]` |

配置也可以在启动后通过网页控制台「配置」标签在线修改。`allow_remote`(内网访问)也可在「状态」标签点「开启内网访问」按钮切换,开启需二次确认。

### 多配置文件

支持在同一目录放多份配置(如 `config.json`、`config-work.json`),随时切换且即时生效,在途请求不打断(继续用旧配置跑完):

- **托盘**:右键图标 ->「切换配置」子菜单列出所有 `.json`,当前项打勾,点击即时切换;「刷新列表」用于新增文件后重建菜单。
- **网页**:配置区下拉框切换;「新建」用 `config.example.json` 作空白模板创建并切换;「重命名」「删除」需输入文件名(删除两次确认,不可删除当前生效的配置)。
- **状态页**:状态灯行右侧显示当前生效的配置文件名;在途流表格每行带状态灯(🟢转发中/🟡等待/⚪请求)。

配置目录沿用启动解析顺序:当前目录有 `config.json` 优先,否则用 `%AppData%\proxy429\`。多份配置都放同一目录。

## 3. 启动

双击 `proxy429.exe`,系统托盘会出现状态灯图标:

- ⚪ 灰灯 = 空闲
- 🟡 黄灯 = 已发请求,等待上游响应
- 🟢 绿灯 = 正在流式转发

右键托盘图标 → **「查看日志」**,浏览器会打开:

```
http://127.0.0.1:8080/__logs
```

> 看不到托盘图标?在 `config.json` 里加 `"log_file": "proxy.log"`,重启后看 `proxy.log` 里的启动日志。

## 4. 让 Claude Code 走代理

在启动 Claude Code 的终端里执行:

```bash
set ANTHROPIC_BASE_URL=http://127.0.0.1:8080
set ANTHROPIC_AUTH_TOKEN=你的APIKey
claude
```

注意:

- 必须是启动 `claude` 的同一个终端。
- 如果 `routes` 里已经配了 `api`,`ANTHROPIC_AUTH_TOKEN` 可填任意非空串,代理会覆盖成路由里的 key。
- ARK 等只认 `Authorization: Bearer` 的上游,必须用 `ANTHROPIC_AUTH_TOKEN`,不是 `ANTHROPIC_API_KEY`。

## 5. 退出

右键托盘图标 → **「退出代理」**。

关掉浏览器标签页**不会**退出代理,只是隐藏网页控制台。

## 6. 排错

| 现象 | 处理 |
|------|------|
| 托盘图标没出现 | 加 `"log_file": "proxy.log"`,重启,看 `proxy.log` |
| 网页控制台打不开 | 检查 `listen` 端口是否被占用;改端口后需重启代理 |
| Claude Code 报 API error | 看网页控制台「日志」标签的 429 重试日志;检查 `ANTHROPIC_BASE_URL` 是否设置正确 |
| 想保留日志 | 配置里设 `"log_file": "proxy.log"`,默认只在内存保留 500 行供网页查看 |

## 7. 重新编译

在项目根目录(含 `go.mod` 的那层):

```bash
bash build.sh
```

Windows 产物在 `release/proxy429.exe`。

完整配置说明、路由规则、429 保活机制等见项目根 `README.md`。
