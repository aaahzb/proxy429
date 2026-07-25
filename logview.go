package main

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// 网页控制台挂载路径（与代理同端口，仅本机访问）。
// 选 /__ 前缀：Anthropic API 走 /v1/...，不冲突；Go DefaultServeMux 精确匹配优先于 / 通配。
const (
	logViewerPath = "/__logs"
	logDataPath   = "/__logs/data"
	configPath    = "/__config" // GET 取配置内容、POST 保存并重载
	reloadPath    = "/__reload" // POST 仅重载（不改动文件）
)

// isLocalRequest 限制只有本机浏览器能访问控制台。
// 即使代理 listen 在 0.0.0.0 暴露到内网，远程请求 /__* 也返回 403，避免日志/配置泄露。
func isLocalRequest(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return false
	}
	return host == "127.0.0.1" || host == "::1" || host == "localhost" || strings.HasPrefix(host, "127.")
}

// ---- 实时速率（字节/秒）----
// 控制台每 500ms 轮询一次 /__logs/data，用两次轮询间的 bytesForward 增量算速率。
// 状态保存在包级变量，跨请求延续。

var (
	rateMu        sync.Mutex
	prevRateBytes int64
	prevRateT     time.Time
	lastRate      int64
)

// computeRate 根据累计字节 b 与上次采样计算 bytes/s，并更新基线。
func computeRate(b int64) int64 {
	rateMu.Lock()
	defer rateMu.Unlock()
	now := time.Now()
	if prevRateT.IsZero() {
		prevRateBytes, prevRateT = b, now
		return lastRate
	}
	dt := now.Sub(prevRateT).Seconds()
	if dt > 0.05 {
		lastRate = int64(float64(b-prevRateBytes) / dt)
		prevRateBytes, prevRateT = b, now
	}
	return lastRate
}

// logViewerHandler 返回控制台 HTML 页（状态/日志/配置三标签）。
func logViewerHandler(w http.ResponseWriter, r *http.Request) {
	if !isLocalRequest(r) {
		http.Error(w, "forbidden (local only)", http.StatusForbidden)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(logViewerHTML))
}

// flightInfo 是单个在途流的展示信息（网页「状态」标签的表格用）。
type flightInfo struct {
	ID     uint64 `json:"id"`
	Model  string `json:"model"`
	Phase  int    `json:"phase"`  // 0=等待响应, 1=已收到响应（开始转发）
	Bytes  int64  `json:"bytes"`  // 已转发字节
	Status int    `json:"status"` // HTTP 状态码（phase=1 时有效）
}

// logData 是 /__logs/data 返回的 JSON：最近日志 + 全量状态计数 + 在途流列表。
type logData struct {
	Lines        []string     `json:"lines"`
	Active       int          `json:"active"`
	Waiting      int          `json:"waiting"`
	Version      string       `json:"version"`
	CacheRead    int64        `json:"cacheRead"`
	InputTokens  int64        `json:"inputTokens"`
	OutputTokens int64        `json:"outputTokens"`
	BytesForward int64        `json:"bytesForward"`
	Rate         int64        `json:"rate"` // bytes/s
	Retries      int64        `json:"retries"`
	Classifiers  int64        `json:"classifiers"`
	AvgFirstByte float64      `json:"avgFirstByte"` // ms
	Tps          float64      `json:"tps"`          // tok/s
	Flights      []flightInfo `json:"flights"`
}

// logDataHandler 返回最近 maxLogBuf 行日志和全量状态（JSON），供页面每 500ms 轮询。
func logDataHandler(w http.ResponseWriter, r *http.Request) {
	if !isLocalRequest(r) {
		http.Error(w, "forbidden (local only)", http.StatusForbidden)
		return
	}
	d := logData{Lines: logBuf.window(0, maxLogBuf), Version: Version}
	stats.mu.Lock()
	d.Active = stats.active
	d.Waiting = stats.waiting
	d.CacheRead = stats.cacheRead
	d.InputTokens = stats.inputTokens
	d.OutputTokens = stats.outputTokens
	stats.mu.Unlock()
	d.BytesForward = stats.bytesForward.Load()
	d.Rate = computeRate(d.BytesForward)
	d.Retries = stats.statusRetries.Load()
	d.Classifiers = stats.classifierRewrites.Load()
	d.AvgFirstByte, d.Tps = stats.recentLatency()

	// 在途流列表：按 ID 排序（snapshot 已排序），展示 model 与转发进度。
	for _, f := range flights.snapshot() {
		model := f.origModel
		if f.targetModel != "" && f.targetModel != f.origModel {
			model = f.origModel + " -> " + f.targetModel
		}
		d.Flights = append(d.Flights, flightInfo{
			ID:     f.id,
			Model:  model,
			Phase:  int(f.phase.Load()),
			Bytes:  f.bytes.Load(),
			Status: f.status,
		})
	}
	if d.Flights == nil {
		d.Flights = []flightInfo{}
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(d)
}

// configGetHandler 返回配置文件路径与原始内容（JSON），供「配置」标签载入编辑器。
func configGetHandler(w http.ResponseWriter, r *http.Request) {
	if !isLocalRequest(r) {
		http.Error(w, "forbidden (local only)", http.StatusForbidden)
		return
	}
	data, err := os.ReadFile(configFilePath)
	content := ""
	if err == nil {
		content = string(data)
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"path":    configFilePath,
		"content": content,
		"exists":  err == nil,
	})
}

// configPostHandler 保存编辑后的配置并重载。
// 先用 json.Unmarshal 校验是合法 JSON 且符合 Config 结构，校验通过才写盘——
// 避免把损坏的配置写到磁盘导致下次启动失败。写盘后调 reloadConfig 生效。
func configPostHandler(w http.ResponseWriter, r *http.Request) {
	if !isLocalRequest(r) {
		http.Error(w, "forbidden (local only)", http.StatusForbidden)
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "读取请求体失败: "+err.Error(), http.StatusBadRequest)
		return
	}
	// 先校验：合法 JSON 且能解析进 Config（未知字段忽略，类型错误会失败）。
	var probe Config
	if err := json.Unmarshal(body, &probe); err != nil {
		http.Error(w, "JSON 解析失败，未保存: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := os.WriteFile(configFilePath, body, 0644); err != nil {
		http.Error(w, "写入文件失败: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if err := reloadConfig(); err != nil {
		// 文件已保存但重载失败（极少见，因上面已校验过）：旧配置仍在跑，告知用户。
		http.Error(w, "已保存但重载失败: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
}

// reloadHandler 仅重载配置（不改动文件），供「仅重载」按钮使用。
func reloadHandler(w http.ResponseWriter, r *http.Request) {
	if !isLocalRequest(r) {
		http.Error(w, "forbidden (local only)", http.StatusForbidden)
		return
	}
	if err := reloadConfig(); err != nil {
		http.Error(w, "重载失败: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
}

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
  #log { white-space:pre-wrap; word-break:break-word; line-height:1.45; }
  #cfg { width:100%; min-height:60vh; background:#1e1e1e; color:#d4d4d4; border:1px solid #333; border-radius:4px; padding:8px; font:inherit; }
  button { background:#264f78; color:#d4d4d4; border:1px solid #3a6ea5; border-radius:3px; padding:5px 12px; cursor:pointer; font:inherit; }
  button:hover { background:#2f6090; }
  button.ghost { background:#333; border-color:#444; }
  #cfgMsg { margin-left:10px; }
  .ok { color:#4ec9b0; } .err { color:#c75; }
  #cfgPath { color:#6a6a6a; margin-bottom:8px; }
</style>
</head>
<body>
<div id="bar">
  <div class="tab active" data-tab="status">状态</div>
  <div class="tab" data-tab="logs">日志</div>
  <div class="tab" data-tab="config">配置</div>
  <span id="hint">关闭此标签页即隐藏 · 代理继续运行</span>
</div>

<div class="pane active" id="pane-status">
  <div><span class="st idle" id="status">● 连接中</span></div>
  <div class="cards" id="cards"></div>
  <div>在途流</div>
  <table id="flights"><thead><tr><th>#</th><th>model</th><th>阶段</th><th>字节</th><th>状态</th></tr></thead><tbody></tbody></table>
</div>

<div class="pane" id="pane-logs">
  <div style="margin-bottom:6px"><span id="count">0</span> 行 · 滚轮翻历史，自动滚到底</div>
  <pre id="log"></pre>
</div>

<div class="pane" id="pane-config">
  <div id="cfgPath"></div>
  <textarea id="cfg" spellcheck="false"></textarea>
  <div style="margin-top:8px">
    <button id="saveBtn">保存并重载</button>
    <button id="reloadBtn" class="ghost">仅重载（不改动文件）</button>
    <span id="cfgMsg"></span>
  </div>
</div>

<script>
const logEl = document.getElementById('log');
const stEl  = document.getElementById('status');
const cardsEl = document.getElementById('cards');
const flightsBody = document.querySelector('#flights tbody');
let stick = true;
let cfgLoaded = false;

// 标签切换
document.querySelectorAll('.tab').forEach(t => {
  t.onclick = () => {
    document.querySelectorAll('.tab').forEach(x => x.classList.remove('active'));
    document.querySelectorAll('.pane').forEach(x => x.classList.remove('active'));
    t.classList.add('active');
    document.getElementById('pane-' + t.dataset.tab).classList.add('active');
    if (t.dataset.tab === 'logs') stick = true, window.scrollTo(0, document.body.scrollHeight);
    if (t.dataset.tab === 'config' && !cfgLoaded) loadConfig();
  };
});

window.addEventListener('scroll', () => {
  stick = (window.innerHeight + window.scrollY) >= (document.body.scrollHeight - 30);
});

function fmtBytes(n){
  if(n<1024) return n+'B';
  if(n<1048576) return (n/1024).toFixed(1)+'KB';
  return (n/1048576).toFixed(2)+'MB';
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
    // 状态灯
    stEl.className = 'st ' + (d.active>0?'active':d.waiting>0?'wait':'idle');
    stEl.textContent = d.active>0?'● 流式中':d.waiting>0?'● 等待首字节':'● 空闲';
    // 统计卡片
    cardsEl.innerHTML =
      card('活跃', d.active) + card('等待', d.waiting) +
      card('流出', fmtBytes(d.bytesForward)) + card('速率', fmtBytes(d.rate)+'/s') +
      card('缓存命中', fmtNum(d.cacheRead)) + card('输入', fmtNum(d.inputTokens)) +
      card('输出', fmtNum(d.outputTokens)) + card('重试', d.retries) +
      card('分类器', d.classifiers) + card('首字', (d.avgFirstByte/1000).toFixed(2)+'s') +
      card('tok/s', d.tps.toFixed(1));
    // 在途流
    const fs = d.flights || [];
    flightsBody.innerHTML = fs.map(f =>
      '<tr><td>#'+f.id+'</td><td>'+f.model+'</td><td>'+(f.phase?'转发':'等待')+'</td><td>'+fmtBytes(f.bytes)+'</td><td>'+f.status+'</td></tr>'
    ).join('');
    // 日志
    logEl.textContent = (d.lines||[]).join('\n');
    document.getElementById('count').textContent = d.lines?d.lines.length:0;
    if(stick) window.scrollTo(0, document.body.scrollHeight);
  }catch(e){
    stEl.className='st dead';
    stEl.textContent='● 已断开（代理可能已退出）';
  }
}
poll();
setInterval(poll, 500);

// 配置标签
async function loadConfig(){
  try{
    const r = await fetch('/__config',{cache:'no-store'});
    const d = await r.json();
    document.getElementById('cfgPath').textContent = d.path + (d.exists?'':'（文件不存在，保存将创建）');
    document.getElementById('cfg').value = d.content || '';
    cfgLoaded = true;
  }catch(e){
    document.getElementById('cfgMsg').innerHTML = '<span class="err">加载失败: '+e+'</span>';
  }
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
</script>
</body>
</html>`
