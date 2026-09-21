# proxy429 重做计划（从 00c08ab 基线出发）

背景：4d1672a 由另一个 AI 实现，已用 revert 提交 e575fe9 打回。
参考材料：.git/messy.diff（被打回实现的完整 diff）、.git/zhdoc.txt（097003d 原始中文文档正文，106 行）。

## 提交 B — 打捞两个好修复（独立于新功能）
- [x] logview.go：恢复 logViewerDocZH 常量（内容来自 .git/zhdoc.txt），logViewerHandler 中文分支补 `strings.Replace(page, "__DOC_BODY__", logViewerDocZH, 1)`（当前 bug：中文界面文档弹窗正文渲染成字面 `__DOC_BODY__`）
- [x] logview.go：uiLangHandler 写回 config.json 改为文本级编辑（setTopLevelJSONKey 风格），保留手写排版与字段顺序；现实现 json.MarshalIndent 全量重写
- [x] 测试：zh 文档替换、ui_lang 文本级写回（排版/顺序保留）

## 提交 C — translateNone2Low（仅 Responses 翻译口）
动机：Kimi 文档「关闭 thinking 后路由到 K2.8 Preview 无思考版」，不想让 K3 被路由到 K2.8。
- [ ] Config 加 `TranslateNone2Low bool \`json:"translateNone2Low"\``（默认 false）；config.example.json 同步加；config_example_test.go 字段清单同步
- [ ] responses.go：responsesToAnthropicTriple 加 none2Low 参数与 upgraded 返回；升级逻辑覆盖 historyValid 与 !historyValid 两条路径（被打回实现只覆盖前者，工具续推不可达是致命缺陷）
  - adaptive 模型：thinking={type:adaptive} + output_config.effort="low"
  - budget 模型：thinking={type:enabled, budget_tokens:2048}（受 maxTokens/2 上限、<1024 则不升级）
  - tool_choice 冲突分支要把 upgraded 复位为 false
- [ ] main.go：ctxKeyNone2Low 透传；handler 里升级流 f.think="off->low"；一次性 400 回退：错误体含 thinking 字样时用文本级手术把 thinking 改回 disabled、删 output_config，attempt-- 重发（仿 search-strip 回退）
- [ ] responses_stream.go：anthToRespStream 剥离思考块（bkDropped：不分配 outputIndex、不发事件、回滚）；usage 不动，如实透传
- [ ] responses.go：anthropicToResponsesObject 非流式重建也剥离 thinking/redacted_thinking
- [ ] logview.go：apiCell 双色徽标 [off->low]：off=#c586c0（[translate] 同色）、箭头 #9a9a9a、low=#d97757（Anthropic 橙）
- [ ] 测试：升级形状（adaptive/budget）、!historyValid 升级、流式+非流式剥离、usage 保留
- [ ] 文档：应用内文档中英两版、docs/使用说明.md、README.md

## 提交 D — 双链路报文查看（下游↔代理 / 代理↔上游）
- [ ] flight 加 reqDown/reqDownTrunc/contentDown/fullContentDown + purgeFull/finishedFlight 镜像；仅当与上行侧有差异时才存（翻译流恒存；原生流被改写或 convertAlltoStream 重建才存）
- [ ] main.go：responsesHandler 原始 raw 经 ctxKeyReqDown 传入；handler 入口留 downBody，原生流 bytes.Equal 对比决定是否存 reqDown；重试循环分发点每次 setReqBody（最后一次为准，覆盖 400 重拆）
- [ ] responses_stream.go：translatingWriter 加 setDownTap，所有 dst.Write 路径（emit/finish/finishBuffered/错误）都过 tap → appendContentDown；collectStreamToJSON 的重建 JSON tee 改 appendContentDown
- [ ] logview.go：/__flight 与 /__flightreq 支持 ?side=up|down，缺侧回退另一侧并带 X-Proxy429-Side 头；flightInfo/recentFlights JSON 加 reqDown/respDown/reqDownTrunc/hasFullDown
- [ ] 查看器 UI：flightViewSide 状态（在途默认 'up'=代理↔上游链路），请求体/返回体各加侧切换按钮，回退提示，下载/交互树随侧走；flightFlags 扩 {rt,hf,rd,sd,rdt,hfd}
- [ ] EN 翻译表补齐所有新增中文串
- [ ] 测试：双侧端点回退、side 参数、tap 覆盖全路径
- [ ] 文档：应用内文档中英两版、docs/使用说明.md、README.md

## 收尾
- [ ] go build ./... 与 go test ./... 全绿（含旧测试签名更新）
- [ ] README.md / docs 同步（用户全局规则）
- [ ] 删除 TODO.md（需用户确认）
