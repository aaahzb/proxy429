# proxy429 重做计划（从 00c08ab 基线出发）

背景：4d1672a 由另一个 AI 实现，已用 revert 提交 e575fe9 打回。
参考材料：.git/messy.diff（被打回实现的完整 diff）、.git/zhdoc.txt（097003d 原始中文文档正文，106 行）。

## 提交 B — 打捞两个好修复（独立于新功能）
- [x] logview.go：恢复 logViewerDocZH 常量（内容来自 .git/zhdoc.txt），logViewerHandler 中文分支补 `strings.Replace(page, "__DOC_BODY__", logViewerDocZH, 1)`（当前 bug：中文界面文档弹窗正文渲染成字面 `__DOC_BODY__`）
- [x] logview.go：uiLangHandler 写回 config.json 改为文本级编辑（setTopLevelJSONKey 风格），保留手写排版与字段顺序；现实现 json.MarshalIndent 全量重写
- [x] 测试：zh 文档替换、ui_lang 文本级写回（排版/顺序保留）

## 提交 C — translateNone2Low（仅 Responses 翻译口）
动机：Kimi 文档「关闭 thinking 后路由到 K2.8 Preview 无思考版」，不想让 K3 被路由到 K2.8。
- [x] Config 加 `TranslateNone2Low bool \`json:"translateNone2Low"\``（默认 false）；config.example.json 同步加；config_example_test.go 字段清单同步
- [x] responses.go：responsesToAnthropicTriple 加 none2Low 参数与 upgraded 返回；升级逻辑覆盖 historyValid 与 !historyValid 两条路径（被打回实现只覆盖前者，工具续推不可达是致命缺陷）
  - adaptive 模型：thinking={type:adaptive} + output_config.effort="low"
  - budget 模型：thinking={type:enabled, budget_tokens:2048}（受 maxTokens/2 上限、<1024 则不升级）
  - tool_choice 冲突分支要把 upgraded 复位为 false
- [x] main.go：ctxKeyNone2Low 透传；handler 里升级流 f.think="off->low"；一次性 400 回退：错误体含 thinking 字样时用文本级手术把 thinking 改回 disabled、删 output_config，attempt-- 重发（仿 search-strip 回退）
- [x] responses_stream.go：anthToRespStream 剥离思考块（bkDropped：不分配 outputIndex、不发事件、回滚）；usage 不动，如实透传
- [x] responses.go：anthropicToResponsesObject 非流式重建也剥离 thinking/redacted_thinking
- [x] logview.go：apiCell 双色徽标 [off->low]：off=#c586c0（[translate] 同色）、箭头 #9a9a9a、low=#d97757（Anthropic 橙）
- [x] 测试：升级形状（adaptive/budget）、!historyValid 升级、流式+非流式剥离、usage 保留
- [x] 文档：应用内文档中英两版、docs/使用说明.md、README.md

## 提交 D — 双链路报文查看（下游↔代理 / 代理↔上游）
- [x] flight 加 reqDown/reqDownTrunc/contentDown/fullContentDown + purgeFull/finishedFlight 镜像；仅当与上行侧有差异时才存（翻译流恒存；原生流被改写或 convertAlltoStream 重建才存）
- [x] main.go：responsesHandler 原始 raw 经 ctxKeyReqDown 传入；handler 入口留 downBody，原生流 bytes.Equal 对比决定是否存 reqDown；重试循环分发点每次 setReqBody（最后一次为准，覆盖 400 重拆）
- [x] responses_stream.go：translatingWriter 加 setDownTap，所有 dst.Write 路径（emit/finish/finishBuffered/错误）都过 tap → appendContentDown；collectStreamToJSON 的重建 JSON tee 改 appendContentDown
- [x] logview.go：/__flight 与 /__flightreq 支持 ?side=up|down，缺侧回退另一侧并带 X-Proxy429-Side 头；flightInfo/recentFlights JSON 加 reqDown/respDown/reqDownTrunc/hasFullDown
- [x] 查看器 UI：flightViewSide 状态（在途默认 'up'=代理↔上游链路），请求体/返回体各加侧切换按钮，回退提示，下载/交互树随侧走；flightFlags 扩 {rt,hf,rd,sd,rdt,hfd}
- [x] EN 翻译表补齐所有新增中文串
- [x] 测试：双侧端点回退、side 参数、tap 覆盖全路径
- [x] 文档：应用内文档中英两版、docs/使用说明.md、README.md

## 提交 E — 界面语言移出配置文件、链路按钮去 emoji 箭头、配置文件零重排
- [x] logview.go：链路按钮「链路:代理↔上游/下游↔代理」的 ↔（emoji 呈现，太挤）改 ASCII `<->`；enHTMLRepl 两对同步
- [x] 界面语言改存 program-settings.txt（配置目录下，与 active-config.txt 同族的程序设置载体；key=value 行式，可扩展）——程序设置与上游路由配置分离
- [x] Config 删 ui_lang 字段与校验；reloadConfig/switchConfig 不再触碰界面语言
- [x] 一次性迁移：旧配置文件残留的 ui_lang → program-settings.txt 并从配置删除（setTopLevelJSONValue 文本级，不动排版）
- [x] uiLangHandler POST 改写 program-settings.txt：不再写配置、不再 reloadConfig
- [x] 根除重排：ui_lang 迁出后代码里再无对既有配置文件的程序化写入（配置编辑器保存本就是用户原文 verbatim）
- [x] 测试：lang_test.go 三个用例改新行为；新增 program-settings 读写往返、迁移用例
- [x] README 两处 ui_lang 描述更新
- [x] go build/vet/test 全绿 + build.sh 重出 release

## 提交 F — translateNone2Low 覆盖「历史不可回放的代理兜底关思考」（#11/#12 实况修复）
背景：用户开启 translateNone2Low 后 #11/#12 上游仍收到关思考。诊断闭环：Codex 发的 effort 是
max（codex config model_reasoning_effort="max"，enabled-reasoning-efforts 无 none 档），显式关
=false；是 trailingTurnSupportsThinking=false（工具续轮缺签名思考块）触发 !historyValid 兜底分支
把 thinking 显式关成 disabled——关思考恰会触发 Kimi 把 K3 路由到 K2.8 无思考版，正是本参数要防
的事，却从另一扇门发生；参数按字面 spec（只升级下游显式关）正确地没介入。
- [x] responses.go：第 4 返回值 upgraded bool 改三态 n2l int（0 未升级 / 1 隐式升级=回传剥思考块+[off->low]徽标 / 2 试 low=不剥思考块只带 400 兜底）
- [x] !historyValid 分支：none2Low 开时不再自行关思考——下游显式关→隐式升级（原行为）；其余（下游本就要思考）→试 low 发上游（上游拒则主 handler 既有 400 兜底回退关思考重发）；开关关→保持 cc-switch 的显式关闭
- [x] 试 low 不剥思考块的理由：下游本来就要思考，思考块随回传带回签名块，下一轮历史自愈
- [x] main.go：ctxKeyNone2Low 改携 int；[off->low] 徽标仅 n2l==1；400 兜底罩 n2l!=0；Config 字段注释更新
- [x] 测试：#11/#12 复现形状（工具续轮缺签名思考块 + effort max + 路由 thinkStyle=adaptive）→ 上游收到 adaptive+low 且 n2l==2（非隐式、不剥）；开关关 → 仍 disabled；既有用例签名随三态化更新
- [x] 文档同步：README 中英、docs/使用说明.md、应用内文档配置表（logview.go 中英两版）
- [x] gofmt/vet/test 全绿 + build.sh 重出 release（含跨平台矩阵）

## 收尾
- [x] go build ./... 与 go test ./... 全绿（含旧测试签名更新）
- [x] README.md / docs 同步（用户全局规则）
- [ ] 删除 TODO.md（需用户确认）
