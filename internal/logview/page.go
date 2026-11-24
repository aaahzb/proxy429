package logview

import (
	"sort"
	"strings"
)

// ---- Console English version ----
// logViewerHTML is the Chinese master page; renderLogViewerEN swaps the HTML static copy and
// JS-built UI strings to English per the mapping table; alert/confirm/prompt are translated by the page-footer i18n script.
// When adding page copy, both the Chinese and English sides must be updated; TestRenderLogViewerENNoChinese is the leak-proof backstop.
const logViewerHTML = `<!DOCTYPE html>
<html lang="zh">
<head>
<meta charset="utf-8">
<title>Proxy429 控制台</title>
<style>
  * { box-sizing: border-box; }
  body { margin:0; background:#1e1e1e; color:#d4d4d4; font-family:Menlo,Consolas,"Courier New",monospace; font-size:12px; }
  #bar { position:sticky; top:0; z-index:2; background:#252526; border-bottom:1px solid #333; display:flex; gap:0; align-items:stretch; }
  .tab { padding:8px 16px; color:#9a9a9a; cursor:pointer; border-right:1px solid #333; }
  .tab.active { color:#d4d4d4; background:#1e1e1e; border-bottom:2px solid #4ec9b0; }
  #hint { margin-left:auto; align-self:center; padding:0 12px; color:#6a6a6a; }
  .pane { display:none; padding:10px 14px; }
  .pane.active { display:block; }
  .st { font-weight:bold; }
  .idle { color:#969696; } .wait { color:#d9b34a; } .active { color:#4ec9b0; } .dead { color:#c75; }
  .cards { display:flex; flex-wrap:wrap; gap:8px; margin-bottom:12px; }
  .card { background:#252526; border:1px solid #333; border-radius:4px; padding:8px 12px; min-width:96px; }
  .card .k { color:#9a9a9a; font-size:11px; }
  .card .v { color:#d4d4d4; font-size:16px; margin-top:2px; }
  table { border-collapse:collapse; width:100%; }
  th,td { text-align:left; padding:4px 8px; border-bottom:1px solid #2a2a2a; }
  th { color:#9a9a9a; font-weight:normal; }
  .thq { display:inline-block; width:13px; height:13px; line-height:12px; margin-left:3px; border:1px solid #6a6a6a; border-radius:50%; color:#9a9a9a; font-size:10px; text-align:center; cursor:help; vertical-align:1px; }
  #log { white-space:pre-wrap; word-break:break-word; line-height:1.45; }
  #cfg { width:100%; min-height:60vh; background:#1e1e1e; color:#d4d4d4; border:1px solid #333; border-radius:4px; padding:8px; font:inherit; }
  #codexBox { margin-top:14px; border-top:1px solid #2a2a2a; padding-top:10px; }
  #codexBox .t { color:#9a9a9a; margin-bottom:6px; }
  #codexBox .dim { color:#777; font-size:12px; margin-top:4px; }
  #codexGen .cmdRow { display:flex; align-items:center; gap:6px; margin:4px 0; }
  #codexGen .cmdRow .os { flex:none; width:165px; color:#9a9a9a; font-size:12px; }
  #codexGen .cmdRow code { flex:1; min-width:0; background:#181818; color:#8ec88e; border:1px solid #333; border-radius:4px; padding:6px 8px; font:12px/1.4 Consolas,monospace; white-space:nowrap; overflow-x:auto; }
  #codexModelCustom { background:#1e1e1e; color:#d4d4d4; border:1px solid #333; border-radius:3px; padding:4px 6px; font:inherit; }
  #codexEnable { background:#2a2410; border:1px solid #6b5d1f; border-radius:4px; padding:8px 10px; margin-bottom:8px; color:#d8c27a; }
  #codexEnable .dim { color:#9a8a55; font-size:12px; margin-top:4px; }
  #codexGen.off { opacity:.35; pointer-events:none; }
  button { background:#264f78; color:#d4d4d4; border:1px solid #3a6ea5; border-radius:3px; padding:5px 12px; cursor:pointer; font:inherit; }
  button:hover { background:#2f6090; }
  button.ghost { background:#333; border-color:#444; }
  button.ghost:disabled { opacity:0.4; cursor:not-allowed; }
  button.danger { background:#5a2a2a; border-color:#7a3a3a; color:#ffb0b0; }
  button.danger:hover { background:#6a3a3a; }
  #cfgMsg { margin-left:10px; }
  .ok { color:#4ec9b0; } .err { color:#c75; }
  #cfgPath { color:#6a6a6a; margin-bottom:8px; }
  /* 解析视图内容块样式 */
  .ss-think { color:#9a9a9a; font-style:italic; white-space:pre-wrap; word-break:break-all; }
  .ss-tool { margin:4px 0; padding:4px 6px; background:#2a2a2a; border-left:3px solid #4ec9b0; white-space:pre-wrap; word-break:break-all; }
  .ss-tool b { color:#4ec9b0; }
  .ss-tool code { color:#9cdcfe; white-space:pre-wrap; word-break:break-all; }
  .ss-search { margin:4px 0; padding:4px 6px; background:#2a2515; border-left:3px solid #dcdcaa; white-space:pre-wrap; word-break:break-all; }
  .ss-search .ss-url { color:#569cd6; }
  .ss-empty { color:#9a9a9a; }
  .ss-error { margin:4px 0; padding:4px 6px; background:#3a1a1a; border-left:3px solid #f48771; color:#f48771; white-space:pre-wrap; word-break:break-all; }
  /* 交互式 JSON 树（流查看器「交互式JSON」按钮） */
  .jt-line { line-height:1.5; }
  .jt-kids { margin-left:18px; border-left:1px dotted #333; }
  .jt-head { cursor:pointer; user-select:none; }
  .jt-head:hover { background:#2a2a2a; }
  .jt-arrow { color:#888; display:inline-block; width:14px; }
  .jt-key { color:#9cdcfe; }
  .jt-str { color:#ce9178; white-space:pre-wrap; word-break:break-all; }
  .jt-num { color:#b5cea8; }
  .jt-bool { color:#569cd6; }
  .jt-null { color:#808080; }
  .jt-count { color:#808080; }
  /* Usage docs browser: full-screen modal, generated nav, live search, copy buttons */
  #docModal { position:fixed; inset:0; background:rgba(0,0,0,.65); z-index:60; display:none; align-items:center; justify-content:center; }
  .docFrame { background:#1a1a1a; border:1px solid #555; border-radius:8px; width:min(1180px,96vw); height:92vh; display:flex; flex-direction:column; overflow:hidden; }
  .docHead { display:flex; align-items:center; gap:12px; padding:12px 16px; border-bottom:1px solid #333; flex:none; }
  .docTitle { font-size:15px; font-weight:bold; color:#d4d4d4; white-space:nowrap; }
  .docSearch { flex:1; min-width:0; background:#1e1e1e; color:#d4d4d4; border:1px solid #333; border-radius:4px; padding:6px 10px; font:inherit; }
  .docSearch:focus { outline:none; border-color:#3a6ea5; }
  .docSearchInfo { color:#9a9a9a; font-size:12px; min-width:64px; text-align:right; white-space:nowrap; }
  .docMain { display:flex; flex:1; min-height:0; }
  .docNav { width:220px; flex:none; overflow:auto; border-right:1px solid #333; padding:8px 8px 24px; }
  .docNavTitle { color:#777; font-size:11px; letter-spacing:.4px; margin:14px 8px 4px; }
  .docNavGroup:first-child .docNavTitle { margin-top:4px; }
  .docNavItem { color:#c0c0c0; font-size:13px; line-height:1.35; padding:4px 8px; border-radius:4px; border-left:2px solid transparent; cursor:pointer; }
  .docNavItem:hover { background:#252526; color:#e0e0e0; }
  .docNavItem.active { color:#4ec9b0; background:#1f2a28; border-left-color:#4ec9b0; }
  .docBody { flex:1; overflow:auto; position:relative; padding:16px 28px 60px; }
  .docSec { margin-bottom:30px; }
  .docHide { display:none !important; }
  .docBody h4 { color:#dcdcaa; margin:14px 0 6px; font-size:12.5px; }
  .docTableWrap { overflow-x:auto; margin:8px 0; }
  .docBody table { border-collapse:collapse; font-size:12px; }
  .docBody th, .docBody td { border:1px solid #333; padding:5px 9px; vertical-align:top; color:#c0c0c0; text-align:left; }
  .docBody th { background:#252526; color:#d4d4d4; white-space:nowrap; }
  .docSteps { margin:6px 0 6px 20px; color:#c0c0c0; }
  .docSteps li { margin:8px 0; }
  .docPre { position:relative; margin:8px 0; }
  .docPre pre { margin:0; background:#181818; border:1px solid #333; border-radius:6px; padding:10px 12px; color:#9ecb9e; overflow-x:auto; font:12px/1.5 Consolas,monospace; }
  .docCopyBtn { position:absolute; top:6px; right:6px; padding:2px 8px; font-size:11px; opacity:.75; }
  .docCopyBtn:hover { opacity:1; }
  .docNoResult { color:#9a9a9a; padding:24px 0; display:none; }
  .doc h3 { color:#4ec9b0; margin:14px 0 4px; font-size:13px; }
  .docSec h3 { margin-top:2px; font-size:14px; }
  .doc p { margin:4px 0; color:#c0c0c0; }
  .doc ul { margin:4px 0 4px 18px; color:#c0c0c0; }
  .doc li { margin:2px 0; }
  .doc code { background:#2a2a2a; padding:1px 5px; border-radius:3px; color:#9cdcfe; }
  .doc b { color:#d4d4d4; }
  @media (max-width:900px) { .docNav { width:180px; } .docBody { padding:14px 18px 48px; } }
  @media (max-width:640px) { .docFrame { height:96vh; } .docMain { flex-direction:column; } .docNav { width:auto; max-height:26vh; border-right:0; border-bottom:1px solid #333; } }
  /* 删除配置弹窗 */
  #cfgDelModal { position:fixed; inset:0; background:rgba(0,0,0,.55); z-index:100; display:none; align-items:center; justify-content:center; }
  #cfgDelModal .box { background:#252526; border:1px solid #444; border-radius:6px; padding:16px; min-width:320px; }
  #cfgDelModal .t { margin-bottom:10px; color:#d4d4d4; }
  #cfgDelModal select { width:100%; background:#1e1e1e; color:#d4d4d4; border:1px solid #444; border-radius:3px; padding:5px; font:inherit; }
  #cfgDelModal .err { color:#c75; margin-top:8px; min-height:16px; }
  #cfgDelModal .btns { margin-top:14px; display:flex; gap:8px; justify-content:flex-end; }
</style>
</head>
<body>
<div id="bar">
  <div class="tab active" data-tab="status">状态</div>
  <div class="tab" data-tab="logs">日志</div>
  <div class="tab" data-tab="config">配置</div>
  <span id="hint">关闭此标签页即隐藏 · 代理继续运行</span>
  <button id="docBtn" class="ghost" style="align-self:center;margin:0 12px" onclick="docOpen()">文档</button>
  <span id="verBox" style="align-self:center;margin-left:auto;padding-right:12px;color:#666;font-size:12px">__VERSION__</span>
  <span id="langBox" style="align-self:center;padding-right:12px;color:#9a9a9a">语言
    <select id="langSel" style="background:#1e1e1e;color:#d4d4d4;border:1px solid #333;border-radius:3px;padding:2px 4px;font:inherit">
      <option value="zh">中文</option>
      <option value="en">English</option>
    </select>
  </span>
</div>

<div class="pane active" id="pane-status">
  <div style="margin-bottom:8px"><span class="st idle" id="status">⚪ 连接中</span> <span id="curCfg" style="margin-left:10px;color:#888"></span>
    <button id="resetStatsBtn" class="ghost" style="margin-left:16px;padding:2px 8px">清空统计</button>
    <label style="margin-left:12px;color:#9a9a9a">保留完成流: <input id="finishedCapInput" type="number" min="0" max="200" value="10" style="width:50px;background:#1e1e1e;color:#d4d4d4;border:1px solid #333;border-radius:3px;padding:2px 4px;font:inherit"></label>
    <button id="finishedCapBtn" class="ghost" style="padding:2px 8px">设置</button>
    <span id="finishedCapMsg"></span>
    <label style="margin-left:12px;color:#9a9a9a;font-weight:normal" title="开启后新开始的请求记录完整请求体与输出（不设 256KB 上限），流查看器出现「下载请求体/下载输出」按钮；关闭立即清空已存的完整副本，仅能浏览截断内容"><input type="checkbox" id="fullStoreChk"> 储存完整结构体</label>
    <button id="heatBtn" class="ghost" style="margin-left:12px;padding:2px 8px" onclick="document.getElementById('heatModalBody').innerHTML=heatHTML();document.getElementById('heatModal').style.display='flex'">活跃度</button>
  </div>
  <div id="hijackWarn" style="display:none;background:#3d1414;border:1px solid #c0392b;border-radius:4px;padding:8px 10px;margin-bottom:8px;color:#f0b9b0">⚠ 检测到系统代理已开启，但 NO_PROXY 环境变量未排除回环地址——遵循系统代理的客户端（如 Codex）会把发往 127.0.0.1 的请求交给代理服务器，表现为 503 且本代理收不到任何请求。修复：到「配置」页底部重跑一次 Codex 一键命令（脚本会自动修好），然后重启 Codex。</div>
  <div class="cards" id="cards"></div>
  <div>在途流 <label style="margin-left:8px;color:#9a9a9a;font-weight:normal"><input type="checkbox" id="autoTrackChk" checked>自动跟踪最新</label> <label style="color:#9a9a9a;font-weight:normal">最多显示 <input type="number" id="maxFlightsInput" min="1" max="20" value="3" style="width:40px;background:#1e1e1e;color:#d4d4d4;border:1px solid #333;border-radius:3px;padding:2px 4px;font:inherit"> 个</label></div>
  <table id="flights"><thead><tr><th></th><th>#</th><th>model</th><th>API<span class="thq" title="协议来源：橙 [Anthropic] = Anthropic 口原生流量、紫 [translate] = Responses 口翻译成 Anthropic。名后 [值] = 实际发给上游的思考配置最短形态，其颜色 = 词汇口径（思考是针对上游的：上游收到的都是 Anthropic 格式）——橙 = Anthropic thinking（直连原样或翻译映射后）。值：关 = thinking 关或 effort none/off；开 N = enabled+budget_tokens N；adaptive = 自适应无档；low/high/max 等档位词 = adaptive 的 effort；无 [值] = 请求体未带思考字段">?</span></th><th>token 入/出<span class="thq" title="该流的总输入 / 输出 tokens（输入 = input + 缓存读 + 缓存写）。在途时显示头部/尾部已到达的数值，完成后才准确；未收到的一侧显示 -">?</span></th><th>字节</th><th>状态</th></tr></thead><tbody></tbody></table>
  <div id="flightViewWrap" style="display:none;margin-top:8px">
    <div>流 #<span id="flightViewId"></span> <span id="flightViewKind">输出</span> <button id="flightViewWhatBtn" class="ghost" style="display:none">看请求体</button> <button id="flightViewSideBtn" class="ghost" style="display:none">链路:代理<->上游</button><span id="flightViewSideNote" style="color:#d7ba7d"></span> <button id="flightViewRawBtn" class="ghost">显示原始</button> <button id="flightViewDlBtn" class="ghost" style="display:none">下载请求体</button> <button id="flightViewDlOutBtn" class="ghost" style="display:none">下载输出</button> <button id="flightViewTreeBtn" class="ghost" style="display:none">交互式JSON</button> <button id="flightViewClose" class="ghost">关闭</button></div>
    <div id="flightView" style="max-height:300px;overflow:auto;background:#1e1e1e;border:1px solid #333;padding:8px"></div>
  </div>
  <div style="margin-top:10px">最近完成的流</div>
  <table id="finishedFlights"><thead><tr><th>#</th><th>model</th><th>API<span class="thq" title="协议来源：橙 [Anthropic] = Anthropic 口原生流量、紫 [translate] = Responses 口翻译成 Anthropic。名后 [值] = 实际发给上游的思考配置最短形态，其颜色 = 词汇口径（思考是针对上游的：上游收到的都是 Anthropic 格式）——橙 = Anthropic thinking（直连原样或翻译映射后）。值：关 = thinking 关或 effort none/off；开 N = enabled+budget_tokens N；adaptive = 自适应无档；low/high/max 等档位词 = adaptive 的 effort；无 [值] = 请求体未带思考字段">?</span></th><th>token 入/出<span class="thq" title="该流的总输入 / 输出 tokens（输入 = input + 缓存读 + 缓存写），完成后才准确；未收到的一侧显示 -（如被下游取消的流 [107k/-]）。鼠标悬停单元格显示该流字节数">?</span></th><th>总时间</th><th>状态码</th><th>缓存命中</th><th>缓存年龄<span class="thq" title="该会话+路由最近一次缓存写入距现在的时长（m:ss 递增）。显示方括号且数字冻结（如 [4:32]）时：同会话同路由有一条正在进行的流刚刷新了缓存，方括号内是刷新那一刻的年龄；该流完成后恢复递增（新锚）。上游缓存真实存活期是动态的——点击「缓存命中」卡片，弹窗内有各上游（按 URL+模型归类）实测的缓存存活时间可对照">?</span></th><th>首字</th><th>tok/s</th><th>结束</th></tr></thead><tbody></tbody></table>
</div>

<div class="pane" id="pane-logs">
  <div style="margin-bottom:6px"><span id="count">0</span> 行 · 滚轮翻历史，自动滚到底 <span style="color:#9a9a9a">（内存仅留最近 500 行，新的覆盖旧的；不会随时间堆积。落盘日志另看 log_file）</span></div>
  <pre id="log"></pre>
</div>
<button id="clearLogsBtn" class="ghost" style="position:fixed;right:16px;bottom:16px;z-index:20;display:none">清空日志</button>

<div id="cacheTip" style="display:none;position:fixed;z-index:35;background:#1a1a1a;border:1px solid #555;border-radius:6px;padding:8px 10px;max-width:540px;box-shadow:0 4px 12px rgba(0,0,0,0.5);font-size:12px"></div>
<div id="cacheModal" style="display:none;position:fixed;inset:0;background:rgba(0,0,0,0.6);z-index:40;align-items:center;justify-content:center" onclick="if(event.target===this)this.style.display='none'">
  <div style="background:#1a1a1a;border:1px solid #555;border-radius:8px;padding:20px 24px;max-width:720px;max-height:80vh;overflow:auto;position:relative">
    <button class="ghost" style="position:absolute;top:10px;right:12px" onclick="document.getElementById('cacheModal').style.display='none'">关闭</button>
    <div style="font-size:15px;margin-bottom:12px;color:#d4d4d4">缓存命中明细（按真实上游模型） <span id="cacheModalTotal" style="color:#888;font-size:12px;margin-left:8px"></span></div>
    <div id="cacheModalBody"></div>
  </div>
</div>
<div id="heatModal" style="display:none;position:fixed;inset:0;background:rgba(0,0,0,0.6);z-index:40;align-items:center;justify-content:center" onclick="if(event.target===this)this.style.display='none'">
  <div style="background:#1a1a1a;border:1px solid #555;border-radius:8px;padding:20px 24px;max-width:96vw;position:relative">
    <button class="ghost" style="position:absolute;top:10px;right:12px" onclick="document.getElementById('heatModal').style.display='none'">关闭</button>
    <div style="font-size:15px;margin-bottom:8px;color:#d4d4d4;padding-right:70px">活跃度</div>
    <div id="heatMetricRow" style="margin-bottom:12px">
      <button id="heatM0" class="ghost" style="padding:1px 8px;font-size:12px" onclick="heatSetMetric(0)">请求次数</button>
      <button id="heatM1" class="ghost" style="margin-left:4px;padding:1px 8px;font-size:12px" onclick="heatSetMetric(1)">输入+输出</button>
      <button id="heatM2" class="ghost" style="margin-left:4px;padding:1px 8px;font-size:12px;border-color:#39d353" onclick="heatSetMetric(2)">输出</button>
    </div>
    <div id="heatModalBody"></div>
  </div>
</div>
<div id="retryTip" style="display:none;position:fixed;z-index:35;background:#1a1a1a;border:1px solid #555;border-radius:6px;padding:8px 10px;max-width:540px;box-shadow:0 4px 12px rgba(0,0,0,0.5);font-size:12px"></div>
<div id="retryModal" style="display:none;position:fixed;inset:0;background:rgba(0,0,0,0.6);z-index:40;align-items:center;justify-content:center" onclick="if(event.target===this)this.style.display='none'">
  <div style="background:#1a1a1a;border:1px solid #555;border-radius:8px;padding:20px 24px;max-width:720px;max-height:80vh;overflow:auto;position:relative">
    <button class="ghost" style="position:absolute;top:10px;right:12px" onclick="document.getElementById('retryModal').style.display='none'">关闭</button>
    <div style="font-size:15px;margin-bottom:12px;color:#d4d4d4">重试明细（按路由目标模型） <span id="retryModalTotal" style="color:#888;font-size:12px;margin-left:8px"></span></div>
    <div id="retryModalBody"></div>
  </div>
</div>
<div id="clsTip" style="display:none;position:fixed;z-index:35;background:#1a1a1a;border:1px solid #555;border-radius:6px;padding:8px 10px;max-width:540px;box-shadow:0 4px 12px rgba(0,0,0,0.5);font-size:12px"></div>
<div id="clsModal" style="display:none;position:fixed;inset:0;background:rgba(0,0,0,0.6);z-index:40;align-items:center;justify-content:center" onclick="if(event.target===this)this.style.display='none'">
  <div style="background:#1a1a1a;border:1px solid #555;border-radius:8px;padding:20px 24px;max-width:720px;max-height:80vh;overflow:auto;position:relative">
    <button class="ghost" style="position:absolute;top:10px;right:12px" onclick="document.getElementById('clsModal').style.display='none'">关闭</button>
    <div style="font-size:15px;margin-bottom:12px;color:#d4d4d4">分类器明细 <span id="clsModalTotal" style="color:#888;font-size:12px;margin-left:8px"></span></div>
    <div id="clsModalBody"></div>
  </div>
</div>

<div id="docModal" onclick="if(event.target===this)docClose()">
  <div class="docFrame">
    __DOC_BODY__</div>
</div>

<div class="pane" id="pane-config">
  <div id="cfgPath"></div>
  <div style="margin-bottom:8px">
    配置文件: <select id="cfgSelect"></select>
    <button id="cfgRefreshBtn" class="ghost">刷新列表</button>
    <button id="cfgNewBtn" class="ghost">新建</button>
    <button id="cfgRenameBtn" class="ghost">重命名</button>
    <button id="cfgDelBtn" class="ghost">删除</button>
    <span id="cfgSwitchMsg"></span>
  </div>
  <textarea id="cfg" spellcheck="false"></textarea>
  <div style="margin-top:8px">
    <button id="saveBtn">保存并重载</button>
    <button id="reloadBtn" class="ghost">仅重载（不改动文件）</button>
    <button id="lazyPreviewBtn" class="ghost">预览 lazyApply 生效后 JSON</button>
    <span id="cfgMsg"></span>
  </div>
  <div id="lazyPreviewBox" style="display:none;margin-top:8px">
    <div class="dim">只读预览，不写入文件。展示每个路由类结构在补上 lazyApply 默认值后的样子；局部显式值优先，noLazyApply:true 的对象保持不变。search_fallback 只显示自身值，命中路由的 claudeSearch* 覆盖在运行时叠加。</div>
    <textarea id="lazyPreview" spellcheck="false" readonly style="width:100%;background:#1e1e1e;color:#d4d4d4;border:1px solid #333;border-radius:4px;padding:8px;font:inherit;min-height:32vh;margin-top:6px"></textarea>
    <div style="margin-top:8px">
      <button id="lazyPreviewCopyBtn" class="ghost">复制到编辑框</button>
      <button id="lazyPreviewCloseBtn" class="ghost">关闭预览</button>
      <span id="lazyPreviewMsg"></span>
    </div>
  </div>
  <div id="codexBox">
    <div class="t">Codex 一键配置 —— 下面两行命令按上方编辑框实时生成（地址取自 responses_listen，模型目录取自 routes 的 pattern），在对应终端粘贴回车即运行</div>
    <div id="codexEnable" style="display:none">
      当前配置没有 responses_listen，Responses API 监听口未启用。要为本配置增加 Responses API 功能吗？
      <button id="codexEnableBtn">是，添加并保存</button>
      <span id="codexEnableMsg"></span>
      <div class="dim">会在上方 JSON 的 "listen" 行后加一行 "responses_listen": "127.0.0.1:8081"（端口可改）并立即保存；监听口随保存即时启动，不用重启代理。</div>
    </div>
    <div id="codexGen">
    <div style="margin-bottom:6px">
      默认模型: <select id="codexModel">
        <option value="gpt-5-codex">gpt-5-codex（gpt-5*）</option>
        <option value="claude-fable-5">claude-fable-5</option>
        <option value="__custom__">自定义…</option>
        <option value="__restore__">（还原默认 Codex 配置）</option>
      </select>
      <input id="codexModelCustom" placeholder="自定义模型名" style="display:none">
      &nbsp;上下文窗口: <input id="codexCtx" type="number" value="262144" min="4096" max="2000000" style="width:84px" title="写进模型目录每个条目的上下文窗口（tokens）">
      &nbsp;压缩阈值%: <input id="codexCompact" type="number" value="95" min="1" max="99" style="width:56px" title="上下文用到该百分比时 Codex 自动压缩（95 = 用到 95% 触发）">
      <span id="codexMsg"></span>
    </div>
    <div class="cmdRow"><span class="os">Windows（PowerShell）</span><code id="codexCmdWin"></code><button id="codexCopyWin" class="ghost">复制</button></div>
    <div class="cmdRow"><span class="os">macOS / Linux（终端）</span><code id="codexCmdSh"></code><button id="codexCopySh" class="ghost">复制</button></div>
    <div class="dim">命令从本代理拉取按当前配置烤好的脚本并直接执行（免交互、免官方登录、token 占位——真实 key 由上面路由的 api 注入）；脚本先备份再改写 ~/.codex/config.toml 并写模型目录，下拉选「（还原默认 Codex 配置）」得到的命令可完全撤销。下拉列出 routes 每个 pattern 的一个代表名（route.model 能命中 pattern 时用真名，否则用去 * 的 pattern；纯 * 兜底路由固定叫 Fallback——它接住任意模型名，显示某个真实 model 名会误导）：这些名字全部写进 Codex 的 /model 菜单，选中项为默认模型；pattern 不允许全字叫 Fallback 或 fast_route（保留名，撞名的路由不生效也不进菜单）；配了 fast_route 且带 model 时菜单追加 fast_route 条目，选中即走 fast 通道（等效请求带 speed:"fast"）。上下文窗口与压缩阈值写进目录的每个条目（Codex 用到该百分比时自动压缩上下文，顶层 model_context_window 等覆盖键会被脚本清掉以免压过目录声明）。改了 responses_listen 点「保存并重载」后监听口即按新地址生效，命令跟着变。</div>
    </div>
  </div>
</div>

<div id="cfgDelModal">
  <div class="box">
    <div class="t">选择要删除的配置（当前生效的配置不可删除）：</div>
    <select id="cfgDelSelect"></select>
    <div class="err" id="cfgDelMsg"></div>
    <div class="btns">
      <button id="cfgDelCancelBtn" class="ghost">取消</button>
      <button id="cfgDelConfirmBtn" class="danger">确认删除</button>
    </div>
  </div>
</div>

<script>
const logEl = document.getElementById('log');
const stEl  = document.getElementById('status');
const cardsEl = document.getElementById('cards');
const flightsBody = document.querySelector('#flights tbody');
let stick = true;
let cfgLoaded = false;
let cfgPollTimer = null; // 配置页激活时定时轮询配置文件列表，切走清掉
let lastCfgFiles = null; // 上次配置列表签名，未变化跳过重绘避免下拉闪烁
let selectedFlight = 0; // 当前查看的在途流 id（0=未查看）
let autoTrack = true; // 勾选时 poll 自动跟踪最新在途流（id 最大=最新），不勾选则用户手选
let maxFlights = 3; // 自动跟踪多流模式下最多并排显示多少个在途流（最新 N 个），防止太多太细
let flightViewRaw = false; // 查看区显示模式：false=解析文本，true=原始 SSE
let flightEnded = false; // 选中的流是否已结束（结束后停止拉取，保留最后内容）
let flightViewWhat = 'resp'; // 手选单流查看内容四态：'req'=请求体，'resp'=输出[渲染]（SSE 解析成可读文本，默认），'raw'=输出[流式原始]（SSE 原文），'asm'=输出[非流式]（事件流组装成最终 JSON）（仅手选单流可切，「看…」按钮按 请求体→输出[渲染]→输出[流式原始]→输出[非流式] 循环）
let lastReqRaw = ''; // 当前选中流当前链路侧的请求体缓存（切到 req 或切链路侧时拉一次；请求体静态不变）
let flightViewSide = 'up'; // 查看链路侧：'up'=代理↔上游（默认观测面），'down'=下游↔代理；请求体/输出共用，点「链路」按钮切换
let fullStoreOn = false; // 「储存完整结构体」开关（以服务端为准）：开=可下载完整请求体/输出
let flightFlags = {}; // 流 id -> {rt: 请求体被截断, hf: 仍持有完整输出}，poll 时在途/完成两表下发，用于置灰下载按钮
let flightViewTree = false; // 单流交互式 JSON 树视图：true=查看区显示可折叠树（点击时的静态快照，poll 不刷新）
let flightViewTreeManual = false; // 用户本次选择内手动点过「交互式JSON/退出交互」：钉住其选择，自动成树不再介入（重选流时复位）
let lastRaw = ''; // 单流模式：最后一次拉到的 raw SSE（模式切换重渲染 + 流结束后保留）
let autoRaws = {}; // 自动跟踪多流模式：每个在途流 id -> 最近 raw（模式切换重渲染用）

// 标签切换
document.querySelectorAll('.tab').forEach(t => {
  t.onclick = () => {
    document.querySelectorAll('.tab').forEach(x => x.classList.remove('active'));
    document.querySelectorAll('.pane').forEach(x => x.classList.remove('active'));
    t.classList.add('active');
    document.getElementById('pane-' + t.dataset.tab).classList.add('active');
    document.getElementById('clearLogsBtn').style.display = (t.dataset.tab === 'logs') ? 'block' : 'none';
    if (t.dataset.tab === 'logs') stick = true, window.scrollTo(0, document.body.scrollHeight);
    if (cfgPollTimer){ clearInterval(cfgPollTimer); cfgPollTimer=null; }
    if (t.dataset.tab === 'config'){
      if(!cfgLoaded) loadConfig(); else loadConfigList();
      cfgPollTimer = setInterval(loadConfigList, 3000);
      genCodexCmds();
    }
  };
});

window.addEventListener('scroll', () => {
  stick = (window.innerHeight + window.scrollY) >= (document.body.scrollHeight - 30);
});

// ---- Usage docs browser ----
// The doc body arrives whole via the page's doc placeholder (logViewerDocZH / logViewerDocEN in doc.go). The nav is
// generated from the section elements, so the two languages never drift. Any UI text in this block comes
// from the body's data attributes, keeping the shared script language-neutral.
let docQuery = '';
// docGo sets this while a programmatic scroll settles; the spy must not override the clicked item (at the bottom of
// the body the "clamped to the last section" rule would otherwise steal the highlight from the section just picked).
let docNavLock = 0;
function docOpen(){
  document.getElementById('docModal').style.display = 'flex';
  docInit();
  const s = document.getElementById('docSearch');
  if(s && !docQuery) s.focus();
  docSpy();
}
function docClose(){
  document.getElementById('docModal').style.display = 'none';
}
function docInit(){
  const nav = document.getElementById('docNav');
  if(nav.dataset.built) return;
  nav.dataset.built = '1';
  const body = document.getElementById('docBody');
  let groupName = null, groupBox = null;
  body.querySelectorAll('.docSec').forEach(sec => {
    sec.dataset.search = sec.textContent.toLowerCase(); // cached before the copy buttons join the text
    const name = sec.dataset.group || '';
    if(name !== groupName){
      groupName = name;
      groupBox = document.createElement('div');
      groupBox.className = 'docNavGroup';
      const title = document.createElement('div');
      title.className = 'docNavTitle';
      title.textContent = name;
      groupBox.appendChild(title);
      nav.appendChild(groupBox);
    }
    const item = document.createElement('div');
    item.className = 'docNavItem';
    item.dataset.target = sec.id;
    item.textContent = sec.querySelector('h3').textContent;
    item.title = item.textContent;
    item.onclick = () => docGo(sec.id);
    groupBox.appendChild(item);
  });
  body.querySelectorAll('pre[data-copy]').forEach(pre => {
    const wrap = document.createElement('div');
    wrap.className = 'docPre';
    pre.parentNode.insertBefore(wrap, pre);
    wrap.appendChild(pre);
    const btn = document.createElement('button');
    btn.type = 'button';
    btn.className = 'ghost docCopyBtn';
    btn.dataset.copy = pre.dataset.copy;
    btn.dataset.done = pre.dataset.done;
    btn.textContent = pre.dataset.copy;
    btn.onclick = () => docCopy(pre, btn);
    wrap.appendChild(btn);
  });
  body.addEventListener('scroll', docSpy, {passive:true});
  document.addEventListener('keydown', docKey);
}
function docGo(id, silent){
  const sec = document.getElementById(id);
  const body = document.getElementById('docBody');
  if(!sec || !body) return;
  docNavLock = Date.now() + 900;
  body.scrollTo({top: Math.max(0, sec.offsetTop - 8), behavior: silent ? 'auto' : 'smooth'});
  docSetActive(id);
}
function docSetActive(id){
  document.querySelectorAll('#docNav .docNavItem').forEach(it => {
    it.classList.toggle('active', it.dataset.target === id);
  });
}
function docSpy(){
  if(docQuery || Date.now() < docNavLock) return;
  const body = document.getElementById('docBody');
  if(!body) return;
  const secs = body.querySelectorAll('.docSec');
  let cur = '';
  const top = body.scrollTop + 60;
  secs.forEach(sec => { if(sec.offsetTop <= top) cur = sec.id; });
  if(body.scrollTop + body.clientHeight >= body.scrollHeight - 4 && secs.length) cur = secs[secs.length-1].id;
  if(cur) docSetActive(cur);
}
function docFilter(q){
  docQuery = String(q || '').trim().toLowerCase();
  const body = document.getElementById('docBody');
  const info = document.getElementById('docSearchInfo');
  const none = document.getElementById('docNoResult');
  if(!body) return;
  let count = 0;
  body.querySelectorAll('.docSec').forEach(sec => {
    const hit = !docQuery || sec.dataset.search.includes(docQuery);
    sec.classList.toggle('docHide', !hit);
    if(hit) count++;
  });
  document.querySelectorAll('#docNav .docNavItem').forEach(it => {
    const sec = document.getElementById(it.dataset.target);
    it.classList.toggle('docHide', !!sec && sec.classList.contains('docHide'));
  });
  document.querySelectorAll('#docNav .docNavGroup').forEach(g => {
    const any = Array.from(g.querySelectorAll('.docNavItem')).some(it => !it.classList.contains('docHide'));
    g.classList.toggle('docHide', !any);
  });
  if(info) info.textContent = docQuery ? count + ' ' + info.dataset.label : '';
  if(none) none.style.display = (docQuery && !count) ? 'block' : 'none';
  if(docQuery) body.scrollTo({top: 0, behavior: 'auto'});
  else docSpy();
}
function docSearchKey(ev){
  if(ev.key === 'Escape'){
    ev.stopPropagation();
    if(ev.target.value){ ev.target.value = ''; docFilter(''); }
    else docClose();
  } else if(ev.key === 'Enter'){
    const first = document.querySelector('#docBody .docSec:not(.docHide)');
    if(first) docGo(first.id);
  }
}
function docKey(ev){
  if(ev.key !== 'Escape') return;
  const m = document.getElementById('docModal');
  if(m && m.style.display === 'flex') docClose();
}
function docCopy(pre, btn){
  const flash = txt => {
    btn.textContent = txt;
    setTimeout(() => { btn.textContent = btn.dataset.copy; }, 1200);
  };
  const done = () => flash(btn.dataset.done);
  const failed = () => flash('✗');
  const fallback = () => {
    const ta = document.createElement('textarea');
    ta.value = pre.textContent;
    ta.style.position = 'fixed';
    ta.style.opacity = '0';
    document.body.appendChild(ta);
    ta.select();
    let ok = false;
    try { ok = document.execCommand('copy'); } catch (e) { ok = false; } finally { ta.remove(); }
    if(ok) done(); else failed();
  };
  if(navigator.clipboard && navigator.clipboard.writeText){
    navigator.clipboard.writeText(pre.textContent).then(done, fallback);
  } else fallback();
}
function docOpenConfig(){
  docClose();
  const tab = document.querySelector('.tab[data-tab="config"]');
  if(tab) tab.click();
}
function lastTag(f){ return (f && f.last) ? '<span title="Last #'+f.last+'">&lt;-'+f.last+'</span>' : ''; }
function fmtBytes(n){
  if(n<1024) return n+'B';
  if(n<1048576) return (n/1024).toFixed(1)+'KB';
  return (n/1048576).toFixed(2)+'MB';
}
// token 入/出列：[总输入/输出] 例如 [107k/4]。未收到的一侧显示 -（如被取消的流 [107k/-]），
// 两侧都未知时整格显示 -。传入 bytes（最近完成流）时单元格带 title，悬停显示该流字节数——
// 该表用它替换了字节列；在途流字节数就在旁边一列，不重复挂提示。
function tokensCell(t, bytes){
  var title = bytes===undefined ? '' : ' title="'+fmtBytes(bytes)+'"';
  if(!t || t==='-') return '<td'+title+'>-</td>';
  return '<td'+title+'>['+esc(t)+']</td>';
}
// model 列单元格：有 searchPrompt 时整个 td 加 title，hover 显示系统原生提示词（宽度自适应、无滚动条）。
// routeReason 非 0（透传）时前置 [标签]，一眼看出是什么原因路由的（如 [Fast]claude-sonnet-5 -> glm-5.2）。
// countTokens 为 true（/v1/messages/count_tokens 探针）时前置灰色 [count_tokens]：
// 这类流原始内容只有 {"input_tokens":N}，没标记看着像空响应。
// API 来源（[Anthropic]/[Response]/[translate]）在独立 API 列显示，见 apiCell，不在本列。
function modelCell(f){
  var model = f.model;
  var tag = routeTag(f.routeReason);
  if(tag) model = tag + model;
  // [Compact] marks a client context-compaction request; when the top-level compactEffort pins a level the badge
  // carries it ([Compact-max], wire form so mid reads medium) and the route [level] tag is suppressed on that flight.
  // Finished rows append the result ([170k->20k, 88%]) after the model name; the in-flight JSON carries no
  // compactSave field, so only the badge shows there.
  if(f.compaction) model = '<span style="color:#ce9178">[Compact'+(f.compactEffort?'-'+esc(f.compactEffort):'')+']</span>' + model;
  // 独立搜索子代理徽标：所有标记命中挂 [Search]，摘要注入成功升级为 [SearchSummary-档位]（青绿 #4ec9b0，与路由标签淡蓝区分）
  if(f.searchMarker) model = (f.searchLevel ? '<span style="color:#4ec9b0">[SearchSummary-'+esc(f.searchLevel)+']</span>' : '<span style="color:#4ec9b0">[Search]</span>') + model;
  if(f.countTokens) model = '<span style="color:#9d9d9d">[count_tokens]</span>' + model;
  if(f.compactSave) model = model + '<span style="color:#ce9178">['+esc(f.compactSave)+']</span>';
  // 工具调用标签（"[Read*1][Edit*3]"）：跟在 model 链后，金色区分；来自服务端 toolCallsTag 的已格式化串，转义后插入
  // （注意：本函数内局部变量不能叫 esc——var 声明提升会遮蔽全局 esc()，上面这行就会拿不到函数）
  if(f.tools) model = model + '<span style="color:#d7ba7d">' + esc(f.tools) + '</span>';
  // 剥块红标 [剥N]：该流剥掉的回放搜索块总数（对话水位+400 兜底）；只在在途流挂本列，
  // 完成流改挂缓存命中列（见行模板）——同一红标两处只出现一处，避免重复计数观感
  if(f.stripped > 0) model = model + '<span style="color:#f48771">[剥' + f.stripped + ']</span>';
  if(!f.searchPrompt) return '<td>'+model+'</td>';
  var escP = String(f.searchPrompt).replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;').replace(/"/g,'&quot;');
  return '<td title="'+escP+'">'+model+'</td>';
}
// API 列单元格：协议来源 + 思考值合一格（省一列）——
// 名部分：空 = Anthropic 口原生流量（Claude Code 等，橙色 [Anthropic]，#d97757 = Claude 珊瑚橙）；
// 'responses-raw' = Responses 口原生透传（路由配了 url_response_api，绿色 [Response]，#2bbf8a = ChatGPT 绿 #10a37f 的提亮版）；
// 'responses' = Responses 口翻译成 Anthropic 走主管线（紫色 [translate]，沿用旧 model 列前缀的颜色）。
// 思考值（实际发给上游的思考配置最短形态：关 / 开 N / adaptive / high·max 等档位词）紧跟名后 [值]，
// 颜色 = 词汇口径。思考是针对上游的——非透传流上游收到的都是 Anthropic 格式（直连原样、翻译映射后），
// 故只有两色：橙 #d97757=Anthropic thinking 口径、绿 #2bbf8a=Responses reasoning 口径（仅透传）。
// 所以紫色 [translate] 名后跟的是橙值。关也带色：绿关 vs 橙关能看出是哪侧关的。无思考字段时裸名无 [值]。
function apiCell(translated, think, thinkFrom, thinkTo){
  var name, color;
  if(translated === 'responses-raw'){ name = '[Response]'; color = '#2bbf8a'; }
  else if(translated){ name = '[translate]'; color = '#c586c0'; }
  else { name = '[Anthropic]'; color = '#d97757'; }
  var html = '<span style="color:'+color+'">'+name+'</span>';
  if(thinkTo){
    // Body-side thinking rewrite badge [from->to] (route effort pin / compactEffort pin on a compaction request / search sub-agent rewrite; empty from = the request had no thinking field, shown as auto), teal; replaces the normal [think] badge when present
    html += '<span style="color:#4ec9b0">['+esc((thinkFrom||'auto')+'->'+thinkTo)+']</span>';
  } else if(think === 'off->low'){
    // convertOff2Low 升级流双色徽标：off 用翻译紫（下游口径：它发的是关思考），
    // low 用 Anthropic 橙（上游口径：实际发给上游的 low 思考）。
    html += '<span style="color:#c586c0">[off</span><span style="color:#9a9a9a">-&gt;</span><span style="color:#d97757">low]</span>';
  } else if(think){
    var tc = translated==='responses-raw' ? '#2bbf8a' : '#d97757';
    html += '<span style="color:'+tc+'">['+esc(think)+']</span>';
  }
  return '<td>'+html+'</td>';
}
// 路由原因 -> model 列前缀标签（与 main.go route* 枚举对齐：0透传 1pattern 2分类器 3fast 4多模态 5搜索）。
// 透传不加前缀；标签淡蓝色与 model 链区分。
function routeTag(r){
  var tags = ['','[Pattern]','[分类器]','[Fast]','[多模态]','[搜索]'];
  if(!tags[r]) return '';
  return '<span style="color:#7ec8e3">'+tags[r]+'</span>';
}
// 在途流状态灯：转发中(绿)/等待首字节(黄)/请求阶段(白)，用 emoji 与顶部连接状态灯同等大小。
function flightDot(f){
  if(f.stage===3) return '🟢';
  if(f.stage>=1) return '🟡';
  return '⚪';
}
// 状态灯旁的持续时长：当前灯色已点亮的秒数（服务端 stageMs，随 500ms 轮询走动）。
function fmtStageDur(ms){ return (ms/1000).toFixed(1)+'s'; }
// 黄灯旁的当前尝试计时，夹在状态灯与黄灯总时长之间：[尝试N: Xs]=本次尝试已等首字节的时长
// （每次尝试重新起算）；attemptMs=-1 表示重试退避中（下一次尝试未发出），显示 [退避中]；
// 非尝试阶段（白灯请求/黄灯路由/绿灯转发）不显示。
function attemptTag(f){
  if(f.stage!==2) return '';
  if(f.attemptMs==null || f.attemptMs<0) return ' [退避中]';
  return ' [尝试'+(f.attempt||1)+': '+fmtStageDur(f.attemptMs)+']';
}
// 在途流「状态」列：转发阶段显示状态码，其余阶段显示中文进度；
// 尝试及以后附加路由原因（如「尝试1·搜索」「200·透传」）。
// stage: 0=请求 1=路由 2=尝试N 3=转发(显示状态码)
// 转义 & < >，innerHTML 安全插入用户内容（tool input/搜索结果可能含 <>）。
function esc(s){
  return String(s).replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;');
}
// 解析 SSE 原文为 HTML：按到达顺序提取 text/thinking/tool_use/server_tool_use/web_search_tool_result，
// 每类独立成块渲染（tool 各起新行），"啥都渲染"。后端只 tee raw，解析放前端避免影响透传热路径。
function parseSSEHTML(raw){
  var blocks = [];          // 按顺序: {type:'text'|'think'|'tool'|'search', ...}
  var idx = {};             // content_block index -> block（input_json_delta 回填用）
  var rIdx = {};            // Responses 透传流：output_item id -> block（arguments delta / done 回填用）
  var curText = null, curThink = null;
  function textBlock(){ if(!curText){ curText = {type:'text', text:''}; blocks.push(curText); } return curText; }
  function thinkBlock(){ if(!curThink){ curThink = {type:'think', text:''}; blocks.push(curThink); } return curThink; }
  // Responses output 数组 → blocks（response.completed 兜底与非流式 response 对象共用）：
  // message→text、reasoning→think、function_call/custom_tool_call→tool、web_search_call→search。
  function renderResponsesOutput(output){
    output.forEach(function(it){
      if(!it || !it.type) return;
      if(it.type === 'message' && Array.isArray(it.content)){
        it.content.forEach(function(c){
          if(c && c.type === 'output_text' && c.text) blocks.push({type:'text', text:c.text});
        });
      } else if(it.type === 'reasoning' && Array.isArray(it.summary)){
        var t = it.summary.map(function(s){ return (s && s.text) || ''; }).join('');
        if(t) blocks.push({type:'think', text:t});
      } else if(it.type === 'function_call' || it.type === 'custom_tool_call'){
        blocks.push({type:'tool', name: it.name || it.type, input: it.arguments || it.input || ''});
      } else if(it.type === 'web_search_call'){
        var act = it.action || {};
        var srcs = (act.sources || []).map(function(s){ return {title:'', url: (s && s.url) || s || ''}; });
        blocks.push({type:'search', query: act.query || '', results: srcs});
      }
    });
  }
  raw.split('\n').forEach(function(line){
    if(line.indexOf('data:') !== 0) return;
    var p = line.slice(5).trim();
    if(p === '' || p === '[DONE]') return;
    var j;
    try { j = JSON.parse(p); } catch(e) { return; }
    // error 事件：流内错误（限流/overloaded 等），显示错误信息。
    // 兼认 Responses 的扁平 error 事件（{type:'error', code, message}，无嵌套 error 对象）。
    if(j.type === 'error' && (j.error || j.message)){
      blocks.push({type:'error', msg: (j.error && (j.error.message || j.error.type)) || j.message || 'error'});
      curText = null; curThink = null;
      return;
    }
    // ---- Responses 原生透传流（url_response_api）：事件名 response.*，须在通用 delta 兜底前分流 ----
    if(j.type === 'response.output_text.delta' && j.delta != null){ textBlock().text += j.delta; return; }
    if(j.type === 'response.reasoning_summary_text.delta' && j.delta != null){ thinkBlock().text += j.delta; return; }
    if(j.type === 'response.function_call_arguments.delta' && j.delta != null){
      // 按 item_id 回填；added 丢失/在途截断时退回最近 tool 块，再没有就新建
      var fb = rIdx[j.item_id];
      if(!fb){ for(var k = blocks.length-1; k >= 0; k--){ if(blocks[k].type === 'tool'){ fb = blocks[k]; break; } } }
      if(!fb){ fb = {type:'tool', name:'function_call', input:''}; blocks.push(fb); curText = null; curThink = null; }
      fb.input += j.delta;
      return;
    }
    if(j.type === 'response.output_item.added' || j.type === 'response.output_item.done'){
      var it = j.item;
      if(!it || !it.type) return;
      if(it.type === 'function_call' || it.type === 'custom_tool_call'){
        var rtb = rIdx[it.id];
        if(!rtb){ rtb = {type:'tool', name: it.name || it.type, input:''}; blocks.push(rtb); if(it.id) rIdx[it.id] = rtb; curText = null; curThink = null; }
        if(it.name) rtb.name = it.name; // done 事件里的 name 可能更全
        // done 时 arguments 兜底回填：delta 被截断/丢失时好歹显示完整入参；delta 已填过就不覆盖
        if(j.type === 'response.output_item.done' && !rtb.input && it.arguments) rtb.input = it.arguments;
      } else if(it.type === 'web_search_call'){
        var rsb = rIdx[it.id];
        if(!rsb){ rsb = {type:'search', query:'', results:[]}; blocks.push(rsb); if(it.id) rIdx[it.id] = rsb; curText = null; curThink = null; }
        var act = it.action || {};
        if(act.query && !rsb.query) rsb.query = act.query;
        if(act.sources && rsb.results.length === 0){
          rsb.results = act.sources.map(function(s){ return {title:'', url: (s && s.url) || s || ''}; });
        }
      } else if(it.type === 'reasoning' && Array.isArray(it.summary)){
        // 只在 added 时 summary 已带文本才兜底渲染（部分端点一次性给全）；
        // 流式场景 summary 起始为空、由 reasoning_summary_text.delta 填充，这里不重复渲染
        var rt = it.summary.map(function(s){ return (s && s.text) || ''; }).join('');
        if(rt && j.type === 'response.output_item.added') blocks.push({type:'think', text:rt});
      }
      return;
    }
    if(j.type === 'response.completed' && j.response){
      // 长流归档只留尾部（flightContentCap）：delta 全被截掉时，终局事件里的完整 output 是最后兜底
      if(blocks.length === 0 && Array.isArray(j.response.output)) renderResponsesOutput(j.response.output);
      return;
    }
    if(j.type === 'response.failed'){
      var fe = j.response && j.response.error;
      blocks.push({type:'error', msg: (fe && (fe.message || fe.code)) || 'response.failed'});
      curText = null; curThink = null;
      return;
    }
    if(j.type === 'response.incomplete') return; // 终局标记，内容已随 delta 渲染
    // content_block_start：tool_use / server_tool_use / web_search_tool_result 建块
    if(j.type === 'content_block_start' && j.content_block){
      var cb = j.content_block;
      if(cb.type === 'tool_use' || cb.type === 'server_tool_use'){
        var tb = {type:'tool', name: cb.name || cb.type, input: ''};
        blocks.push(tb); idx[j.index] = tb; curText = null; curThink = null;
      } else if(cb.type === 'web_search_tool_result'){
        var sb = {type:'search', results: cb.content || []};
        blocks.push(sb); idx[j.index] = sb; curText = null; curThink = null;
      } else if(cb.type === 'redacted_thinking'){
        blocks.push({type:'think', text: '[已编辑思考（加密）]'});
        curText = null; curThink = null;
      }
      return;
    }
    // content_block_delta：text/thinking 累加，input_json_delta 回填到对应 tool
    if(j.type === 'content_block_delta' && j.delta){
      var d = j.delta;
      if(d.type === 'text_delta' && d.text){ textBlock().text += d.text; }
      else if(d.type === 'thinking_delta' && d.thinking){ thinkBlock().text += d.thinking; }
      else if(d.type === 'input_json_delta' && d.partial_json != null){
        // idx[index] 可能不存在：content_block_start 丢失/类型非 tool_use/在途流被截断
        var tb = idx[j.index];
        if(!tb){ for(var k = blocks.length-1; k >= 0; k--){ if(blocks[k].type === 'tool'){ tb = blocks[k]; break; } } }
        if(!tb){ tb = {type:'tool', name:'tool_use', input:''}; blocks.push(tb); curText = null; curThink = null; }
        tb.input += d.partial_json;
      }
      return;
    }
    // 扁平 text_delta / thinking_delta（部分上游不包 content_block_delta，直接发 delta 事件）
    if(j.type === 'text_delta' && j.text != null){ textBlock().text += j.text; return; }
    if(j.type === 'thinking_delta' && j.thinking != null){ thinkBlock().text += j.thinking; return; }
    // 扁平 input_json_delta（部分上游不包 content_block_delta，直接发 input_json_delta 事件）
    if(j.type === 'input_json_delta' && j.partial_json != null){
      var tb = idx[j.index];
      if(!tb){
        for(var k = blocks.length-1; k >= 0; k--){ if(blocks[k].type === 'tool'){ tb = blocks[k]; break; } }
      }
      if(!tb){ tb = {type:'tool', name:'tool_use', input:''}; blocks.push(tb); curText = null; curThink = null; }
      tb.input += j.partial_json;
      return;
    }
    // 兜底：通用 delta（非 Anthropic 标准事件结构的流）
    if(j.delta){
      if(j.delta.text) textBlock().text += j.delta.text;
      else if(j.delta.thinking) thinkBlock().text += j.delta.thinking;
      else if(j.delta.content) textBlock().text += j.delta.content;
      else if(j.delta.reasoning_content) thinkBlock().text += j.delta.reasoning_content;
      return;
    }
    // OpenAI 风格
    if(j.choices && j.choices[0] && j.choices[0].delta){
      var dd = j.choices[0].delta;
      if(dd.content) textBlock().text += dd.content;
      else if(dd.reasoning_content) thinkBlock().text += dd.reasoning_content;
    }
  });
  // 非流式 JSON 兜底：无 data: 行（非 SSE）时，解析整个 body 为非流式响应。
  // 分类器等请求常返回非流式 JSON（stream:false 或上游偶发不流式）。
  if(blocks.length === 0){
    try {
      var nj = JSON.parse(raw.trim());
      if(Array.isArray(nj.content)){
        // Anthropic 非流式：{content:[{type:'text'|'thinking'|'tool_use'|'redacted_thinking'}]}
        nj.content.forEach(function(c){
          if(c.type === 'text' && c.text) blocks.push({type:'text', text:c.text});
          else if(c.type === 'thinking' && c.thinking) blocks.push({type:'think', text:c.thinking});
          else if(c.type === 'tool_use') blocks.push({type:'tool', name:c.name||'tool_use', input: JSON.stringify(c.input||{})});
          else if(c.type === 'redacted_thinking') blocks.push({type:'think', text:'[已编辑思考（加密）]'});
        });
      } else if(nj.choices && nj.choices[0] && nj.choices[0].message){
        // OpenAI 非流式：{choices:[{message:{content, reasoning_content}}]}
        var mc = nj.choices[0].message;
        if(mc.reasoning_content) blocks.push({type:'think', text:mc.reasoning_content});
        if(mc.content) blocks.push({type:'text', text: typeof mc.content === 'string' ? mc.content : JSON.stringify(mc.content)});
      } else if(nj.object === 'response' && Array.isArray(nj.output)){
        // Responses 非流式（透传客户端 stream:false）：{object:'response', output:[...]}
        renderResponsesOutput(nj.output);
        if(nj.error && nj.error.message) blocks.push({type:'error', msg: nj.error.message});
      } else if(nj.error && nj.error.message){
        blocks.push({type:'error', msg: nj.error.message});
      }
    } catch(e) {}
  }
  var html = '';
  blocks.forEach(function(b){
    if(b.type === 'text') html += '<div class="ss-text">'+esc(b.text)+'</div>';
    else if(b.type === 'think') html += '<div class="ss-think">'+esc(b.text)+'</div>';
    else if(b.type === 'tool') html += '<div class="ss-tool">🔧 <b>'+esc(b.name)+'</b> <code>'+esc(b.input)+'</code></div>';
    else if(b.type === 'error') html += '<div class="ss-error">⛔ <b>错误</b> '+esc(b.msg)+'</div>';
    else if(b.type === 'search'){
      var q = b.query ? ' <code>'+esc(b.query)+'</code>' : ''; // Responses web_search_call 带查询词
      var items = (b.results||[]).map(function(r){ return '<div>· '+esc(r.title||'')+' <span class="ss-url">'+esc(r.url||'')+'</span></div>'; }).join('');
      html += '<div class="ss-search">🔍 搜索结果'+q+' ('+(b.results||[]).length+')'+items+'</div>';
    }
  });
  return html;
}
// 查看区按钮/标签可见性：看请求体仅手选单流时出现；下载按钮还要「储存完整结构体」开启
// （完整副本只在该模式下记录，关闭即被服务端清空）；交互式JSON 不要求开关——能浏览的内容就能成树。
// 该流数据残缺时下载按钮置灰带悬停提示：
// 请求体被截断（rt）的不能当完整版下载，无完整输出副本（!hf）的不能下载输出。
function updateFlightViewChrome(){
  var whatBtn = document.getElementById('flightViewWhatBtn');
  var sideBtn = document.getElementById('flightViewSideBtn');
  var sideNote = document.getElementById('flightViewSideNote');
  var dlBtn = document.getElementById('flightViewDlBtn');
  var dlOutBtn = document.getElementById('flightViewDlOutBtn');
  var treeBtn = document.getElementById('flightViewTreeBtn');
  var rawBtn = document.getElementById('flightViewRawBtn');
  var fl = flightFlags[selectedFlight] || {};
  var down = flightViewSide==='down';
  if(selectedFlight){
    whatBtn.style.display = '';
    whatBtn.textContent = flightViewWhat==='req' ? '看输出[渲染]' : (flightViewWhat==='resp' ? '看输出[流式原始]' : (flightViewWhat==='raw' ? '看输出[非流式]' : '看请求体'));
    sideBtn.style.display = '';
    sideBtn.textContent = down ? '链路:下游<->代理' : '链路:代理<->上游'; // ASCII 箭头：↔ 在按钮上会走 emoji 字体，挤且丑
    // 类型标签带链路侧后缀：请求体[代理→上游] / 输出[上游→代理] / 请求体[下游→代理] / 输出[代理→下游]
    var kind = flightViewWhat==='req' ? '请求体' : (flightViewWhat==='resp' ? '输出[渲染]' : (flightViewWhat==='raw' ? '输出[流式原始]' : '输出[非流式]'));
    kind += down ? (flightViewWhat==='req' ? '[下游→代理]' : '[代理→下游]') : (flightViewWhat==='req' ? '[代理→上游]' : '[上游→代理]');
    document.getElementById('flightViewKind').textContent = kind;
    dlBtn.style.display = fullStoreOn ? '' : 'none';
    dlBtn.disabled = down ? fl.rdt === true : fl.rt === true;
    dlBtn.title = dlBtn.disabled ? '该流请求体只剩截断版（前 256KB），完整版未记录或已清空' : '';
    dlOutBtn.style.display = fullStoreOn ? '' : 'none';
    dlOutBtn.disabled = down ? fl.hfd === false : fl.hf === false;
    dlOutBtn.title = dlOutBtn.disabled ? '该流未记录完整输出（开启前已开始/已清空/无透传内容）' : '';
    treeBtn.style.display = ''; // 四态都可交互看树，不要求储存开关：该侧有完整副本优先拉完整版，否则对手头浏览内容（前 256KB）成树；非流式态对组装结果成树
    rawBtn.style.display = 'none'; // 单流四态循环已含 渲染/流式原始，解析/原始切换按钮只留给 grid 多流自动跟踪
    treeBtn.textContent = flightViewTree ? '退出交互' : '交互式JSON';
  } else {
    whatBtn.style.display = 'none';
    sideBtn.style.display = 'none';
    sideNote.textContent = '';
    dlBtn.style.display = 'none';
    dlOutBtn.style.display = 'none';
    treeBtn.style.display = 'none';
    rawBtn.style.display = ''; // 无手选（grid 多流）时 显示原始/解析 仍可用
    document.getElementById('flightViewKind').textContent = '输出';
  }
}
// 请求体渲染：解析=JSON 美化（非完整 JSON 退回原文并注明），原始=逐字原文。
// 显示永远只给前 256KB：开关关闭时服务端本就截断；开启时服务端存完整体，显示截断、完整体走下载。
// rt（服务端下发）= 该流请求体只剩截断版：被 purge 截回的流不再提示「下载获取完整」。
function renderReqBody(){
  var fv = document.getElementById('flightView');
  var full = lastReqRaw;
  var rt = (flightViewSide==='down' ? (flightFlags[selectedFlight]||{}).rdt : (flightFlags[selectedFlight]||{}).rt) === true; // 截断标记按当前链路侧取
  var overCap = new TextEncoder().encode(full).length >= 262144;
  var note = '';
  if(rt) note = '（请求体超长，仅保留前 256KB）\n';
  else if(overCap) note = fullStoreOn ? '（仅显示前 256KB，点「下载请求体」获取完整）\n' : '（请求体超长，仅保留前 256KB）\n';
  var disp;
  if(flightViewRaw){ disp = full; }
  else {
    try{ disp = JSON.stringify(JSON.parse(full), null, 2); }
    catch(e){ disp = (rt ? '' : '（请求体非完整 JSON，按原文显示）\n') + full; }
  }
  if(fullStoreOn && !rt && disp.length > 262144) disp = disp.slice(0, 262144);
  fv.textContent = note + disp;
}
// renderRespFromLastRaw 用 lastRaw 重渲染输出视图（切状态/退出交互树/切链路侧时用）：渲染=解析成可读文本，流式原始=逐字原文，非流式=组装成最终结构。
function renderRespFromLastRaw(){
  var fv = document.getElementById('flightView');
  if(lastRaw === ''){
    fv.textContent = flightEnded ? '（该流无透传内容：失败/重试用尽/非流式）' : '加载中…';
  } else if(flightViewWhat==='asm'){ fv.textContent = assembledOutputText(lastRaw); }
  else if(flightViewWhat==='raw'){ fv.textContent = lastRaw; }
  else { var h = parseSSEHTML(lastRaw); fv.innerHTML = h || '<span class="ss-empty">（未解析出内容，切到 输出[流式原始] 查看）</span>'; }
}
// ---- 交互式 JSON 树：树根第一层默认展开，更深层点击键名行懒展开（子节点展开时才构建，大 JSON 不一次卡死）----
// jtPrimitiveHTML 渲染标量值（字符串带引号转义，null/数字/布尔着色）。
function jtPrimitiveHTML(v){
  if(v === null) return '<span class="jt-null">null</span>';
  if(typeof v === 'string') return '<span class="jt-str">'+esc(JSON.stringify(v))+'</span>';
  if(typeof v === 'number') return '<span class="jt-num">'+v+'</span>';
  if(typeof v === 'boolean') return '<span class="jt-bool">'+v+'</span>';
  return esc(String(v));
}
// jtNode 构建一个键值对行：容器（对象/数组）默认折叠成 {N 键}/[N 项] 摘要，点击展开时
// 懒构建子行、再点折叠回摘要；空容器与标量直接内联不可点。数组项的 key 传下标数字。
function jtNode(key, value){
  var line = document.createElement('div');
  line.className = 'jt-line';
  var keyHtml = key===null ? '' : '<span class="jt-key">'+esc(JSON.stringify(key))+'</span>: ';
  var isObj = value!==null && typeof value==='object';
  if(!isObj){
    line.innerHTML = '<span class="jt-arrow"></span>'+keyHtml+jtPrimitiveHTML(value);
    return line;
  }
  var isArr = Array.isArray(value);
  var keys = isArr ? value.map(function(_,i){return i;}) : Object.keys(value);
  var open = isArr?'[':'{', close = isArr?']':'}';
  if(keys.length===0){
    line.innerHTML = '<span class="jt-arrow"></span>'+keyHtml+open+close;
    return line;
  }
  var head = document.createElement('span');
  head.className = 'jt-head';
  var summary = '<span class="jt-arrow">▶</span>'+keyHtml+open+' <span class="jt-count">'+keys.length+(isArr?' 项':' 键')+'</span> '+close;
  head.innerHTML = summary;
  line.appendChild(head);
  var kids = null; // 展开后才构建的子节点容器；折叠时销毁
  head.onclick = function(){
    if(kids){
      kids.remove();
      kids = null;
      head.innerHTML = summary;
      return;
    }
    kids = document.createElement('div');
    kids.className = 'jt-kids';
    keys.forEach(function(k){ kids.appendChild(jtNode(k, value[k])); });
    var tail = document.createElement('div');
    tail.className = 'jt-line';
    tail.innerHTML = '<span class="jt-arrow"></span>'+close;
    kids.appendChild(tail);
    line.appendChild(kids);
    head.innerHTML = '<span class="jt-arrow">▼</span>'+keyHtml+open;
  };
  return line;
}
// jsonTreeRoot 用整值构建树根（根无 key）；第一层默认展开：构建后触发一次根头点击走原懒展开路径，
// 子容器仍是折叠摘要、更深一层照常点键懒构建；标量/空容器根没有 .jt-head 可点，原样内联。
function jsonTreeRoot(value){
  var root = document.createElement('div');
  var line = jtNode(null, value);
  root.appendChild(line);
  var head = line.querySelector('.jt-head');
  if(head) head.onclick(); // 第一层默认展开
  return root;
}
// sseToJSONArray 把 SSE 原文解析成 [{event, data}, ...] 供交互式树查看；
// data 非 JSON 时保留原文行；全为注释/空块时返回空数组（表示不像 SSE）。
function sseToJSONArray(raw){
  var out = [];
  raw.split(/\r?\n\r?\n/).forEach(function(block){
    var ev = null, datas = [];
    block.split(/\r?\n/).forEach(function(l){
      if(l.indexOf('event:')===0) ev = l.slice(6).trim();
      else if(l.indexOf('data:')===0) datas.push(l.slice(5).trim());
    });
    if(ev===null && !datas.length) return;
    var d = datas.join('\n');
    try{ d = JSON.parse(d); }catch(e){}
    out.push({event: ev, data: d});
  });
  return out;
}
// assembledOutputText 渲染 输出[非流式]：整体 JSON（非流式上游/重建回传）直接美化；SSE 事件流组装成最终结构
// （Anthropic 流按 message_start 骨架 + delta 累加组装，Responses 流取 response.completed 的 response 对象）；
// 流未完整到达时顶部注明是部分快照；两种格式都不像时提示切回 输出[流式原始]。
function assembledOutputText(raw){
  try{ return JSON.stringify(JSON.parse(raw), null, 2); }catch(e){}
  var r = assembleSSEToObject(raw);
  if(!r) return '（无法整理：输出不是 JSON 也不是可识别的 SSE 事件流；切回 输出[流式原始] 查看）';
  return (r.complete ? '' : '（流未完整到达，以下是已到部分的整理）\n') + JSON.stringify(r.obj, null, 2);
}
// assembleSSEToObject 把 SSE 原文整理成最终响应对象（规则同服务端 collectStreamToJSON）：Anthropic 事件流 =
// message_start 骨架 + content_block_start/delta 按 index 累加（text/thinking/signature/input_json）+ message_delta
// 合并 stop_reason/usage，message_stop 标记完整；Responses 事件流 = response.completed/failed/incomplete 的
// response 对象，在途则 response.created 壳 + 已到 output_item.done 项。返回 {obj, complete}；不像两种格式返回 null。
function assembleSSEToObject(raw){
  var evs = sseToJSONArray(raw);
  if(!evs.length) return null;
  var isResp = false;
  for(var i=0;i<evs.length;i++){
    var t0 = evs[i].event || (evs[i].data && evs[i].data.type) || '';
    if(typeof t0 === 'string' && t0.indexOf('response.')===0){ isResp = true; break; }
  }
  if(isResp){
    var resp = null, complete = false, items = [];
    evs.forEach(function(e){
      var d = e.data; if(!d || typeof d !== 'object') return;
      var t = e.event || d.type || '';
      if((t==='response.completed'||t==='response.failed'||t==='response.incomplete') && d.response){ resp = d.response; complete = true; }
      else if((t==='response.created'||t==='response.in_progress') && d.response && !resp){ resp = d.response; }
      else if(t==='response.output_item.done' && d.item){ items.push({i: d.output_index, item: d.item}); }
    });
    if(!resp) return null;
    if(!complete && items.length){
      resp = JSON.parse(JSON.stringify(resp));
      items.sort(function(a,b){ return a.i-b.i; });
      resp.output = items.map(function(x){ return x.item; });
    }
    return {obj: resp, complete: complete};
  }
  var msg = null, blocks = {}, usage = null, stopped = false;
  evs.forEach(function(e){
    var d = e.data; if(!d || typeof d !== 'object') return;
    var t = d.type || e.event || '';
    if(t==='message_start' && d.message){ msg = d.message; if(d.message.usage) usage = d.message.usage; }
    else if(t==='content_block_start'){ blocks[d.index] = JSON.parse(JSON.stringify(d.content_block || {})); }
    else if(t==='content_block_delta' && blocks[d.index]){
      var b = blocks[d.index], dt = d.delta || {};
      if(dt.type==='text_delta') b.text = (b.text||'') + (dt.text||'');
      else if(dt.type==='thinking_delta') b.thinking = (b.thinking||'') + (dt.thinking||'');
      else if(dt.type==='signature_delta') b.signature = (b.signature||'') + (dt.signature||'');
      else if(dt.type==='input_json_delta') b._pj = (b._pj||'') + (dt.partial_json||'');
    }
    else if(t==='message_delta'){
      if(msg && d.delta) Object.keys(d.delta).forEach(function(k){ msg[k] = d.delta[k]; });
      if(d.usage){ usage = usage || {}; Object.keys(d.usage).forEach(function(k){ usage[k] = d.usage[k]; }); }
    }
    else if(t==='message_stop'){ stopped = true; }
  });
  if(!msg) return null;
  msg = JSON.parse(JSON.stringify(msg));
  msg.content = Object.keys(blocks).map(function(k){ return +k; }).sort(function(a,b){ return a-b; }).map(function(i){
    var b = blocks[i];
    if(b._pj !== undefined){ try{ b.input = JSON.parse(b._pj); }catch(e){ b.input = b._pj; } delete b._pj; }
    return b;
  });
  if(usage) msg.usage = usage;
  return {obj: msg, complete: stopped};
}
// 点击在途流/完成流行：切到单流模式（保持 autoTrack 勾选，由 selectedFlight 优先决定显示），显示该流。
function selectFlight(id){
  // 手选单流：不取消 autoTrack 勾选（点行查看时勾选保持），
  // 仅设 selectedFlight 让轮询切到单流分支；autoTrack 重新勾选时再清手选回 grid。
  selectedFlight = id;
  flightEnded = false;
  lastRaw = '';
  flightViewWhat = 'resp'; // 新手选默认看输出；请求体点「看请求体」再拉
  lastReqRaw = '';
  flightViewSide = 'up'; // 双链路默认 代理↔上游 侧；点「链路」按钮切 下游↔代理 侧
  flightViewTree = false;
  flightViewTreeManual = false;
  document.getElementById('flightViewSideNote').textContent = '';
  var fv = document.getElementById('flightView');
  fv.innerHTML = '';
  fv.style.display = 'block';
  fv.style.gridTemplateColumns = '';
  fv.style.whiteSpace = 'pre-wrap';
  fv.style.wordBreak = 'break-all';
  fv.textContent = '加载中…';
  document.getElementById('flightViewWrap').style.display = 'block';
  document.getElementById('flightViewId').textContent = id + (flightFlags[id]&&flightFlags[id].lst?'<-'+flightFlags[id].lst:'');
  updateFlightViewChrome();
}
function flightStatus(f){
  var s;
  if(f.stage===3) s = f.status?f.status:'响应';
  else if(f.stage===2) s = '尝试'+(f.attempt||1);
  else if(f.stage===1) s = '路由';
  else s = '请求';
  if(f.stage>=2) s += '·' + routeLabel(f.routeReason);
  return s;
}
// 路由原因 -> 中文标签（与 main.go route* 枚举对齐：0透传 1pattern 2分类器 3fast 4多模态 5搜索）。
function routeLabel(r){
  return ['透传','pattern','分类器','fast','多模态','搜索'][r] || '透传';
}
function fmtNum(n){
  if(n<1000) return ''+n;
  if(n<1000000) return (n/1000).toFixed(1)+'k';
  return (n/1000000).toFixed(2)+'M';
}
function card(k,v){ return '<div class="card"><div class="k">'+k+'</div><div class="v">'+v+'</div></div>'; }

async function poll(){
  try{
    const r = await fetch('/__logs/data',{cache:'no-store'});
    const d = await r.json();
    // 储存完整结构体开关：以服务端为准（决定下载按钮显隐）
    fullStoreOn = !!d.fullStore;
    var fsChk = document.getElementById('fullStoreChk');
    if(fsChk.checked !== fullStoreOn) fsChk.checked = fullStoreOn;
    // 状态灯
    stEl.className = 'st ' + (d.active>0?'active':d.waiting>0?'wait':'idle');
    stEl.textContent = d.active>0?'🟢 流式中':d.waiting>0?'🟡 等待首字节':'⚪ 空闲';
    document.getElementById('hijackWarn').style.display = d.hijackRisk ? '' : 'none';
    document.getElementById('curCfg').textContent = d.currentCfg ? ('配置: '+d.currentCfg) : '';
    // 统计卡片
    cardsEl.innerHTML =
      card('活跃/等待', d.active + ' / ' + d.waiting) +
      card('发给上游', fmtBytes(d.bytesUp)) + card('上游返回', fmtBytes(d.bytesDown)) +
      '<div class="card" id="cacheCard" style="cursor:pointer"><div class="k">缓存命中</div><div class="v">'+cacheHitPct(d.cacheRead, d.inputTokens, d.cacheCreation)+'</div></div>' + card('输入', fmtNum(d.inputTokens)) +
      card('输出', fmtNum(d.outputTokens)) + '<div class="card" id="retryCard" style="cursor:pointer"><div class="k">重试</div><div class="v">'+d.retries+'</div></div>' +
      '<div class="card" id="clsCard" style="cursor:pointer"><div class="k">分类器</div><div class="v">'+d.classifiers+'</div></div>' + card('首字', (d.avgFirstByte/1000).toFixed(2)+'s') +
      card('tok/s', d.tps.toFixed(1));
    // 缓存命中明细：缓存最新 modelStats / cacheObs / 分类器双计数，tooltip/modal 打开时实时刷新
    latestModelStats = d.modelStats || [];
    latestCacheObs = d.cacheObs || [];
    latestHeat = d.heat || []; latestHeatBase = d.heatBase || 0; // 活跃度热图缓存，弹窗打开时渲染
    latestClsHits = d.classifiers || 0;
    latestClsNoThink = d.classifierNoThink || 0;
    latestClsHitsCC = d.classifiersCC || 0;
    latestClsHitsCodex = d.classifiersCodex || 0;
    latestClsRwCC = d.classifierRewritesCC || 0;
    latestClsRwCodex = d.classifierRewritesCodex || 0;
    if(document.getElementById('cacheTip').style.display !== 'none') refreshCacheTip();
    if(document.getElementById('cacheModal').style.display !== 'none') refreshCacheModal();
    if(document.getElementById('retryTip').style.display !== 'none') refreshRetryTip();
    if(document.getElementById('retryModal').style.display !== 'none') refreshRetryModal();
    if(document.getElementById('clsTip').style.display !== 'none') refreshClsTip();
    if(document.getElementById('clsModal').style.display !== 'none') refreshClsModal();
    // 在途流
    const fs = d.flights || [];
    fs.forEach(function(f){ flightFlags[f.id] = {rt:!!f.reqTrunc, hf:!!f.hasFull, rd:!!f.reqDown, sd:!!f.respDown, rdt:!!f.reqDownTrunc, hfd:!!f.hasFullDown, lst:f.last||0}; });
    flightsBody.innerHTML = fs.map(f =>
      '<tr style="cursor:pointer" onclick="selectFlight('+f.id+')"><td>'+flightDot(f)+attemptTag(f)+' '+fmtStageDur(f.stageMs)+'</td><td>#'+f.id+lastTag(f)+'</td>'+modelCell(f)+apiCell(f.translated,f.think,f.thinkFrom,f.thinkTo)+tokensCell(f.tokens)+'<td>'+fmtBytes(f.bytes)+'</td><td>'+flightStatus(f)+'</td></tr>'
    ).join('');
    // 在途流输出查看：手选单流优先，否则自动跟踪 grid，否则隐藏
    var fv = document.getElementById('flightView');
    var wrap = document.getElementById('flightViewWrap');
    if(selectedFlight){
      // 手选单流模式（点表格行触发，优先于自动跟踪；autoTrack 勾选状态保持）
      wrap.style.display = 'block';
      document.getElementById('flightViewId').textContent = selectedFlight + (flightFlags[selectedFlight]&&flightFlags[selectedFlight].lst?'<-'+flightFlags[selectedFlight].lst:'');
      fv.style.display = 'block';
      fv.style.gridTemplateColumns = '';
      fv.style.whiteSpace = 'pre-wrap';
      fv.style.wordBreak = 'break-all';
      updateFlightViewChrome();
      if(!flightEnded && flightViewWhat!=='req' && !flightViewTree){ // req 模式/交互树看的是静态内容，不拉流；输出三态（渲染/流式原始/非流式）都随 poll 刷新
        try{
          const fr = await fetch('/__flight?id='+selectedFlight+'&side='+flightViewSide,{cache:'no-store'});
          if(fr.ok){
            // 缺侧回退提示：服务端实际返回侧与请求侧不一致时（该侧无记录）在链路按钮旁提示
            document.getElementById('flightViewSideNote').textContent = (fr.headers.get('X-Proxy429-Side')||'up') !== flightViewSide ? '（该侧无记录，显示另一侧）' : '';
            const atBottom = fv.scrollTop + fv.clientHeight >= fv.scrollHeight - 2;
            lastRaw = await fr.text();
            if(lastRaw === ''){
              fv.textContent = '（该流无透传内容：失败/重试用尽/非流式）';
            } else {
              if(flightViewWhat==='asm'){ fv.textContent = assembledOutputText(lastRaw); }
              else if(flightViewWhat==='raw'){ fv.textContent = lastRaw; } else { var h = parseSSEHTML(lastRaw); fv.innerHTML = h || '<span class="ss-empty">（未解析出内容，切到 输出[流式原始] 查看）</span>'; }
            }
            if(atBottom) fv.scrollTop = fv.scrollHeight; // 用户滚到底则跟随，否则不动
            // 完成流（不在在途列表）内容是静态快照，拉一次即可，停止重复拉取。
            if(!fs.some(function(x){return x.id===selectedFlight;})) flightEnded = true;
          } else if(fr.status === 404){
            flightEnded = true;
            document.getElementById('flightViewSideNote').textContent = '';
            var endDiv = document.createElement('div');
            endDiv.className = 'ss-empty';
            endDiv.textContent = '-- 流已结束 --';
            fv.appendChild(endDiv);
          }
        }catch(e){}
      }
    } else if(autoTrack){
      // 自动跟踪：在途流并排 grid（1 个则单列全宽，N 个则 1×N 左右排列）
      // 最多显示 maxFlights 个（最新 N 个），防止太多太细
      updateFlightViewChrome();
      var show = fs.slice(-maxFlights);
      if(show.length > 0){
        wrap.style.display = 'block';
        document.getElementById('flightViewId').textContent = show.length+' 个在途流';
        fv.style.display = 'grid';
        fv.style.gridTemplateColumns = 'repeat('+show.length+', 1fr)';
        fv.style.gap = '4px';
        fv.style.whiteSpace = '';
        fv.style.wordBreak = '';
        // 移除不在显示范围内的 cell（已结束或超出最多个数）
        Array.from(fv.children).forEach(function(cell){
          if(!show.some(function(f){return String(f.id)===cell.dataset.id;})){
            delete autoRaws[cell.dataset.id];
            cell.remove();
          }
        });
        // 新增 cell
        show.forEach(function(f){
          if(!fv.querySelector('[data-id="'+f.id+'"]')){
            var cell = document.createElement('div');
            cell.dataset.id = f.id;
            cell.innerHTML = '<div style="color:#9a9a9a;margin-bottom:2px">流 #'+f.id+lastTag(f)+' '+(f.model||'')+'</div><pre class="cellPre" style="max-height:240px;overflow:auto;background:#1a1a1a;border:1px solid #333;padding:6px;white-space:pre-wrap;word-break:break-all;margin:0;font:inherit">加载中…</pre>';
            fv.appendChild(cell);
            var np = cell.querySelector('.cellPre');
            np.dataset.stick = '1'; // 默认跟踪底部；用户手动上滚后才停止跟随
            np.addEventListener('scroll', function(){
              np.dataset.stick = (np.scrollTop + np.clientHeight >= np.scrollHeight - 2) ? '1' : '0';
            });
          }
        });
        // 拉取每个在途流内容（独立更新各自 cell）
        show.forEach(function(f){
          fetch('/__flight?id='+f.id,{cache:'no-store'}).then(function(r){return r.ok?r.text():null;}).then(function(raw){
            if(raw===null) return;
            autoRaws[f.id] = raw;
            var pre = fv.querySelector('[data-id="'+f.id+'"] .cellPre');
            if(pre){
              if(flightViewRaw){ pre.textContent = raw; } else { var h = parseSSEHTML(raw); pre.innerHTML = h || '<span class="ss-empty">（未解析出内容，点「显示原始」查看）</span>'; }
              if(pre.dataset.stick === '1') pre.scrollTop = pre.scrollHeight;
            }
          }).catch(function(){});
        });
      } else {
        wrap.style.display = 'none';
        fv.innerHTML = '';
      }
    } else {
      // 未勾选自动跟踪，也无手选：隐藏查看区
      wrap.style.display = 'none';
      fv.innerHTML = '';
    }
    // 最近完成流列表（点击复用查看区回看其输出）
    try{
      const rf = await fetch('/__recentflights',{cache:'no-store'});
      if(rf.ok){
        const rfd = await rf.json();
        (rfd.list||[]).forEach(function(f){ flightFlags[f.id] = {rt:!!f.reqTrunc, hf:!!f.hasFull, rd:!!f.reqDown, sd:!!f.respDown, rdt:!!f.reqDownTrunc, hfd:!!f.hasFullDown, lst:f.last||0}; });
        document.querySelector('#finishedFlights tbody').innerHTML = (rfd.list||[]).map(function(f){
          // Finished rows render the [剥N] strip badge in the cache-hit column, so clear stripped before the shared model cell (no duplicate badge).
          return '<tr style="cursor:pointer" onclick="selectFlight('+f.id+')"><td>#'+f.id+lastTag(f)+'</td>'+modelCell(Object.assign({}, f, {stripped:0}))+apiCell(f.translated,f.think,f.thinkFrom,f.thinkTo)+tokensCell(f.tokens, f.bytes)+'<td>'+(f.total||'-')+'</td><td>'+(f.gaveUp?'[重试尽]':(f.status?(f.status===200?'200':'['+f.status+']'):'-'))+(f.attempts>1?'<span style="color:#d7ba7d">[重试'+(f.attempts-1)+'次]</span>':'')+(f.noStop?'<span style="color:#f48771">[无stop]</span>':'')+(f.searchGuard?'<span style="color:#f48771">[空搜]</span>':'')+'</td><td>'+(f.hitRate||'-')+(f.stripped>0?'<span style="color:#f48771">[剥'+f.stripped+']</span>':'')+'</td><td>'+(f.cacheAge||'-')+'</td><td>'+(f.firstByte||'-')+'</td><td>'+(f.tps||'-')+'</td><td>'+f.ended+'</td></tr>';
        }).join('');
      }
    }catch(e){}
    // 回填保留个数（用户正在输入时不覆盖）
    var fcInp = document.getElementById('finishedCapInput');
    if(document.activeElement !== fcInp) fcInp.value = d.finishedCap;
    // 日志
    logEl.textContent = (d.lines||[]).join('\n');
    document.getElementById('count').textContent = d.lines?d.lines.length:0;
    // 日志页自动滚到底（仅日志标签；状态页推流时主窗口不应被拽到底）
    if(stick && document.querySelector('.tab.active').dataset.tab === 'logs') window.scrollTo(0, document.body.scrollHeight);
  }catch(e){
    stEl.className='st dead';
    stEl.textContent='🔴 已断开（代理可能已退出）';
  }
}
poll();
setInterval(poll, 500);

// 配置标签
// fillCfgLists 用 /__configs 的结果填充切换下拉 cfgSelect（全部，当前打勾）。
function fillCfgLists(dc){
  const sel = document.getElementById('cfgSelect');
  sel.innerHTML = (dc.files||[]).map(f => '<option value="'+f+'"'+(f===dc.current?' selected':'')+'>'+f+'</option>').join('');
}

async function loadConfig(){
  try{
    const [r, rc] = await Promise.all([
      fetch('/__config',{cache:'no-store'}),
      fetch('/__configs',{cache:'no-store'})
    ]);
    const d = await r.json();
    document.getElementById('cfgPath').textContent = d.path + (d.exists?'':'（文件不存在，保存将创建）');
    document.getElementById('cfg').value = d.content || '';
    cfgLoaded = true;
    genCodexCmds();
    const dc = await rc.json();
    fillCfgLists(dc);
    lastCfgFiles = JSON.stringify(dc.files);
  }catch(e){
    document.getElementById('cfgMsg').innerHTML = '<span class="err">加载失败: '+e+'</span>';
  }
}

// loadConfigList 只刷新配置文件下拉列表（不覆盖编辑器未保存内容），供配置页轮询/切回时用。
// 列表未变化时跳过重绘，避免下拉闪烁。
function loadConfigList(){
  fetch('/__configs',{cache:'no-store'}).then(r=>r.json()).then(d=>{
    const sig = JSON.stringify(d.files);
    if(sig !== lastCfgFiles){ fillCfgLists(d); lastCfgFiles = sig; }
  }).catch(()=>{});
}

function setMsg(cls, txt){ document.getElementById('cfgMsg').innerHTML = '<span class="'+cls+'">'+txt+'</span>'; }

document.getElementById('saveBtn').onclick = async () => {
  setMsg('', '保存中…');
  try{
    const r = await fetch('/__config',{method:'POST',body:document.getElementById('cfg').value});
    if(r.ok){ setMsg('ok', '已保存并重载'); const d = await r.json().catch(()=>{}); }
    else { const t = await r.text(); setMsg('err', '失败: '+t); }
  }catch(e){ setMsg('err', '失败: '+e); }
};
document.getElementById('reloadBtn').onclick = async () => {
  setMsg('', '重载中…');
  try{
    const r = await fetch('/__reload',{method:'POST'});
    if(r.ok) setMsg('ok','已重载'); else setMsg('err','失败: '+await r.text());
  }catch(e){ setMsg('err','失败: '+e); }
};

// lazyApply 预览：把编辑框当前文本发给服务端，按真实加载规则补默认值后只读展示。
// 预览保留 lazyApply 本身和 $VAR 引用；复制回编辑框再保存即可把默认值固化为显式字段。
document.getElementById('lazyPreviewBtn').onclick = async () => {
  setMsg('', '预览中…');
  try{
    const r = await fetch('/__lazyapply',{method:'POST',body:document.getElementById('cfg').value});
    const t = await r.text();
    if(!r.ok){ setMsg('err', '失败: '+t); return; }
    document.getElementById('lazyPreview').value = t;
    document.getElementById('lazyPreviewBox').style.display = 'block';
    document.getElementById('lazyPreviewMsg').innerHTML = '';
    setMsg('ok', '已按 lazyApply 展开');
  }catch(e){ setMsg('err', '失败: '+e); }
};
document.getElementById('lazyPreviewCopyBtn').onclick = () => {
  document.getElementById('cfg').value = document.getElementById('lazyPreview').value;
  document.getElementById('lazyPreviewMsg').innerHTML = '<span class="ok">已复制到编辑框（尚未保存）</span>';
};
document.getElementById('lazyPreviewCloseBtn').onclick = () => {
  document.getElementById('lazyPreviewBox').style.display = 'none';
};

// ---- Codex 一键命令：按编辑框的 responses_listen + routes + 所选模型实时生成 ----
// 页面只给一行终端命令（DeepSeek 文档同款格式），脚本由服务端 /__codexsetup.ps1|sh
// 按 query 参数（base/model/catalog）烤制下发；值全是白名单字符，query 无需转义。
let codexModelList = [{name:'gpt-5-codex',src:'gpt-5*'},{name:'claude-fable-5',src:''}]; // 上次成功推导的目录模型
// responses_listen 为空 = Responses 口未启用：生成区整组置灰，只留「添加该功能」入口。
// 绝不回落默认地址——本机 8081 可能被别的程序占用，Codex 指过去会出莫名错误。
function setCodexUiEnabled(on){
  document.getElementById('codexEnable').style.display = on ? 'none' : '';
  document.getElementById('codexGen').classList.toggle('off', !on);
  if(!on){
    document.getElementById('codexCmdWin').textContent = '（Responses API 未启用：点上方「是，添加并保存」后这里才会生成命令）';
    document.getElementById('codexCmdSh').textContent = '（同上）';
  }
}
// 「是，添加并保存」：在编辑框 JSON 的 "listen" 行后插入 responses_listen，再走既有保存流程
document.getElementById('codexEnableBtn').onclick = () => {
  const ta = document.getElementById('cfg');
  const lines = ta.value.split('\n');
  let idx = lines.findIndex(l => /^\s*"listen"\s*:/.test(l));
  if(idx < 0) idx = lines.findIndex(l => l.indexOf('{') >= 0);
  if(idx < 0){ document.getElementById('codexEnableMsg').innerHTML = '<span class="err">找不到插入位置，请手动在配置 JSON 里加一行 "responses_listen": "127.0.0.1:8081",</span>'; return; }
  const indent = (lines[idx].match(/^\s*/) || [''])[0];
  lines.splice(idx + 1, 0, indent + '"responses_listen": "127.0.0.1:8081",');
  ta.value = lines.join('\n');
  document.getElementById('codexEnableMsg').textContent = '';
  document.getElementById('saveBtn').click();
  genCodexCmds();
  document.getElementById('codexMsg').innerHTML = '<span class="ok">已添加并保存 responses_listen（端口可在上方 JSON 改），监听口已随保存启动，现在就能用下面的命令</span>';
};
// 与 main.go matchModel 同语义：* 匹配任意长度（含 0）任意字符
function matchPat(p, n){
  const parts = String(p).split('*');
  if(parts.length === 1) return p === n;
  if(!n.startsWith(parts[0])) return false;
  n = n.slice(parts[0].length);
  for(let i = 1; i < parts.length - 1; i++){
    const idx = n.indexOf(parts[i]);
    if(idx < 0) return false;
    n = n.slice(idx + parts[i].length);
  }
  return n.endsWith(parts[parts.length - 1]);
}
// 从编辑框 routes 推导目录模型（Codex /model 菜单内容）：route.model 自己能命中 pattern 就用真名
// （thinking 查表更准），否则用去掉 * 的 pattern 代表名（* 可匹配 0 字符，必命中）；
// 纯 * 兜底路由固定叫 "Fallback"——它接住的是任意模型名，菜单里显示某个真实 model 名会误导。
// JSON 暂无法解析返回 null（保持现有清单不动，打字途中不闪）。
function deriveCodexModels(){
  let c;
  try{ c = JSON.parse(document.getElementById('cfg').value); }catch(e){ return null; }
  const out = [];
  for(const r of (c.routes || [])){
    if(!r) continue;
    // pattern 可为数组（OR，等价于复制多份路由）：每个成员各出一个代表名，顺序与服务端展开一致
    for(const p of (Array.isArray(r.pattern) ? r.pattern : [r.pattern])){
      if(typeof p !== 'string' || !p) continue;
      if(p === 'Fallback' || p === 'fast_route') continue; // 保留名（* 兜底 / fast 通道在菜单里的名字）：全字撞名的路由不生效也不进菜单
      const m = (typeof r.model === 'string') ? r.model.trim() : '';
      const name = (p === '*') ? 'Fallback' : ((m && matchPat(p, m)) ? m : (p.replace(/\*/g, '') || m));
      // 名字要过服务端 slug 白名单（烤进脚本单引号串 + URL query 都不能有特殊字符）：不合规的跳过
      if(name && /^[A-Za-z0-9._*/:+-]+$/.test(name) && !out.some(o => o.name === name)) out.push({name: name, src: p});
    }
  }
  // fast_route 也进菜单：条目名就是字面名 "fast_route"（翻译层认这个名字注入 speed:"fast" 走 fast 通道）；
  // 没配 model 不列——fast 分支要把 model 改写成 fast_route.model 发给上游
  const fr = c.fast_route;
  if(fr && typeof fr.url === 'string' && fr.url && typeof fr.model === 'string' && fr.model.trim()){
    if(!out.some(o => o.name === 'fast_route')) out.push({name: 'fast_route', src: 'fast_route'});
  }
  return out;
}
// 模型下拉按 routes 重建（选中项尽量保留）；只在清单变化时调用，避免打字途中闪烁
function refreshCodexModels(list){
  const sel = document.getElementById('codexModel');
  const prev = sel.value;
  sel.innerHTML = '';
  for(const o of list){
    const opt = document.createElement('option');
    opt.value = o.name;
    opt.textContent = (o.src && o.src !== o.name) ? o.name + '（' + o.src + '）' : o.name;
    sel.appendChild(opt);
  }
  const oc = document.createElement('option'); oc.value = '__custom__'; oc.textContent = '自定义…'; sel.appendChild(oc);
  const orr = document.createElement('option'); orr.value = '__restore__'; orr.textContent = '（还原默认 Codex 配置）'; sel.appendChild(orr);
  let found = false;
  for(const opt of sel.options){ if(opt.value === prev){ found = true; break; } }
  sel.value = found ? prev : sel.options[0].value;
  document.getElementById('codexModelCustom').style.display = (sel.value === '__custom__') ? '' : 'none';
}
function codexModelChoice(){
  const v = document.getElementById('codexModel').value;
  if(v === '__custom__'){
    const m = document.getElementById('codexModelCustom').value.trim();
    return /^[A-Za-z0-9._*/:+-]+$/.test(m) ? m : codexModelList[0].name;
  }
  return v;
}
// 拼两行终端命令；qs 的值全是白名单字符（slug 白名单 / http 地址），无需 URL 转义
function codexScriptCmds(qs){
  const o = location.origin;
  return {
    win: "irm '" + o + "/__codexsetup.ps1?" + qs + "' | iex",
    sh: "bash <(curl -fsSL '" + o + "/__codexsetup.sh?" + qs + "')"
  };
}
function genCodexCmds(){
  const ta = document.getElementById('cfg');
  if(!ta.value.trim()) return; // 配置尚未加载
  // 还原不碰代理：即使 responses_listen 缺失也直接给命令（也就无需「添加该功能」提示）
  if(codexModelChoice() === '__restore__'){
    document.getElementById('codexMsg').textContent = '';
    setCodexUiEnabled(true);
    const cmds = codexScriptCmds('model=__restore__');
    document.getElementById('codexCmdWin').textContent = cmds.win;
    document.getElementById('codexCmdSh').textContent = cmds.sh;
    return;
  }
  let c;
  try{ c = JSON.parse(ta.value); }
  catch(e){
    // 打字途中解析失败：不动现有内容，只提示
    document.getElementById('codexMsg').innerHTML = '<span class="err">配置 JSON 暂无法解析，命令保持上次有效内容</span>';
    return;
  }
  const addr = String(c.responses_listen || '').trim();
  if(!addr || /['"\s]/.test(addr)){ document.getElementById('codexMsg').textContent = ''; setCodexUiEnabled(false); return; }
  document.getElementById('codexMsg').textContent = '';
  setCodexUiEnabled(true);
  // 保留名检查：pattern 全字撞 "Fallback"/"fast_route" 的路由不生效（服务端路由时直接跳过），点名提醒改名
  let reservedName = '';
  for(const r of (c.routes || [])){
    if(!r) continue;
    for(const p of (Array.isArray(r.pattern) ? r.pattern : [r.pattern])){
      if(p === 'Fallback' || p === 'fast_route'){ reservedName = p; break; }
    }
    if(reservedName) break;
  }
  if(reservedName){
    document.getElementById('codexMsg').innerHTML = '<span class="err">⚠ pattern 不允许全字叫 "' + reservedName + '"（Codex 菜单的保留名：* 兜底路由=Fallback、fast 通道=fast_route），撞名的路由不生效，请改名</span>';
  }
  const derived = deriveCodexModels();
  if(derived !== null && derived.length &&
      derived.map(o => o.name).join('|') !== codexModelList.map(o => o.name).join('|')){
    codexModelList = derived;
    refreshCodexModels(codexModelList);
  }
  // 目录 = routes 各 pattern 的代表名全集 + 选中项（脚本侧还会并入已存在的目录条目）
  const cat = codexModelList.map(o => o.name);
  const sel = codexModelChoice();
  if(!cat.includes(sel)) cat.push(sel);
  // 上下文窗口/压缩阈值随命令烤进脚本（写进模型目录每个条目）；输入非法时回落默认，
  // 服务端另有 4096..2M / 1..99 的白名单兜底
  let ctx = parseInt(document.getElementById('codexCtx').value, 10);
  if(!(ctx >= 4096 && ctx <= 2000000)) ctx = 262144;
  let compact = parseInt(document.getElementById('codexCompact').value, 10);
  if(!(compact >= 1 && compact <= 99)) compact = 95;
  const cmds = codexScriptCmds('base=' + 'http://' + addr + '/v1' + '&model=' + sel + '&catalog=' + cat.join(',') + '&ctx=' + ctx + '&compact=' + compact);
  document.getElementById('codexCmdWin').textContent = cmds.win;
  document.getElementById('codexCmdSh').textContent = cmds.sh;
}
document.getElementById('cfg').addEventListener('input', genCodexCmds);
document.getElementById('codexModel').onchange = () => {
  document.getElementById('codexModelCustom').style.display =
    (document.getElementById('codexModel').value === '__custom__') ? '' : 'none';
  genCodexCmds();
};
document.getElementById('codexModelCustom').addEventListener('input', genCodexCmds);
document.getElementById('codexCtx').addEventListener('input', genCodexCmds);
document.getElementById('codexCompact').addEventListener('input', genCodexCmds);
function codexCopyCmd(id, hint){
  return async () => {
    try{
      await navigator.clipboard.writeText(document.getElementById(id).textContent);
      document.getElementById('codexMsg').innerHTML = '<span class="ok">已复制，' + hint + '</span>';
    }catch(e){ document.getElementById('codexMsg').innerHTML = '<span class="err">复制失败: '+e+'（可手动选中命令复制）</span>'; }
  };
}
document.getElementById('codexCopyWin').onclick = codexCopyCmd('codexCmdWin', '粘贴到 PowerShell 窗口回车即运行');
document.getElementById('codexCopySh').onclick = codexCopyCmd('codexCmdSh', '粘贴到终端回车即运行');

// ---- 配置文件切换 ----
document.getElementById('cfgSelect').onchange = async () => {
  const name = document.getElementById('cfgSelect').value;
  const sw = document.getElementById('cfgSwitchMsg');
  sw.innerHTML = '切换中…';
  try{
    const r = await fetch('/__switch',{method:'POST',headers:{'content-type':'application/json'},body:JSON.stringify({name})});
    if(r.ok){
      const d = await r.json();
      sw.innerHTML = '<span class="ok">已切换到 '+d.current+'</span>';
      loadConfig();
    } else {
      sw.innerHTML = '<span class="err">失败: '+await r.text()+'</span>';
      loadConfig();
    }
  }catch(e){
    sw.innerHTML = '<span class="err">失败: '+e+'</span>';
  }
};
document.getElementById('cfgRefreshBtn').onclick = async () => {
  try{
    const r = await fetch('/__configs',{cache:'no-store'});
    const d = await r.json();
    fillCfgLists(d);
    document.getElementById('cfgSwitchMsg').innerHTML = '<span class="ok">列表已刷新</span>';
  }catch(e){
    document.getElementById('cfgSwitchMsg').innerHTML = '<span class="err">刷新失败: '+e+'</span>';
  }
};

// ---- 配置文件 新建/重命名/删除 ----
function setSwitchMsg(cls, txt){ document.getElementById('cfgSwitchMsg').innerHTML = '<span class="'+cls+'">'+txt+'</span>'; }

document.getElementById('cfgNewBtn').onclick = async () => {
  const name = prompt('新建配置文件名（无需 .json 后缀）：');
  if(!name) return;
  setSwitchMsg('', '新建中…');
  try{
    const r = await fetch('/__newconfig',{method:'POST',headers:{'content-type':'application/json'},body:JSON.stringify({name:name})});
    if(r.ok){ const d = await r.json(); setSwitchMsg('ok', '已新建并切换到 '+d.current); loadConfig(); }
    else { setSwitchMsg('err', '失败: '+await r.text()); }
  }catch(e){ setSwitchMsg('err', '失败: '+e); }
};

document.getElementById('cfgRenameBtn').onclick = async () => {
  const sel = document.getElementById('cfgSelect');
  // 旧名用 prompt 输入（默认下拉值，可改），跟下拉选中解耦——理由同删除。
  const old = prompt('重命名哪个配置（输入文件名）：', sel.value);
  if(!old) return;
  const newName = prompt('将「'+old+'」重命名为（无需 .json 后缀）：');
  if(!newName) return;
  setSwitchMsg('', '重命名中…');
  try{
    const r = await fetch('/__renameconfig',{method:'POST',headers:{'content-type':'application/json'},body:JSON.stringify({old:old,new:newName})});
    if(r.ok){ const d = await r.json(); setSwitchMsg('ok', '已重命名，当前 '+d.current); loadConfig(); }
    else { setSwitchMsg('err', '失败: '+await r.text()); }
  }catch(e){ setSwitchMsg('err', '失败: '+e); }
};

// 删除配置：点「删除」弹出窗口选要删的（排除当前生效的，当前删不掉），
// 与切换下拉 cfgSelect 解耦：下拉切换会即时生效，不能把删除目标绑在它上面。
const delModal = document.getElementById('cfgDelModal');
const delSel = document.getElementById('cfgDelSelect');
const delMsg = document.getElementById('cfgDelMsg');
function delModalShow(){ delModal.style.display = 'flex'; }
function delModalHide(){ delModal.style.display = 'none'; }

document.getElementById('cfgDelBtn').onclick = async () => {
  try{
    const r = await fetch('/__configs',{cache:'no-store'});
    const d = await r.json();
    const deletable = (d.files||[]).filter(f => f !== d.current);
    delSel.innerHTML = deletable.map(f => '<option value="'+f+'">'+f+'</option>').join('');
    delSel.disabled = deletable.length === 0;
    delMsg.textContent = deletable.length === 0 ? '当前目录下没有可删除的配置' : '';
    delModalShow();
  }catch(e){ setSwitchMsg('err', '加载配置列表失败: '+e); }
};
delModal.onclick = (ev) => { if(ev.target === delModal) delModalHide(); }; // 点遮罩关闭
document.getElementById('cfgDelCancelBtn').onclick = delModalHide;
document.getElementById('cfgDelConfirmBtn').onclick = async () => {
  const name = delSel.value;
  if(!name){ delMsg.textContent = '请先选择要删除的配置'; return; }
  if(!confirm('确定删除「'+name+'」？此操作不可恢复。')) return;
  if(!confirm('再次确认：真的要删除「'+name+'」吗？')) return;
  delMsg.textContent = '删除中…';
  try{
    const r = await fetch('/__delconfig',{method:'POST',headers:{'content-type':'application/json'},body:JSON.stringify({name:name})});
    if(r.ok){ delModalHide(); setSwitchMsg('ok', '已删除 '+name); loadConfig(); }
    else { delMsg.textContent = '失败: '+await r.text(); }
  }catch(e){ delMsg.textContent = '失败: '+e; }
};

// ---- 清空统计（切换配置不再自动清，独立按钮）----
document.getElementById('resetStatsBtn').onclick = async () => {
  if(!confirm('确定清空所有累计统计？（含最近完成的流列表；在途流与流编号不清）')) return;
  try{
    const r = await fetch('/__resetstats',{method:'POST'});
    if(!r.ok) alert('失败: '+await r.text());
  }catch(e){ alert('失败: '+e); }
};

// ---- 设置保留完成流个数 N ----
// 轮询每 0.5s 用服务端值回填输入框（输入框无焦点时）。点「设置」会先触发 mousedown 把焦点
// 移出输入框，若轮询恰好插在 mousedown 与 click 之间，输入框被刷回旧值、click 读到旧值——
// 所以 mousedown 时先把值存下，click 用存下的值，杜绝这个竞态。
var fcCapPending = null;
document.getElementById('finishedCapBtn').onmousedown = function(){
  fcCapPending = document.getElementById('finishedCapInput').value;
};
document.getElementById('finishedCapBtn').onclick = async () => {
  var n = parseInt(fcCapPending !== null ? fcCapPending : document.getElementById('finishedCapInput').value, 10);
  fcCapPending = null;
  if(isNaN(n) || n < 0) n = 0;
  if(n > 200) n = 200;
  document.getElementById('finishedCapInput').value = n;
  var msg = document.getElementById('finishedCapMsg');
  msg.innerHTML = '<span style="color:#9a9a9a">设置中…</span>';
  try{
    const r = await fetch('/__finishedcap',{method:'POST',headers:{'content-type':'application/json'},body:JSON.stringify({n:n})});
    if(r.ok){ const d = await r.json(); msg.innerHTML = '<span class="ok">已设为 '+d.n+'</span>'; }
    else { msg.innerHTML = '<span class="err">失败: '+await r.text()+'</span>'; }
  }catch(e){ msg.innerHTML = '<span class="err">失败: '+e+'</span>'; }
};

// ---- 清空日志（仅内存缓冲，不影响 log_file）----
document.getElementById('clearLogsBtn').onclick = async () => {
  try{
    const r = await fetch('/__clearlogs',{method:'POST'});
    if(!r.ok) alert('失败: '+await r.text());
  }catch(e){ alert('失败: '+e); }
};

// ---- 关闭在途流内容查看（同时停止自动跟踪，避免下次 poll 又被打开）----
document.getElementById('flightViewClose').onclick = () => {
  // 关闭手选单流：清 selectedFlight 回到自动跟踪/隐藏状态，但不动 autoTrack 勾选
  selectedFlight = 0;
  flightEnded = false;
  lastRaw = '';
  flightViewWhat = 'resp';
  lastReqRaw = '';
  flightViewSide = 'up';
  flightViewTree = false;
  flightViewTreeManual = false;
  var fv = document.getElementById('flightView');
  fv.innerHTML = '';
  fv.style.gridTemplateColumns = '';
  updateFlightViewChrome();
  // 由轮询根据 autoTrack 决定显示 grid 或隐藏，这里不强制隐藏
};
// 自动跟踪最新流开关：勾选时清手选单流，回 grid；取消勾选则隐藏
document.getElementById('autoTrackChk').onchange = function(){
  autoTrack = this.checked;
  if(autoTrack){
    selectedFlight = 0;
    flightEnded = false;
    lastRaw = '';
    flightViewWhat = 'resp';
    lastReqRaw = '';
    flightViewSide = 'up';
    flightViewTree = false;
    flightViewTreeManual = false;
  }
  updateFlightViewChrome();
};
// 最多并排显示多少个在途流（1..20）
document.getElementById('maxFlightsInput').onchange = function(){
  var n = parseInt(this.value, 10);
  if(isNaN(n) || n < 1) n = 1;
  if(n > 20) n = 20;
  this.value = n;
  maxFlights = n;
};
document.getElementById('flightViewRawBtn').onclick = () => {
  flightViewRaw = !flightViewRaw;
  document.getElementById('flightViewRawBtn').textContent = flightViewRaw ? '显示解析' : '显示原始';
  var fv = document.getElementById('flightView');
  if(selectedFlight) return; // 单流视图里本按钮隐藏：渲染/流式原始 由「看…」按钮循环切换
  if(autoTrack){
    // 多流模式：用缓存的 autoRaws 重渲染每个 cell
    Array.from(fv.children).forEach(function(cell){
      var raw = autoRaws[cell.dataset.id];
      var pre = cell.querySelector('.cellPre');
      if(pre && raw !== undefined){
        if(flightViewRaw){ pre.textContent = raw; } else { var h = parseSSEHTML(raw); pre.innerHTML = h || '<span class="ss-empty">（未解析出内容，点「显示原始」查看）</span>'; }
      }
    });
  }
};
// ---- 查看内容四态切换（请求体 → 输出[渲染] → 输出[流式原始] → 输出[非流式] 循环；仅手选单流；请求体拉一次即静态，下载用同一缓存）----
document.getElementById('flightViewWhatBtn').onclick = async () => {
  if(!selectedFlight) return;
  var keepTree = flightViewTree; // 树开着则跟随内容重成树：新内容能成树就不退出交互
  if(flightViewWhat === 'asm'){
    // 输出[非流式] → 请求体：拉一次静态请求体
    flightViewWhat = 'req';
    updateFlightViewChrome();
    await fetchFlightViewSide();
    updateFlightViewChrome();
  } else {
    // 请求体 → 输出[渲染] → 输出[流式原始] → 输出[非流式]：同一份 lastRaw 三种渲染，未结束的流下一次 poll 继续刷新
    flightViewWhat = flightViewWhat==='req' ? 'resp' : (flightViewWhat==='resp' ? 'raw' : 'asm');
    updateFlightViewChrome();
    renderRespFromLastRaw();
  }
  // 树跟随/默认成树只覆盖 JSON 格式状态（见 jsonViewState）；切入 渲染/流式原始 文本视图时正常退出树
  if(jsonViewState()){
    if(keepTree || !flightViewTreeManual) await autoFlightTree();
  } else if(keepTree){ flightViewTree = false; updateFlightViewChrome(); }
};
// fetchFlightViewSide 按当前 请求体/输出 + 链路侧 拉一次查看区内容（看请求体/切链路侧共用）；
// 服务端缺侧回退时经 X-Proxy429-Side 头告知实际侧，回退提示显示在链路按钮旁。
async function fetchFlightViewSide(){
  if(!selectedFlight) return;
  var fv = document.getElementById('flightView');
  var note = document.getElementById('flightViewSideNote');
  if(flightViewWhat==='req'){
    fv.textContent = '加载中…';
    try{
      const r = await fetch('/__flightreq?id='+selectedFlight+'&side='+flightViewSide,{cache:'no-store'});
      if(r.ok){
        note.textContent = (r.headers.get('X-Proxy429-Side')||'up') !== flightViewSide ? '（该侧无记录，显示另一侧）' : '';
        lastReqRaw = await r.text();
        renderReqBody();
      } else {
        lastReqRaw = '';
        note.textContent = '';
        fv.textContent = '（'+await r.text()+'）'; // 404: stream does not exist or no request body recorded
      }
    }catch(e){ fv.textContent = '（请求体拉取失败）'; }
  } else {
    try{
      const r = await fetch('/__flight?id='+selectedFlight+'&side='+flightViewSide,{cache:'no-store'});
      if(r.ok){
        note.textContent = (r.headers.get('X-Proxy429-Side')||'up') !== flightViewSide ? '（该侧无记录，显示另一侧）' : '';
        lastRaw = await r.text();
        renderRespFromLastRaw();
      }
    }catch(e){}
  }
}
// 链路侧切换：上游侧=代理↔上游（请求体是实发上游的、输出是上游回来的原始流），
// 下游侧=下游↔代理（客户端发出/实际收到的）。输出视图立拉一次覆盖 lastRaw，
// 在途流随后由 poll 按新侧续刷；请求体视图立拉静态内容。
document.getElementById('flightViewSideBtn').onclick = async () => {
  if(!selectedFlight) return;
  flightViewSide = flightViewSide==='up' ? 'down' : 'up';
  document.getElementById('flightViewSideNote').textContent = '';
  updateFlightViewChrome();
  await fetchFlightViewSide();
  // 树开着则跟随切侧重成树；树没开但当前是 JSON 格式状态且未被手动钉住时默认成树；不能成树由 autoFlightTree 退回常规渲染并关状态
  if(flightViewTree || (!flightViewTreeManual && jsonViewState())) await autoFlightTree();
};
// ---- 交互式 JSON 树查看（按钮恒可用，不要求「储存完整结构体」；第一层默认展开，更深层点键懒展开）----
// 数据源：请求体视图用已拉取的 lastReqRaw（开关开启时即完整体）；输出视图该侧有完整副本时优先拉 full=1 完整版，
// 取不到（开关关闭/缺侧/已清空/拉取失败）就对手头浏览内容 lastRaw（前 256KB，截断尾事件按原文挂入）成树——
// 能显示在屏上的内容就能交互看。切链路侧/切「看…」状态时由 autoFlightTree 重调本函数，树跟随内容。
// 返回 true=已成树；false=内容不可成树（已把提示文案写进查看区，调用方据此退回常规渲染）。
async function renderFlightTree(){
  var fv = document.getElementById('flightView');
  var fl = flightFlags[selectedFlight] || {};
  var text = '', err = '';
  if(flightViewWhat==='req'){
    text = lastReqRaw;
    if((flightViewSide==='down' ? fl.rdt : fl.rt) === true) err = '（该流请求体只剩截断版（前 256KB），不是完整 JSON，无法交互查看）';
    else if(!text) err = '（该流未记录请求体，可点「看输出[渲染]」再点回重试）';
  } else {
    if((flightViewSide==='down' ? fl.hfd : fl.hf) === true){
      fv.textContent = '加载中…';
      try{
        const r = await fetch('/__flight?id='+selectedFlight+'&full=1&side='+flightViewSide,{cache:'no-store'});
        if(r.ok) text = await r.text();
      }catch(e){}
    }
    if(!text) text = lastRaw; // 无完整副本可用时退回手头浏览内容（截断版也能成树，覆盖已到部分）
    if(!text) err = '（该流暂无输出内容可交互查看）';
  }
  if(err){ fv.textContent = err; return false; }
  // 先按整个 JSON 解析；失败按状态分流：非流式视图把 SSE 事件流组装成最终对象（便于按结构看树），其余按事件数组解析。
  // 完整副本整理不出（如储存开关中途才开、副本缺头）时退回手头浏览内容再试一次——能显示在屏上的内容就能交互看。
  var tryTreeVal = function(t){
    if(!t) return null;
    try{ return JSON.parse(t); }catch(e){}
    if(flightViewWhat==='asm'){
      var ar = assembleSSEToObject(t);
      return ar ? ar.obj : null;
    }
    var arr = sseToJSONArray(t);
    return arr.length ? arr : null;
  };
  var val = tryTreeVal(text);
  if(val === null && flightViewWhat !== 'req' && text !== lastRaw) val = tryTreeVal(lastRaw);
  if(val === null){ fv.textContent = '（内容不是 JSON 也不是 SSE 事件流，无法交互查看）'; return false; }
  fv.innerHTML = '';
  fv.appendChild(jsonTreeRoot(val));
  return true;
}
// jsonViewState 判定当前查看状态的内容是否天然是 JSON：请求体（恒为 JSON 文本）；输出[非流式]（事件流组装出的最终
// JSON，仅流结束后默认成树——在途时保持实况文本）。输出[渲染]=可读文本、[流式原始]=逐字原文，是文本视图，不算 JSON 格式。
function jsonViewState(){ return flightViewWhat==='req' || (flightViewWhat==='asm' && flightEnded); }
// autoFlightTree 自动/跟随成树：内容能成树就交互看（树状态开），不能成树则退回当前 请求体/输出 的常规渲染并关掉树状态。
// 用于两处：「看…」四态切换、链路侧切换；手动点过「退出交互」的选择由 flightViewTreeManual 钉住，不走本函数。
async function autoFlightTree(){
  if(await renderFlightTree()){ flightViewTree = true; }
  else { flightViewTree = false; if(flightViewWhat==='req'){ renderReqBody(); } else renderRespFromLastRaw(); }
  updateFlightViewChrome();
}
document.getElementById('flightViewTreeBtn').onclick = async () => {
  if(!selectedFlight) return;
  flightViewTreeManual = true; // 手动钉住：本次选择内自动成树不再介入（重选流复位）
  if(flightViewTree){
    // 退出交互：回到当前 请求体/输出 的常规渲染
    flightViewTree = false;
    updateFlightViewChrome();
    if(flightViewWhat==='req'){ if(lastReqRaw) renderReqBody(); } else renderRespFromLastRaw();
    return;
  }
  flightViewTree = true;
  updateFlightViewChrome();
  await renderFlightTree();
};
// ---- 下载（仅「储存完整结构体」开启时按钮可见）：点击才拉取完整内容，Blob 落盘 ----
// saveBlobText 落盘文本：能解析成 JSON 则美化（pretty，2 空格缩进）存 .json，否则逐字原文存 .txt。
function saveBlobText(text, base){
  var ext = '.txt';
  try{
    text = JSON.stringify(JSON.parse(text), null, 2);
    ext = '.json';
  }catch(e){}
  var a = document.createElement('a');
  a.href = URL.createObjectURL(new Blob([text], {type:'application/octet-stream'}));
  a.download = base+ext;
  a.click();
  setTimeout(function(){ URL.revokeObjectURL(a.href); }, 1000);
}
document.getElementById('flightViewDlBtn').onclick = async () => {
  if(!selectedFlight) return;
  try{
    const r = await fetch('/__flightreq?id='+selectedFlight+'&side='+flightViewSide,{cache:'no-store'});
    if(!r.ok){ alert('下载失败: '+await r.text()); return; }
    // 文件名带实际返回侧（缺侧回退时按服务端告知的侧命名，不误导）
    saveBlobText(await r.text(), 'flight-'+selectedFlight+'-request-'+(r.headers.get('X-Proxy429-Side')||flightViewSide));
  }catch(e){ alert('下载失败: '+e); }
};
document.getElementById('flightViewDlOutBtn').onclick = async () => {
  if(!selectedFlight) return;
  try{
    const r = await fetch('/__flight?id='+selectedFlight+'&full=1&side='+flightViewSide,{cache:'no-store'});
    if(!r.ok){ alert('下载失败: '+await r.text()); return; }
    saveBlobText(await r.text(), 'flight-'+selectedFlight+'-output-'+(r.headers.get('X-Proxy429-Side')||flightViewSide));
  }catch(e){ alert('下载失败: '+e); }
};
// ---- 储存完整结构体开关：POST 服务端，失败回滚勾选 ----
document.getElementById('fullStoreChk').onchange = async function(){
  var want = this.checked;
  try{
    const r = await fetch('/__fullstore',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({enabled:want})});
    if(!r.ok) throw await r.text();
    fullStoreOn = want;
  }catch(e){ alert('设置失败: '+e); this.checked = !want; }
  updateFlightViewChrome();
};

// ---- 缓存命中明细：hover tooltip + 点击放大弹窗 ----
var latestModelStats = [];
var latestCacheObs = [];
var latestHeat = [];
var latestHeatBase = 0;
var heatMetric = 2; // 活跃度着色口径：0=请求次数 1=输入+输出 2=输出（默认）
var latestClsHits = 0;    // 分类器命中总量（classifiers：无论是否分流/改写都计）
var latestClsNoThink = 0; // 其中实际改写思考配置的次数（classifierNoThink：off 或 low）
var latestClsHitsCC = 0, latestClsHitsCodex = 0; // 分源命中：Claude Code / Codex
var latestClsRwCC = 0, latestClsRwCodex = 0;     // 分源思考改写（关思考或降 low）
function cacheHitPct(cr, input, cc){ const d=input+cr+(cc||0); return d>0 ? (cr*100/d).toFixed(1)+'%' : '-'; }
// cacheRowsHTML 生成明细表格；big=true 用大字号（弹窗用）
function cacheRowsHTML(big){
  if(!latestModelStats.length) return '<div style="color:#888">暂无数据</div>';
  var fs = big ? '14px' : '12px';
  var h = '<table style="border-collapse:collapse;width:100%;font-size:'+fs+'"><thead><tr style="color:#9a9a9a;text-align:left">'+
    '<th style="padding:3px 12px 3px 0">模型</th><th style="padding:3px 12px 3px 0;text-align:right">命中率</th>'+
    '<th style="padding:3px 12px 3px 0;text-align:right">命中token</th><th style="padding:3px 12px 3px 0;text-align:right">写入token</th>'+
    '<th style="padding:3px 12px 3px 0;text-align:right">未命中token</th><th style="padding:3px 0;text-align:right">输出token</th></tr></thead><tbody>';
  latestModelStats.forEach(function(m){
    h += '<tr style="color:#d4d4d4"><td style="padding:3px 12px 3px 0">'+esc(m.model)+'</td>'+
      '<td style="padding:3px 12px 3px 0;text-align:right">'+cacheHitPct(m.cacheRead, m.input, m.cacheCreation)+'</td>'+
      '<td style="padding:3px 12px 3px 0;text-align:right">'+fmtNum(m.cacheRead)+'</td>'+
      '<td style="padding:3px 12px 3px 0;text-align:right">'+fmtNum(m.cacheCreation||0)+'</td>'+
      '<td style="padding:3px 12px 3px 0;text-align:right">'+fmtNum(m.input)+'</td>'+
      '<td style="padding:3px 0;text-align:right">'+fmtNum(m.output)+'</td></tr>';
  });
  return h + '</tbody></table>';
}
function refreshCacheTip(){ document.getElementById('cacheTip').innerHTML = '<div style="color:#9a9a9a;margin-bottom:4px">缓存命中明细（按上游模型）</div>' + cacheRowsHTML(false); }
function refreshCacheModal(){
  var tCr=0, tIn=0, tCc=0;
  latestModelStats.forEach(function(m){ tCr+=m.cacheRead; tIn+=m.input; tCc+=(m.cacheCreation||0); });
  document.getElementById('cacheModalTotal').textContent = '总计 '+cacheHitPct(tCr, tIn, tCc);
  document.getElementById('cacheModalBody').innerHTML = cacheRowsHTML(true) + cacheObsHTML();
}
// 活跃度热图着色口径切换：0=请求次数 1=输入+输出 2=输出。
function heatSetMetric(m){
  heatMetric = m;
  for(var k=0;k<3;k++) document.getElementById('heatM'+k).style.borderColor = (k===m?'#39d353':'');
  document.getElementById('heatModalBody').innerHTML = heatHTML();
}
// 活跃度热图单元格取值：v = [请求数, 输出 token, 输入 token, 上行字节, 下行字节]。
function heatCellVal(v){
  if(heatMetric===1) return v[1]+v[2];
  if(heatMetric===2) return v[1];
  return v[0];
}
// 活跃度热图：32 列 x 6 行滚动窗口，列 = 15 分钟一格，行 = 8 小时；右下 = 当前格，左上 = 48 小时前，
// 每 15 分钟最旧一格被挤出，不按日历日翻页。颜色按所选口径的值相对窗口内最大值分 4 级（GitHub 配色）。
function heatHTML(){
  if(!latestHeat.length) return '<div style="color:#888">暂无数据</div>';
  var lvlColor = ['#2d333b','#0e4429','#006d32','#26a641','#39d353'];
  var nowSlot = Math.floor(Date.now()/1000/900);
  var maxVal = 0, i, r, c;
  for(i=0;i<latestHeat.length;i++) if(latestHeat[i] && heatCellVal(latestHeat[i])>maxVal) maxVal = heatCellVal(latestHeat[i]);
  var firstSlot = nowSlot-191; // 滚动窗口最旧一格；末格（右下）= 当前槽 nowSlot
  var labels = ['-48h','-40h','-32h','-24h','-16h','-8h']; // 每行起点距现在的小时数，语言中立
  var h = '<div style="display:grid;grid-template-columns:auto repeat(32,12px);gap:2px;align-items:center;font-size:11px;color:#888">';
  h += '<div></div>';
  for(c=0;c<32;c++) h += '<div style="text-align:center;white-space:nowrap">'+(c%8===0?('+'+(c/4)+'h'):'')+'</div>';
  for(r=0;r<6;r++){
    h += '<div style="padding-right:4px;white-space:nowrap">'+labels[r]+'</div>';
    for(c=0;c<32;c++){
      var slot = firstSlot + r*32 + c;
      var idx = slot - latestHeatBase;
      var v = (idx>=0 && idx<latestHeat.length) ? latestHeat[idx] : null;
      var val = v ? heatCellVal(v) : 0;
      var lvl = val===0 ? 0 : Math.max(1, Math.ceil(val/maxVal*4));
      var tip = '';
      if(v && (v[0]||v[1]||v[2]||v[3]||v[4])){
        var st = new Date(slot*900*1000), en = new Date((slot+1)*900*1000);
        var p2 = function(x){ return (x<10?'0':'')+x; };
        tip = (st.getMonth()+1)+'-'+p2(st.getDate())+' '+p2(st.getHours())+':'+p2(st.getMinutes())+'–'+p2(en.getHours())+':'+p2(en.getMinutes())+' · '+v[0]+' 请求 · 输入 '+fmtNum(v[2])+' · 输出 '+fmtNum(v[1])+' · ↑'+fmtBytes(v[3])+' ↓'+fmtBytes(v[4]);
      }
      h += '<div'+(tip?(' title="'+tip+'"'):'')+' style="width:12px;height:12px;border-radius:2px;background:'+lvlColor[lvl]+'"></div>';
    }
  }
  h += '</div>';
  return h;
}
// 实测缓存时间表（按上游 URL+模型）：数据来自代理侧对同会话相邻流的实测，内存态
function cacheObsHTML(){
  if(!latestCacheObs.length) return '';
  var h = '<div style="color:#9a9a9a;margin:14px 0 4px">实测缓存时间（按上游 URL+模型）</div>'+
    '<table style="border-collapse:collapse;width:100%;font-size:14px"><thead><tr style="color:#9a9a9a;text-align:left">'+
    '<th style="padding:3px 12px 3px 0">模型</th><th style="padding:3px 12px 3px 0">URL</th>'+
    '<th style="padding:3px 12px 3px 0;text-align:right">缓存时间 ≥</th>'+
    '<th style="padding:3px 12px 3px 0;text-align:right">≥形成<span class="thq" title="下界数值形成至今的时长（m:ss 递增）。下界数值发生变化才重新起算；只新增支撑观测、数值不变时不重置；无下界观测时为 -">?</span></th>'+
    '<th style="padding:3px 12px 3px 0;text-align:right">缓存时间 &lt;</th>'+
    '<th style="padding:3px 12px 3px 0;text-align:right">&lt;形成<span class="thq" title="上界数值形成至今的时长（m:ss 递增）。上界数值发生变化（含被新存活观测否决定作废重测）才重新起算；只新增支撑观测、数值不变时不重置；无上界观测时为 -">?</span></th>'+
    '<th style="padding:3px 0;text-align:right">观测<span class="thq" title="支撑当前上下界的观测条数。上下界交叉时（新观测否定旧界——上游缓存时间中途变化或被提前驱逐），被否一侧作废重测，观测次数同步归零重计">?</span></th></tr></thead><tbody>';
  latestCacheObs.forEach(function(o){
    h += '<tr style="color:#d4d4d4"><td style="padding:3px 12px 3px 0">'+esc(o.model)+'</td>'+
      '<td style="padding:3px 12px 3px 0">'+esc(o.url)+'</td>'+
      '<td style="padding:3px 12px 3px 0;text-align:right">'+(o.alive||'-')+'</td>'+
      '<td style="padding:3px 12px 3px 0;text-align:right">'+(o.aliveAge||'-')+'</td>'+
      '<td style="padding:3px 12px 3px 0;text-align:right">'+(o.dead||'-')+'</td>'+
      '<td style="padding:3px 12px 3px 0;text-align:right">'+(o.deadAge||'-')+'</td>'+
      '<td style="padding:3px 0;text-align:right">'+o.samples+'</td></tr>';
  });
  return h + '</tbody></table>';
}
// 事件委托到卡片容器：hover 显示 tooltip，click 打开放大弹窗
cardsEl.addEventListener('mouseover', function(e){ if(e.target.closest('#cacheCard')){ refreshCacheTip(); document.getElementById('cacheTip').style.display='block'; } });
cardsEl.addEventListener('mouseout', function(e){ if(e.target.closest('#cacheCard')){ document.getElementById('cacheTip').style.display='none'; } });
cardsEl.addEventListener('click', function(e){ if(e.target.closest('#cacheCard')){ refreshCacheModal(); document.getElementById('cacheModal').style.display='flex'; } });
// tooltip 跟随鼠标定位
document.addEventListener('mousemove', function(e){
  var tip = document.getElementById('cacheTip');
  if(tip.style.display !== 'none'){ tip.style.left = Math.min(e.clientX+12, window.innerWidth-560)+'px'; tip.style.top = (e.clientY+12)+'px'; }
  var rt = document.getElementById('retryTip');
  if(rt.style.display !== 'none'){ rt.style.left = Math.min(e.clientX+12, window.innerWidth-300)+'px'; rt.style.top = (e.clientY+12)+'px'; }
  var ct = document.getElementById('clsTip');
  if(ct.style.display !== 'none'){ ct.style.left = Math.min(e.clientX+12, window.innerWidth-560)+'px'; ct.style.top = (e.clientY+12)+'px'; }
});
// ---- 重试明细：hover tooltip + 点击放大弹窗（仿缓存命中）----
function retryRowsHTML(big){
  var rows = latestModelStats.filter(function(m){ return m.retries>0; });
  if(!rows.length) return '<div style="color:#888">暂无重试</div>';
  var fs = big ? '14px' : '12px';
  var h = '<table style="border-collapse:collapse;width:100%;font-size:'+fs+'"><thead><tr style="color:#9a9a9a;text-align:left">'+
    '<th style="padding:3px 12px 3px 0">模型</th><th style="padding:3px 0;text-align:right">重试次数</th></tr></thead><tbody>';
  rows.forEach(function(m){
    h += '<tr style="color:#d4d4d4"><td style="padding:3px 12px 3px 0">'+esc(m.model)+'</td><td style="padding:3px 0;text-align:right">'+m.retries+'</td></tr>';
  });
  return h + '</tbody></table>';
}
function refreshRetryTip(){ document.getElementById('retryTip').innerHTML = '<div style="color:#9a9a9a;margin-bottom:4px">重试明细（按路由目标模型）</div>' + retryRowsHTML(false); }
function refreshRetryModal(){
  var t=0;
  latestModelStats.forEach(function(m){ t+=m.retries; });
  document.getElementById('retryModalTotal').textContent = '总计 '+t;
  document.getElementById('retryModalBody').innerHTML = retryRowsHTML(true);
}
cardsEl.addEventListener('mouseover', function(e){ if(e.target.closest('#retryCard')){ refreshRetryTip(); document.getElementById('retryTip').style.display='block'; } });
cardsEl.addEventListener('mouseout', function(e){ if(e.target.closest('#retryCard')){ document.getElementById('retryTip').style.display='none'; } });
cardsEl.addEventListener('click', function(e){ if(e.target.closest('#retryCard')){ refreshRetryModal(); document.getElementById('retryModal').style.display='flex'; } });
// ---- 分类器明细：hover tooltip + 点击放大弹窗（仿重试）----
function clsRowsHTML(big){
  var fs = big ? '14px' : '12px';
  return '<table style="border-collapse:collapse;width:100%;font-size:'+fs+'">'+
    '<thead><tr style="color:#9a9a9a;text-align:left"><th style="padding:3px 12px 3px 0"></th><th style="padding:3px 12px 3px 0;text-align:right">Claude Code</th><th style="padding:3px 12px 3px 0;text-align:right">Codex</th><th style="padding:3px 0;text-align:right">合计</th></tr></thead><tbody>'+
    '<tr style="color:#d4d4d4"><td style="padding:3px 12px 3px 0">命中</td><td style="padding:3px 12px 3px 0;text-align:right">'+latestClsHitsCC+'</td><td style="padding:3px 12px 3px 0;text-align:right">'+latestClsHitsCodex+'</td><td style="padding:3px 0;text-align:right">'+latestClsHits+'</td></tr>'+
    '<tr style="color:#d4d4d4"><td style="padding:3px 12px 3px 0">思考改写</td><td style="padding:3px 12px 3px 0;text-align:right">'+latestClsRwCC+'</td><td style="padding:3px 12px 3px 0;text-align:right">'+latestClsRwCodex+'</td><td style="padding:3px 0;text-align:right">'+latestClsNoThink+'</td></tr>'+
    '</tbody></table>';
}
function refreshClsTip(){ document.getElementById('clsTip').innerHTML = '<div style="color:#9a9a9a;margin-bottom:4px">分类器明细（命中=无论是否分流/改写都计）</div>' + clsRowsHTML(false); }
function refreshClsModal(){
  document.getElementById('clsModalTotal').textContent = latestClsHits>0 ? '改写占比 '+Math.round(latestClsNoThink*100/latestClsHits)+'%' : '';
  document.getElementById('clsModalBody').innerHTML = clsRowsHTML(true);
}
cardsEl.addEventListener('mouseover', function(e){ if(e.target.closest('#clsCard')){ refreshClsTip(); document.getElementById('clsTip').style.display='block'; } });
cardsEl.addEventListener('mouseout', function(e){ if(e.target.closest('#clsCard')){ document.getElementById('clsTip').style.display='none'; } });
cardsEl.addEventListener('click', function(e){ if(e.target.closest('#clsCard')){ refreshClsModal(); document.getElementById('clsModal').style.display='flex'; } });
</script>
<script>
// ---- 界面语言（i18n）----
// 页面默认中文渲染；英文模式 = 服务端 renderLogViewerEN 已把静态文案与 JS 构建串翻成英文，
// 本脚本只负责 alert/confirm/prompt 里的中文（服务端够不着的浏览器弹窗）。
// 切换语言 POST /__uilang 写 program-settings.txt（程序设置，热生效），随后整页刷新。
var I18N = {
  '确定删除「': 'Delete "', '」？此操作不可恢复。': '"? This cannot be undone.',
  '再次确认：真的要删除「': 'Confirm again: really delete "', '」吗？': '"?',
  '删除中…': 'Deleting…', '已删除 ': 'Deleted ',
  '请先选择要删除的配置': 'Pick a config to delete first',
  '确定清空所有累计统计？（含最近完成的流列表；在途流与流编号不清）': 'Clear all cumulative stats? (including the finished-streams list; in-flight streams and stream numbering are kept)',
  '新建配置文件名（无需 .json 后缀）：': 'New config file name (no .json suffix needed):',
  '重命名哪个配置（输入文件名）：': 'Rename which config (enter file name):',
  '将「': 'Rename "', '」重命名为（无需 .json 后缀）：': '" to (no .json suffix needed):',
  '下载失败: ': 'Download failed: ', '设置失败: ': 'Setting failed: ',
  '失败: ': 'Failed: '
};
function t(s){
  if(document.documentElement.lang !== 'en') return s;
  if(I18N[s] !== undefined) return I18N[s];
  var r = s, hit = false;
  for (var k in I18N){ if(r.indexOf(k) >= 0){ r = r.split(k).join(I18N[k]); hit = true; } }
  return hit ? r : s;
}
var __alert = window.alert.bind(window), __confirm = window.confirm.bind(window), __prompt = window.prompt.bind(window);
window.alert = function(s){ return __alert(t(String(s))); };
window.confirm = function(s){ return __confirm(t(String(s))); };
window.prompt = function(s, d){ return __prompt(t(String(s)), d); };
(function(){
  var sel = document.getElementById('langSel');
  sel.value = document.documentElement.lang === 'en' ? 'en' : 'zh';
  sel.onchange = async function(){
    try{
      const r = await fetch('/__uilang', {method:'POST', headers:{'content-type':'application/json'}, body:JSON.stringify({lang: sel.value})});
      if(r.ok){ location.reload(); } else { alert('Failed: ' + await r.text()); }
    }catch(e){ alert('Failed: ' + e); }
  };
})();
</script>
</body>
</html>`

// enHTMLRepl static copy mapping (Chinese → English). Replacement runs by descending key length,
// so short words (e.g. 「缓存命中」) can't pre-emptively truncate long sentences (e.g. 「缓存命中明细（按真实上游模型）」).
var enHTMLRepl = [][2]string{
	{`<html lang="zh">`, `<html lang="en">`},
	{`<title>Proxy429 控制台</title>`, `<title>Proxy429 Console</title>`},
	{`<div class="tab active" data-tab="status">状态</div>`, `<div class="tab active" data-tab="status">Status</div>`},
	{`<div class="tab" data-tab="logs">日志</div>`, `<div class="tab" data-tab="logs">Logs</div>`},
	{`<div class="tab" data-tab="config">配置</div>`, `<div class="tab" data-tab="config">Config</div>`},
	{`关闭此标签页即隐藏 · 代理继续运行`, `Closing this tab only hides the console · the proxy keeps running`},
	{`>文档</button>`, `>Docs</button>`},
	{`>语言`, `>Language`},
	{`⚪ 连接中`, `⚪ Connecting`},
	{`⚠ 检测到系统代理已开启，但 NO_PROXY 环境变量未排除回环地址——遵循系统代理的客户端（如 Codex）会把发往 127.0.0.1 的请求交给代理服务器，表现为 503 且本代理收不到任何请求。修复：到「配置」页底部重跑一次 Codex 一键命令（脚本会自动修好），然后重启 Codex。`, `⚠ A system proxy is enabled, but the NO_PROXY environment variable does not exclude loopback — proxy-following clients (e.g. Codex) will hand requests for 127.0.0.1 to the proxy server (you get a 503 and this proxy sees nothing). Fix: re-run the Codex one-line setup command at the bottom of the Config tab (the script repairs it automatically), then restart Codex.`},
	{`>清空统计</button>`, `>Clear stats</button>`},
	{`保留完成流: `, `Keep finished: `},
	{`>设置</button>`, `>Set</button>`},
	{`title="开启后新开始的请求记录完整请求体与输出（不设 256KB 上限），流查看器出现「下载请求体/下载输出」按钮；关闭立即清空已存的完整副本，仅能浏览截断内容"`, `title="When on, new requests have their full request body and output recorded (no 256KB cap), and the stream viewer shows Download request/output buttons; turning it off immediately purges stored full copies, leaving only truncated content viewable"`},
	{`> 储存完整结构体</label>`, `> Store full payloads</label>`},
	{`在途流 <label`, `In-flight <label`},
	{`自动跟踪最新`, `Auto-track latest`},
	{`最多显示`, `Show at most`},
	{` 个</label>`, `</label>`},
	{`<th>字节</th>`, `<th>Bytes</th>`},
	{`<th>token 入/出<span class="thq" title="该流的总输入 / 输出 tokens（输入 = input + 缓存读 + 缓存写）。在途时显示头部/尾部已到达的数值，完成后才准确；未收到的一侧显示 -">?</span></th>`, `<th>token in/out<span class="thq" title="This stream's total input / output tokens (input = input + cache read + cache write). In flight it shows what the head/tail has delivered so far and is exact only once finished; a side never received shows -">?</span></th>`},
	{`<th>token 入/出<span class="thq" title="该流的总输入 / 输出 tokens（输入 = input + 缓存读 + 缓存写），完成后才准确；未收到的一侧显示 -（如被下游取消的流 [107k/-]）。鼠标悬停单元格显示该流字节数">?</span></th>`, `<th>token in/out<span class="thq" title="This stream's total input / output tokens (input = input + cache read + cache write), exact once finished; a side never received shows - (e.g. [107k/-] for a stream cancelled by the downstream). Hover a cell to see that stream's byte count">?</span></th>`},
	{`<th>状态</th>`, `<th>State</th>`},
	{`<th>总时间</th>`, `<th>Total</th>`},
	{`<th>状态码</th>`, `<th>HTTP</th>`},
	{`<th>缓存命中</th>`, `<th>Cache hit</th>`},
	{`<th>缓存年龄`, `<th>Cache age`},
	{`<th>首字</th>`, `<th>TTFT</th>`},
	{`<th>结束</th>`, `<th>End</th>`},
	{`<span id="flightViewKind">输出</span>`, `<span id="flightViewKind">Output</span>`},
	{`<div>流 #<span`, `<div>Stream #<span`},
	{`>看请求体</button>`, `>Request body</button>`},
	{`>显示原始</button>`, `>Show raw</button>`},
	{`>下载请求体</button>`, `>Download request</button>`},
	{`>下载输出</button>`, `>Download output</button>`},
	{`>交互式JSON</button>`, `>Interactive JSON</button>`},
	{`>关闭</button>`, `>Close</button>`},
	{`最近完成的流`, `Recently finished streams`},
	{` 行 · 滚轮翻历史，自动滚到底 `, ` lines · scroll for history, auto-scrolls to bottom `},
	{`（内存仅留最近 500 行，新的覆盖旧的；不会随时间堆积。落盘日志另看 log_file）`, `(memory keeps only the latest 500 lines, new overwrites old; no unbounded growth. On-disk logs: see log_file)`},
	{`>清空日志</button>`, `>Clear logs</button>`},
	{`缓存命中明细（按真实上游模型）`, `Cache-hit details (by real upstream model)`},
	{`重试明细（按路由目标模型）`, `Retry details (by routed target model)`},
	{`>分类器明细 <span`, `>Classifier details <span`},
	{`配置文件: `, `Config file: `},
	{`>刷新列表</button>`, `>Refresh list</button>`},
	{`>新建</button>`, `>New</button>`},
	{`>重命名</button>`, `>Rename</button>`},
	{`>删除</button>`, `>Delete</button>`},
	{`>保存并重载</button>`, `>Save & reload</button>`},
	{`>仅重载（不改动文件）</button>`, `>Reload only (file untouched)</button>`},
	{`>预览 lazyApply 生效后 JSON</button>`, `>Preview JSON after lazyApply</button>`},
	{`>复制到编辑框</button>`, `>Copy to editor</button>`},
	{`>关闭预览</button>`, `>Close preview</button>`},
	{`只读预览，不写入文件。展示每个路由类结构在补上 lazyApply 默认值后的样子；局部显式值优先，noLazyApply:true 的对象保持不变。search_fallback 只显示自身值，命中路由的 claudeSearch* 覆盖在运行时叠加。`, `Read-only preview; nothing is written. Shows each route-like object after lazyApply defaults are filled in: an explicit local value wins, and noLazyApply:true objects stay untouched. search_fallback shows only its own values; a matched route's claudeSearch* overlay is applied at runtime.`},
	{`'预览中…'`, `'Previewing…'`},
	{`'已按 lazyApply 展开'`, `'Expanded with lazyApply'`},
	{`已复制到编辑框（尚未保存）`, `Copied to the editor (not saved yet)`},
	{`Codex 一键配置 —— 下面两行命令按上方编辑框实时生成（地址取自 responses_listen，模型目录取自 routes 的 pattern），在对应终端粘贴回车即运行`, `Codex one-line setup — the two commands below are generated live from the editor above (address from responses_listen, model catalog from routes patterns); paste into the matching terminal and press Enter`},
	{`当前配置没有 responses_listen，Responses API 监听口未启用。要为本配置增加 Responses API 功能吗？`, `This config has no responses_listen — the Responses API listener is off. Add Responses API support to this config?`},
	{`>是，添加并保存</button>`, `>Yes, add & save</button>`},
	{`会在上方 JSON 的 "listen" 行后加一行 "responses_listen": "127.0.0.1:8081"（端口可改）并立即保存；监听口随保存即时启动，不用重启代理。`, `Adds a line "responses_listen": "127.0.0.1:8081" (port editable) after the "listen" line in the JSON above and saves immediately; the listener starts on save, no proxy restart needed.`},
	{`默认模型: `, `Default model: `},
	{`>自定义…</option>`, `>Custom…</option>`},
	{`oc.textContent = '自定义…'`, `oc.textContent = 'Custom…'`},
	{`（还原默认 Codex 配置）`, `(Restore default Codex config)`},
	{`orr.textContent = '（还原默认 Codex 配置）'`, `orr.textContent = '(Restore default Codex config)'`},
	{`placeholder="自定义模型名"`, `placeholder="Custom model name"`},
	{`上下文窗口: `, `Context window: `},
	{`压缩阈值%: `, `Compact threshold %: `},
	{`title="写进模型目录每个条目的上下文窗口（tokens）"`, `title="Written into every catalog entry as its context window (tokens)"`},
	{`title="上下文用到该百分比时 Codex 自动压缩（95 = 用到 95% 触发）"`, `title="Codex auto-compacts when context usage reaches this percentage (95 = trigger at 95%)"`},
	{`Windows（PowerShell）`, `Windows (PowerShell)`},
	{`macOS / Linux（终端）`, `macOS / Linux (Terminal)`},
	{`>复制</button>`, `>Copy</button>`},
	{`命令从本代理拉取按当前配置烤好的脚本并直接执行（免交互、免官方登录、token 占位——真实 key 由上面路由的 api 注入）；脚本先备份再改写 ~/.codex/config.toml 并写模型目录，下拉选「（还原默认 Codex 配置）」得到的命令可完全撤销。下拉列出 routes 每个 pattern 的一个代表名（route.model 能命中 pattern 时用真名，否则用去 * 的 pattern；纯 * 兜底路由固定叫 Fallback——它接住任意模型名，显示某个真实 model 名会误导）：这些名字全部写进 Codex 的 /model 菜单，选中项为默认模型；pattern 不允许全字叫 Fallback 或 fast_route（保留名，撞名的路由不生效也不进菜单）；配了 fast_route 且带 model 时菜单追加 fast_route 条目，选中即走 fast 通道（等效请求带 speed:"fast"）。上下文窗口与压缩阈值写进目录的每个条目（Codex 用到该百分比时自动压缩上下文，顶层 model_context_window 等覆盖键会被脚本清掉以免压过目录声明）。改了 responses_listen 点「保存并重载」后监听口即按新地址生效，命令跟着变。`, `The command pulls a script baked for the current config from this proxy and runs it directly (non-interactive, no official login, token placeholder — the real key is injected by the route's api above); the script backs up before rewriting ~/.codex/config.toml and the model catalog, and the command from choosing "(Restore default Codex config)" fully undoes it. The dropdown lists one representative name per routes pattern (the route's own model when it matches the pattern, otherwise the pattern without *; a pure * catch-all route is always called Fallback — it catches any model name, so showing a real model name would mislead): all these names go into Codex's /model menu, the selection being the default model; a pattern must not be exactly Fallback or fast_route (reserved names — colliding routes neither take effect nor enter the menu); with fast_route configured and carrying a model, the menu gets a fast_route entry that takes the fast lane when selected (equivalent to the request carrying speed:"fast"). Context window and compact threshold are written into every catalog entry (Codex auto-compacts context at that percentage; top-level override keys like model_context_window are removed by the script so they don't override the catalog). After changing responses_listen and clicking "Save & reload", the listener takes effect at the new address and the commands follow.`},
	{`选择要删除的配置（当前生效的配置不可删除）：`, `Pick a config to delete (the active one cannot be deleted):`},
	{`>取消</button>`, `>Cancel</button>`},
	{`>确认删除</button>`, `>Confirm delete</button>`},
	{`协议来源：橙 [Anthropic] = Anthropic 口原生流量、紫 [translate] = Responses 口翻译成 Anthropic。名后 [值] = 实际发给上游的思考配置最短形态，其颜色 = 词汇口径（思考是针对上游的：上游收到的都是 Anthropic 格式）——橙 = Anthropic thinking（直连原样或翻译映射后）。值：关 = thinking 关或 effort none/off；开 N = enabled+budget_tokens N；adaptive = 自适应无档；low/high/max 等档位词 = adaptive 的 effort；无 [值] = 请求体未带思考字段`, `Protocol: orange [Anthropic] = native Anthropic-port traffic, purple [translate] = Responses port translated to Anthropic. The [value] after the name = shortest form of the thinking config actually sent upstream; its color = vocabulary (thinking targets the upstream: upstreams always receive Anthropic format) — orange = Anthropic thinking (direct or translated). Values: off = thinking off or effort none/off; on N = enabled+budget_tokens N; adaptive = adaptive without level; low/high/max etc = effort of adaptive; no [value] = request carried no thinking field`},
	{`该会话+路由最近一次缓存写入距现在的时长（m:ss 递增）。显示方括号且数字冻结（如 [4:32]）时：同会话同路由有一条正在进行的流刚刷新了缓存，方括号内是刷新那一刻的年龄；该流完成后恢复递增（新锚）。上游缓存真实存活期是动态的——点击「缓存命中」卡片，弹窗内有各上游（按 URL+模型归类）实测的缓存存活时间可对照`, `Time since the most recent cache write for this session+route (m:ss, counting up). Brackets with a frozen number (e.g. [4:32]) mean an in-flight stream of the same session+route just refreshed the cache — the bracket shows the age at that refresh moment; counting resumes (new anchor) when it finishes. Real upstream cache lifetime is dynamic — click the Cache-hit card for measured lifetimes per upstream (grouped by URL+model)`},
	{`全局流式化 convertAlltoStream`, `Global stream-ification: convertAlltoStream`},
	{`Responses API 监听口 responses_listen`, `Responses API listener: responses_listen`},
	{`路由与能力兜底`, `Routing & capability fallbacks`},
	{`参数速查`, `Parameter cheat sheet`},
	{`pattern 顺序`, `Pattern order`},
	{`图片多模态`, `Image multimodality`},
	{`增强搜索 enhance_search`, `Enhanced search: enhance_search`},
	{`缓存命中`, `Cache hit`},
	{`统计字段`, `Stats fields`},
	{`流查看`, `Stream viewer`},
	{`配置管理`, `Config management`},
	{`访问控制`, `Access control`},
	{`>参数</th>`, `>Parameter</th>`},
	{`>配在哪儿</th>`, `>Where</th>`},
	{`>作用</th>`, `>Effect</th>`},
	{`>搭配 / 互斥</th>`, `>Pairs / conflicts</th>`},
	{`routes[] 条目`, `routes[] entry`},
	{`标记上游不支持图片`, `Marks the upstream as image-incapable`},
	{`标记上游不支持搜索`, `Marks the upstream as search-incapable`},
	{`支持搜索时对独立搜索子代理请求注入摘要指令（单请求模式）`, `When search is supported, inject the summary instruction into standalone search sub-agent requests (single-request mode)`},
	{`声明目标模型的思考形态：auto（默认，按客户端 model 名查表）/ adaptive / budget`, `Declares the target model's thinking shape: auto (default, looked up by client model name) / adaptive / budget`},
	{`图片兜底上游`, `Image fallback upstream`},
	{`搜索兜底上游`, `Search fallback upstream`},
	{`顶层`, `Top level`},
	{`'🟢 流式中'`, `'🟢 Streaming'`},
	{`'🟡 等待首字节'`, `'🟡 Waiting for first byte'`},
	{`'⚪ 空闲'`, `'⚪ Idle'`},
	{`'🔴 已断开（代理可能已退出）'`, `'🔴 Disconnected (the proxy may have exited)'`},
	{`('配置: '+d.currentCfg)`, `('Config: '+d.currentCfg)`},
	{`card('活跃/等待', d.active + ' / ' + d.waiting)`, `card('Active / Waiting', d.active + ' / ' + d.waiting)`},
	{`>活跃度</button>`, `>Activity</button>`},
	{`>活跃度</div>`, `>Activity</div>`},
	{`>请求次数</button>`, `>Requests</button>`},
	{`>输入+输出</button>`, `>In+Out</button>`},
	{`>输出</button>`, `>Output</button>`},
	{` 请求 · 输入 `, ` req · in `},
	{` · 输出 `, ` · out `},
	{`card('发给上游', fmtBytes(d.bytesUp))`, `card('To upstream', fmtBytes(d.bytesUp))`},
	{`card('上游返回', fmtBytes(d.bytesDown))`, `card('From upstream', fmtBytes(d.bytesDown))`},
	{`<div class="k">缓存命中</div>`, `<div class="k">Cache hit</div>`},
	{`card('输入', fmtNum(d.inputTokens))`, `card('Input', fmtNum(d.inputTokens))`},
	{`card('输出', fmtNum(d.outputTokens))`, `card('Output', fmtNum(d.outputTokens))`},
	{`<div class="k">重试</div>`, `<div class="k">Retries</div>`},
	{`<div class="k">分类器</div>`, `<div class="k">Classifier</div>`},
	{`card('首字', (d.avgFirstByte/1000).toFixed(2)+'s')`, `card('TTFT', (d.avgFirstByte/1000).toFixed(2)+'s')`},
	{`s = f.status?f.status:'响应';`, `s = f.status?f.status:'Response';`},
	{`s = '尝试'+(f.attempt||1);`, `s = 'Attempt '+(f.attempt||1);`},
	{`s = '路由';`, `s = 'Routing';`},
	{`else s = '请求';`, `else s = 'Request';`},
	{`return ['透传','pattern','分类器','fast','多模态','搜索'][r] || '透传';`, `return ['direct','pattern','classifier','fast','multimodal','search'][r] || 'direct';`},
	{`var tags = ['','[Pattern]','[分类器]','[Fast]','[多模态]','[搜索]'];`, `var tags = ['','[Pattern]','[Classifier]','[Fast]','[Multimodal]','[Search]'];`},
	{`return ' [退避中]';`, `return ' [backing off]';`},
	{`return ' [尝试'+(f.attempt||1)+': '+fmtStageDur(f.attemptMs)+']';`, `return ' [attempt '+(f.attempt||1)+': '+fmtStageDur(f.attemptMs)+']';`},
	{`'<span style="color:#f48771">[剥' + f.stripped + ']</span>'`, `'<span style="color:#f48771">[strip ' + f.stripped + ']</span>'`},
	{`'<span style="color:#f48771">[剥'+f.stripped+']</span>'`, `'<span style="color:#f48771">[strip '+f.stripped+']</span>'`},
	{`(f.gaveUp?'[重试尽]':`, `(f.gaveUp?'[retries exhausted]':`},
	{`[无stop]`, `[no-stop]`},
	{`[空搜]`, `[empty-search]`},
	{`'<span style="color:#d7ba7d">[重试'+(f.attempts-1)+'次]</span>'`, `'<span style="color:#d7ba7d">[retried '+(f.attempts-1)+'x]</span>'`},
	{`text: '[已编辑思考（加密）]'`, `text: '[redacted thinking (encrypted)]'`},
	{`text:'[已编辑思考（加密）]'`, `text:'[redacted thinking (encrypted)]'`},
	{`'加载中…'`, `'Loading…'`},
	{`>加载中…</pre>`, `>Loading…</pre>`},
	{`'（该流无透传内容：失败/重试用尽/非流式）'`, `'(No proxied content for this stream: failed / retries exhausted / non-streaming)'`},
	{`'-- 流已结束 --'`, `'-- stream ended --'`},
	{`show.length+' 个在途流'`, `show.length+' in-flight stream(s)'`},
	{`'（请求体拉取失败）'`, `'(failed to fetch the request body)'`},
	{`'（该流请求体只剩截断版（前 256KB），不是完整 JSON，无法交互查看）'`, `'(Only a truncated copy (first 256KB) of this stream request body remains — not complete JSON, cannot browse interactively)'`},
	{`'（该流未记录请求体，可点「看输出[渲染]」再点回重试）'`, `'(No request body recorded for this stream; click Output [rendered] then back to retry)'`},
	{`'（该流暂无输出内容可交互查看）'`, `'(No output content for this stream to browse interactively yet)'`},
	{`'（内容不是 JSON 也不是 SSE 事件流，无法交互查看）'`, `'(Content is neither JSON nor an SSE event stream; cannot browse interactively)'`},
	{`'该流请求体只剩截断版（前 256KB），完整版未记录或已清空'`, `'Only a truncated copy (first 256KB) of this stream request body remains; the full version was not recorded or was purged'`},
	{`'该流未记录完整输出（开启前已开始/已清空/无透传内容）'`, `'No full output recorded for this stream (started before the switch was on / purged / no proxied content)'`},
	{`flightViewWhat==='req' ? '看输出[渲染]' : (flightViewWhat==='resp' ? '看输出[流式原始]' : (flightViewWhat==='raw' ? '看输出[非流式]' : '看请求体'))`, `flightViewWhat==='req' ? 'Output [rendered]' : (flightViewWhat==='resp' ? 'Output [raw stream]' : (flightViewWhat==='raw' ? 'Output [non-stream]' : 'Request body'))`},
	{`flightViewWhat==='req' ? '请求体' : (flightViewWhat==='resp' ? '输出[渲染]' : (flightViewWhat==='raw' ? '输出[流式原始]' : '输出[非流式]'))`, `flightViewWhat==='req' ? 'Request body' : (flightViewWhat==='resp' ? 'Output [rendered]' : (flightViewWhat==='raw' ? 'Output [raw stream]' : 'Output [non-stream]'))`},
	{`'（无法整理：输出不是 JSON 也不是可识别的 SSE 事件流；切回 输出[流式原始] 查看）'`, `'(Cannot assemble: the output is neither JSON nor a recognizable SSE event stream; switch back to Output [raw stream])'`},
	{`'（流未完整到达，以下是已到部分的整理）\n'`, `'(Stream incomplete; the assembly below covers only what has arrived)\n'`},
	{`down ? (flightViewWhat==='req' ? '[下游→代理]' : '[代理→下游]') : (flightViewWhat==='req' ? '[代理→上游]' : '[上游→代理]')`, `down ? (flightViewWhat==='req' ? '[client→proxy]' : '[proxy→client]') : (flightViewWhat==='req' ? '[proxy→upstream]' : '[upstream→proxy]')`},
	{`链路:下游<->代理`, `Link: client<->proxy`},
	{`链路:代理<->上游`, `Link: proxy<->upstream`},
	{`（该侧无记录，显示另一侧）`, `(no data for that side, showing the other)`},
	{`flightViewTree ? '退出交互' : '交互式JSON'`, `flightViewTree ? 'Exit tree' : 'Interactive JSON'`},
	{`textContent = '输出';`, `textContent = 'Output';`},
	{`flightViewRaw ? '显示解析' : '显示原始'`, `flightViewRaw ? 'Parsed' : 'Raw'`},
	{`>暂无数据</div>`, `>No data yet</div>`},
	{`>暂无重试</div>`, `>No retries yet</div>`},
	{`>模型</th>`, `>Model</th>`},
	{`>命中率</th>`, `>Hit rate</th>`},
	{`>命中token</th>`, `>Hit tokens</th>`},
	{`>写入token</th>`, `>Written tokens</th>`},
	{`>未命中token</th>`, `>Missed tokens</th>`},
	{`>输出token</th>`, `>Output tokens</th>`},
	{`>重试次数</th>`, `>Retries</th>`},
	{`缓存命中明细（按上游模型）`, `Cache-hit details (by upstream model)`},
	{`实测缓存时间（按上游 URL+模型）`, `Measured cache lifetime (by upstream URL+model)`},
	{`>缓存时间 ≥</th>`, `>Cache alive ≥</th>`},
	{`>缓存时间 &lt;</th>`, `>Cache alive &lt;</th>`},
	{`>≥形成<span`, `>≥ age<span`},
	{`>&lt;形成<span`, `>&lt; age<span`},
	{`>观测<span`, `>Samples<span`},
	{`重试明细（按路由目标模型）</div>`, `Retry details (by routed target model)</div>`},
	{`分类器明细（命中=无论是否分流/改写都计）`, `Classifier details (hits count regardless of rerouting or thinking rewrites)`},
	{`'改写占比 '`, `'rewrite share '`},
	{`>命中</td>`, `>Hits</td>`},
	{`>思考改写</td>`, `>Thinking rewrites</td>`},
	{`>合计</th>`, `>Total</th>`},
	{`'总计 '`, `'Total '`},
	{`'保存中…'`, `'Saving…'`},
	{`'已保存并重载'`, `'Saved & reloaded'`},
	{`'重载中…'`, `'Reloading…'`},
	{`'已重载'`, `'Reloaded'`},
	{`>失败: '`, `>Failed: '`},
	{`'失败: '+`, `'Failed: '+`},
	{`>列表已刷新</span>`, `>List refreshed</span>`},
	{`>刷新失败: '`, `>Refresh failed: '`},
	{`prompt('新建配置文件名（无需 .json 后缀）：')`, `prompt('New config file name (no .json suffix needed):')`},
	{`prompt('重命名哪个配置（输入文件名）：', sel.value)`, `prompt('Rename which config (enter file name):', sel.value)`},
	{`prompt('将「'+old+'」重命名为（无需 .json 后缀）：')`, `prompt('Rename "'+old+'" to (no .json suffix needed):')`},
	{`'新建中…'`, `'Creating…'`},
	{`'已新建并切换到 '`, `'Created and switched to '`},
	{`'重命名中…'`, `'Renaming…'`},
	{`'已重命名，当前 '`, `'Renamed; active: '`},
	{`'切换中…'`, `'Switching…'`},
	{`>已切换到 '`, `>Switched to '`},
	{`'列表已刷新'`, `'List refreshed'`},
	{`'刷新失败: '`, `'Refresh failed: '`},
	{`'新建中…'`, `'Creating…'`},
	{`'已新建并切换到 '`, `'Created and switched to '`},
	{`'重命名中…'`, `'Renaming…'`},
	{`'已重命名，当前 '`, `'Renamed; active: '`},
	{`'加载配置列表失败: '`, `'Failed to load the config list: '`},
	{`'当前目录下没有可删除的配置'`, `'No deletable configs in this directory'`},
	{`'设置中…'`, `'Setting…'`},
	{`'已设为 '`, `'Set to '`},
	{`'请先选择要删除的配置'`, `'Pick a config to delete first'`},
	{`'确定删除「'`, `'Delete "'`},
	{`'」？此操作不可恢复。'`, `'"? This cannot be undone.'`},
	{`'再次确认：真的要删除「'`, `'Confirm again: really delete "'`},
	{`'」吗？'`, `'"?'`},
	{`'删除中…'`, `'Deleting…'`},
	{`'已删除 '`, `'Deleted '`},
	{`'确定清空所有累计统计？（含最近完成的流列表；在途流与流编号不清）'`, `'Clear all cumulative stats? (including the finished-streams list; in-flight streams and stream numbering are kept)'`},
	{`'下载失败: '`, `'Download failed: '`},
	{`'设置失败: '`, `'Setting failed: '`},
	{`'失败: '`, `'Failed: '`},
	{`<span style="color:#9a9a9a">设置中…</span>`, `<span style="color:#9a9a9a">Setting…</span>`},
	{`<span class="ok">已设为 '`, `<span class="ok">Set to '`},
	{`title="下界数值形成至今的时长（m:ss 递增）。下界数值发生变化才重新起算；只新增支撑观测、数值不变时不重置；无下界观测时为 -"`, `title="How long the lower-bound value has stood (m:ss, increasing). Re-timed only when the lower-bound value changes; adding supporting observations with an unchanged value does not reset it; shown as - when there is no lower-bound observation"`},
	{`title="上界数值形成至今的时长（m:ss 递增）。上界数值发生变化（含被新存活观测否决定作废重测）才重新起算；只新增支撑观测、数值不变时不重置；无上界观测时为 -"`, `title="How long the upper-bound value has stood (m:ss, increasing). Re-timed when the upper-bound value changes (including being invalidated and re-measured after a new surviving observation refutes it); adding supporting observations with an unchanged value does not reset it; shown as - when there is no upper-bound observation"`},
	{`title="支撑当前上下界的观测条数。上下界交叉时（新观测否定旧界——上游缓存时间中途变化或被提前驱逐），被否一侧作废重测，观测次数同步归零重计"`, `title="Number of observations supporting the current bounds. When the bounds cross (a new observation refutes the old bound — the upstream cache lifetime changed mid-way or entries were evicted early), the refuted side is discarded and re-measured, and its observation count resets to zero"`},
	{`>已复制，' + hint + '`, `>Copied — ' + hint + '`},
	{`>复制失败: '+e+'（可手动选中命令复制）`, `>Copy failed: '+e+' (select the command manually to copy)`},
	{`'粘贴到 PowerShell 窗口回车即运行'`, `'paste into a PowerShell window and press Enter'`},
	{`'粘贴到终端回车即运行'`, `'paste into a terminal and press Enter'`},
	{`'（Responses API 未启用：点上方「是，添加并保存」后这里才会生成命令）'`, `'(Responses API is off: commands appear here after you click "Yes, add & save" above)'`},
	{`'（同上）'`, `'(same as above)'`},
	{`⛔ <b>错误</b>`, `⛔ <b>Error</b>`},
	{`🔍 搜索结果`, `🔍 Search results`},
	{`'（请求体超长，仅保留前 256KB）\n'`, `'(Request body too long; only the first 256KB kept)\n'`},
	{`'（仅显示前 256KB，点「下载请求体」获取完整）\n'`, `'(Showing the first 256KB only; click "Download request" for the full body)\n'`},
	{`'（请求体非完整 JSON，按原文显示）\n'`, `'(Request body is not complete JSON; shown verbatim)\n'`},
	{`（未解析出内容，切到 输出[流式原始] 查看）`, `(Nothing parsed; switch to Output [raw stream] to view)`},
	{`（未解析出内容，点「显示原始」查看）`, `(Nothing parsed; click "Raw" to view)`},
	{`(isArr?' 项':' 键')`, `(isArr?' items':' keys')`},
	{`'<div style="color:#9a9a9a;margin-bottom:2px">流 #'`, `'<div style="color:#9a9a9a;margin-bottom:2px">Stream #'`},
	{`>配置 JSON 暂无法解析，命令保持上次有效内容</span>`, `>Config JSON is temporarily unparseable; commands keep the last valid content</span>`},
	{`⚠ pattern 不允许全字叫 "' + reservedName + '"（Codex 菜单的保留名：* 兜底路由=Fallback、fast 通道=fast_route），撞名的路由不生效，请改名`, `⚠ A pattern must not be exactly "' + reservedName + '" (reserved names in the Codex menu: * catch-all = Fallback, fast lane = fast_route) — a colliding route does not take effect; please rename it`},
	{`已添加并保存 responses_listen（端口可在上方 JSON 改），监听口已随保存启动，现在就能用下面的命令`, `responses_listen added & saved (port editable in the JSON above); the listener started on save — the commands below are ready to use`},
	{`找不到插入位置，请手动在配置 JSON 里加一行 "responses_listen": "127.0.0.1:8081",`, `Insertion point not found; please add a line "responses_listen": "127.0.0.1:8081", to the config JSON manually`},
	{`'（文件不存在，保存将创建）'`, `'(file does not exist; saving will create it)'`},
	{`>加载失败: '+e`, `>Load failed: '+e`},
	{`'⚠ pattern 不允许全字叫 "'`, `'⚠ A pattern must not be exactly "'`},
	{`（gpt-5*）`, ` (gpt-5*)`},
}

// enThinkRepl maps server-pushed thinking values (extractThinkMode) Chinese → English:
// "关"→off, "开 N"→on N. Values reach innerHTML via esc(), so replacement is safe.
var enThinkRepl = map[string]string{
	"[关]": "[off]",
	"[开":  "[on ",
}

// enReplacer is the English page replacer, built once at startup: handlers render pages concurrently,
// so sorting the pair table per request would race on the shared slice; strings.Replacer is immutable
// and safe for concurrent use. Keys are ordered by descending length, so short words (e.g. 「缓存命中」)
// can't pre-emptively truncate long sentences (e.g. 「缓存命中明细（按真实上游模型）」).
var enReplacer = func() *strings.Replacer {
	sorted := append([][2]string(nil), enHTMLRepl...)
	sort.Slice(sorted, func(i, j int) bool { return len(sorted[i][0]) > len(sorted[j][0]) })
	repl := make([]string, 0, len(sorted)*2+len(enThinkRepl)*2)
	for _, p := range sorted {
		repl = append(repl, p[0], p[1])
	}
	for zh, en := range enThinkRepl {
		repl = append(repl, zh, en)
	}
	return strings.NewReplacer(repl...)
}()

// renderLogViewerEN renders the Chinese master page into English;
// unmapped Chinese stays as-is (the browser-side i18n script backstops alert/confirm/prompt).
func renderLogViewerEN(page string) string {
	page = strings.Replace(page, "__DOC_BODY__", logViewerDocEN, 1)
	return enReplacer.Replace(page)
}
