package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"math"
	"math/rand"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Config 是代理的配置结构，对应 config.json。
type Config struct {
	Listen                     string `json:"listen"`
	Upstream                   string `json:"upstream"`
	MaxRetries                 int    `json:"max_retries"`
	BaseDelayMs                int    `json:"base_delay_ms"`
	MaxDelayMs                 int    `json:"max_delay_ms"`
	TotalBudgetMs              int    `json:"total_budget_ms"`
	RetryStatusCodes           []int  `json:"retry_status_codes"`
	RespectRetryAfter          bool   `json:"respect_retry_after"`
	ClassifierThinkingDisabled bool   `json:"classifier_thinking_disabled"`
	ClassifierSystemPrefix     string `json:"classifier_system_prefix"`
	ClassifierMaxTokens        int    `json:"classifier_max_tokens"`      // 命中分类器后压 max_tokens 到此值；0 不压（推荐，避免 thinking 截断导致 Claude Code 收不到安全判断）
	UpstreamHeaderTimeoutMs    int    `json:"upstream_header_timeout_ms"` // 等上游首字节的最长时间；超时认为请求卡住、内部重发。0 用默认 70s
	LogRequestDetail           bool   `json:"log_request_detail"`
	LiveStats                  *bool  `json:"live_stats"` // 实时状态行开关，nil 默认开
}

var cfg atomic.Pointer[Config]

// configFilePath 是配置文件路径，main 启动时设置，托盘 reload 复用。
var configFilePath string

// loadConfig 读取并解析配置文件并应用默认值。main 启动与托盘 reload 共用。
func loadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, err
	}
	if c.ClassifierSystemPrefix == "" {
		c.ClassifierSystemPrefix = "You are a security monitor"
	}
	if c.UpstreamHeaderTimeoutMs <= 0 {
		c.UpstreamHeaderTimeoutMs = 70000 // 默认 70s：等上游首字节超时则内部重发
	}
	return &c, nil
}

// reloadConfig 重新读取配置文件并原子替换全局 cfg，同时清空所有累计统计，
// 效果等同于"关闭程序重新打开"的配置与统计（但不重新监听端口）。
func reloadConfig() {
	c, err := loadConfig(configFilePath)
	if err != nil {
		log.Printf("[重载] 失败: %v", err)
		return
	}
	cfg.Store(c)
	// 清空累计统计（active 保留：它反映当前活跃流数，不是累计值，清了会让在跑的流结束时 active-- 变 -1）。
	stats.mu.Lock()
	stats.cacheRead = 0
	stats.inputTokens = 0
	stats.outputTokens = 0
	stats.mu.Unlock()
	stats.bytesForward.Store(0)
	stats.statusRetries.Store(0)
	stats.classifierRewrites.Store(0)
	// 清屏并光标回原点，模拟"重启"的干净画面（VT 已在启动时启用，ANSI 清屏生效）。
	stderrMu.Lock()
	fmt.Fprint(os.Stderr, "\033[2J\033[3J\033[H")
	stderrMu.Unlock()
	log.Printf("[重载] 配置已重新加载，统计已清空: http://%s -> %s (最多重试 %d 次, 分类器关thinking=%v)",
		c.Listen, c.Upstream, c.MaxRetries, c.ClassifierThinkingDisabled)
}

// client 不设总超时（流式响应可能很长）；首字节超时由 Transport.ResponseHeaderTimeout 控制
// （main 里按 upstream_header_timeout_ms 设置），超时走情况0 重发。重试节奏靠 total_budget_ms 控制。
var client = &http.Client{
	Timeout: 0,
}

// ---- 实时状态行 ----
// 多个并发请求的 token 计数聚合到全局 stats，由独立 goroutine 每 100ms 原地刷新
// 一行（\r 回行首 + \033[K 清行尾），不新增日志行。log 输出经过 clearLineWriter，
// 写 log 前先清状态行，让日志往上滚、状态行始终停在最后一行。

// liveStats 是所有流的聚合计数器，statsPrinter 用它刷新状态行。
type liveStats struct {
	mu                 sync.Mutex
	active             int          // 当前透传中的流数量
	waiting            int          // 已发上游、等首字节的请求数（状态灯黄）
	cacheRead          int64        // 累计缓存命中 token（cache_read_input_tokens）
	inputTokens        int64        // 累计输入 token
	outputTokens       int64        // 累计输出 token（各流当前累积值之和，随流增长）
	bytesForward       atomic.Int64 // 累计已转发字节，流过程中实时增长（ARK 不在流中发 token，用它体现实时迸出）
	statusRetries      atomic.Int64 // 启动至今因状态码命中 retry_status_codes 而重试的次数（每重试一次 +1）
	classifierRewrites atomic.Int64 // 启动至今命中分类器请求并关 thinking 的次数
}

var stats liveStats

// stderrMu 串行化状态行刷新与 log 输出，避免两者在同一行交错。
var stderrMu sync.Mutex

// toInt64 把 JSON 解析出的数字（float64 / json.Number 等）安全转成 int64。
func toInt64(v interface{}) int64 {
	switch n := v.(type) {
	case float64:
		return int64(n)
	case int:
		return int64(n)
	case int64:
		return n
	case json.Number:
		if i, err := n.Int64(); err == nil {
			return i
		}
	}
	return 0
}

// humanNum 把大数字压成 1.2k / 3.40M 形式，便于在单行里显示。
func humanNum(n int64) string {
	if n < 0 {
		n = 0
	}
	switch {
	case n < 1000:
		return strconv.FormatInt(n, 10)
	case n < 1000000:
		return fmt.Sprintf("%.1fk", float64(n)/1000)
	default:
		return fmt.Sprintf("%.2fM", float64(n)/1000000)
	}
}

// humanBytes 把字节数压成 2.3KB / 1.20MB 形式，用于显示实时流量。
func humanBytes(n int64) string {
	if n < 0 {
		n = 0
	}
	switch {
	case n < 1024:
		return fmt.Sprintf("%dB", n)
	case n < 1024*1024:
		return fmt.Sprintf("%.1fKB", float64(n)/1024)
	default:
		return fmt.Sprintf("%.2fMB", float64(n)/(1024*1024))
	}
}

// isTerminal 判断文件是否是终端字符设备；非终端（重定向到文件）时不刷新状态行。
func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

// clearLineWriter 包装 stderr：每次写 log 前先清掉状态行，避免 log 和状态行粘在一起。
type clearLineWriter struct{ w io.Writer }

func (c *clearLineWriter) Write(p []byte) (int, error) {
	stderrMu.Lock()
	defer stderrMu.Unlock()
	if _, err := c.w.Write([]byte("\r\033[K")); err != nil {
		return 0, err
	}
	return c.w.Write(p)
}

// parseSSEStats 解析一行 SSE（data: {...}），把 usage 里的 token 数累加到全局 stats。
// output_tokens 是单流累积值，用增量（new - lastOutput）累加，避免多流互相覆盖。
func parseSSEStats(line []byte, lastOutput *int64) {
	s := bytes.TrimRight(line, "\r\n")
	if !bytes.HasPrefix(s, []byte("data:")) {
		return
	}
	payload := bytes.TrimSpace(bytes.TrimPrefix(s, []byte("data:")))
	if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
		return
	}
	var ev map[string]interface{}
	if json.Unmarshal(payload, &ev) != nil {
		return
	}
	var usage map[string]interface{}
	switch ev["type"] {
	case "message_start":
		if msg, ok := ev["message"].(map[string]interface{}); ok {
			usage, _ = msg["usage"].(map[string]interface{})
		}
	case "message_delta":
		usage, _ = ev["usage"].(map[string]interface{})
	}
	if usage == nil {
		return
	}
	stats.mu.Lock()
	defer stats.mu.Unlock()
	// cache_read / input_tokens 在 message_start 里一次性给出，
	// 直接累加（message_delta 一般不带这些字段，不会重复加）。
	// 注：ARK 的 usage 不含 cache_creation_input_tokens，故无"缓存写入"统计。
	stats.cacheRead += toInt64(usage["cache_read_input_tokens"])
	stats.inputTokens += toInt64(usage["input_tokens"])
	// output_tokens 是本流累积值：只把增量加到全局。
	if out := toInt64(usage["output_tokens"]); out > *lastOutput {
		stats.outputTokens += out - *lastOutput
		*lastOutput = out
	}
}

// statsPrinter 每 100ms 原地刷新一行聚合状态。
// 注意：ARK 只在流末尾的 message_delta 发一次 usage，流过程中 token 不变；
// 因此用「流出字节 + 字节速率」作为实时迸出指标，token 在流结束时更新为准确值。
func statsPrinter() {
	const interval = 100 * time.Millisecond
	prevBytes := int64(0)
	prevT := time.Now()
	for range time.Tick(interval) {
		stats.mu.Lock()
		active := stats.active
		cacheRead := stats.cacheRead
		input := stats.inputTokens
		output := stats.outputTokens
		stats.mu.Unlock()
		bytes := stats.bytesForward.Load()
		retries := stats.statusRetries.Load()
		classifiers := stats.classifierRewrites.Load()
		now := time.Now()
		dt := now.Sub(prevT).Seconds()
		rate := 0.0
		if dt > 0 {
			rate = float64(bytes-prevBytes) / dt
		}
		prevBytes = bytes
		prevT = now
		line := fmt.Sprintf("[流式] 活跃%d | 流出 %s (%s/s) | 缓存命中 %s | 输入 %s | 输出 %s | 重试 %d | 分类器 %d",
			active, humanBytes(bytes), humanBytes(int64(rate)), humanNum(cacheRead), humanNum(input), humanNum(output), retries, classifiers)
		stderrMu.Lock()
		fmt.Fprintf(os.Stderr, "\r\033[K%s", line)
		stderrMu.Unlock()
	}
}

// shouldRetry 判断给定状态码是否在重试名单里。
func shouldRetry(code int) bool {
	c := cfg.Load()
	for _, rc := range c.RetryStatusCodes {
		if rc == code {
			return true
		}
	}
	return false
}

// computeBackoff 计算本次重试前的等待时间：
// 优先听上游 Retry-After 头（秒），否则用指数退避 + 抖动。
func computeBackoff(resp *http.Response, attempt int) time.Duration {
	c := cfg.Load()
	if c.RespectRetryAfter && resp != nil {
		if ra := resp.Header.Get("Retry-After"); ra != "" {
			if secs, err := strconv.Atoi(ra); err == nil {
				return time.Duration(secs) * time.Second
			}
		}
	}
	backoff := float64(c.BaseDelayMs) * math.Pow(2, float64(attempt))
	if backoff > float64(c.MaxDelayMs) {
		backoff = float64(c.MaxDelayMs)
	}
	// 加抖动，避免同时重试造成惊群。
	jitter := backoff * 0.3 * rand.Float64()
	return time.Duration(backoff+jitter) * time.Millisecond
}

// copyHeaders 把 src 的头复制到 dst，跳过逐跳头和 Content-Length。
// Content-Length 必须跳过：改写请求体后长度会变，让 Go 按 body 重新计算。
func copyHeaders(dst, src http.Header) {
	for k, vv := range src {
		if k == "Connection" || k == "Keep-Alive" ||
			k == "Transfer-Encoding" || k == "Upgrade" ||
			k == "Content-Length" {
			continue
		}
		for _, v := range vv {
			dst.Add(k, v)
		}
	}
}

// peekHead 读取响应体开头：读到第一个 SSE 事件结束（空行）、或读满 maxBytes、
// 或遇到 EOF 为止。返回已读到的字节（这些字节已从 br 消费，转发时要补回去）。
func peekHead(br *bufio.Reader, maxBytes int) []byte {
	var buf bytes.Buffer
	for buf.Len() < maxBytes {
		line, err := br.ReadBytes('\n')
		buf.Write(line)
		if err != nil {
			break // EOF 或出错，返回已读部分
		}
		// SSE 事件之间用空行分隔：已经有内容且遇到空行，说明第一个事件读完了。
		if len(bytes.TrimRight(line, "\r\n")) == 0 && buf.Len() > 2 {
			break
		}
	}
	return buf.Bytes()
}

// headHasError 判断响应开头是不是一个错误事件（限流等）。
func headHasError(head []byte) bool {
	s := strings.ToLower(string(head))
	return strings.Contains(s, "event: error") ||
		strings.Contains(s, "event:error") ||
		strings.Contains(s, `"type":"error"`) ||
		strings.Contains(s, `"type": "error"`) ||
		strings.Contains(s, "rate_limit") ||
		strings.Contains(s, "overloaded") ||
		strings.Contains(s, "too many requests")
}

// firstLine 取字节流第一行（截断到 200 字符），用于日志。
func firstLine(b []byte) string {
	s := string(b)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 200 {
		s = s[:200]
	}
	return strings.TrimSpace(s)
}

// truncate 把字符串压成一行并截断到 n 字符，用于日志。
func truncate(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}

// fieldSpan 记录顶层 JSON object 中某字段的字节位置（基于原始 body，含引号/冒号定位）。
// keyStart/keyEnd 是 key（含双引号）的范围；valStart/valEnd 是 value 的字节范围。
type fieldSpan struct {
	name     string
	keyStart int
	keyEnd   int
	valStart int
	valEnd   int
}

// locateTopFields 流式解析顶层 JSON object，返回各字段在原 body 的字节位置（保留出现顺序）。
// 用 json.Decoder 的 Token 读 key、Decode 读 value 到 RawMessage，再结合 InputOffset +
// RawMessage 内容校准出 value 字节范围。只匹配顶层 key，不误伤嵌套对象中的同名字段。
// 定位失败时返回 ok=false，调用方据此放行原 body（转发不受影响）。
func locateTopFields(body []byte) ([]fieldSpan, bool) {
	dec := json.NewDecoder(bytes.NewReader(body))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return nil, false
	}
	var spans []fieldSpan
	for dec.More() {
		tk, err := dec.Token()
		if err != nil {
			return nil, false
		}
		key, ok := tk.(string)
		if !ok {
			return nil, false
		}
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return nil, false
		}
		valStart, valEnd, ok := locateValueRange(body, v, dec.InputOffset())
		if !ok {
			return nil, false
		}
		keyStart, keyEnd, ok := findKeySpan(body, valStart, key)
		if !ok {
			return nil, false
		}
		spans = append(spans, fieldSpan{name: key, keyStart: keyStart, keyEnd: keyEnd, valStart: valStart, valEnd: valEnd})
	}
	return spans, true
}

// locateValueRange 用 Decoder 的 InputOffset 推算 value 字节范围，并用 RawMessage 内容校准。
// InputOffset 在 Decode 后通常指向 value 结束位置，但可能因 scanp/尾部空白差几字节；
// 故在附近 ±8 字节内做精确匹配（body 切片 == RawMessage），匹配不到则判定定位失败。
func locateValueRange(body, v []byte, off int64) (valStart, valEnd int, ok bool) {
	approx := int(off)
	for d := 0; d <= 8; d++ {
		for _, end := range []int{approx + d, approx - d} {
			start := end - len(v)
			if start >= 0 && end >= 0 && end <= len(body) && bytes.Equal(body[start:end], v) {
				return start, end, true
			}
		}
	}
	return 0, 0, false
}

// findKeySpan 从 value 起始位置向前找 "key": 模式，定位 key（含双引号）的字节范围。
// 目标字段名均为无转义的简单标识符，故直接字面量匹配；LastIndex 取最近一次出现。
func findKeySpan(body []byte, valStart int, key string) (keyStart, keyEnd int, ok bool) {
	needle := []byte(`"` + key + `":`)
	end := valStart
	if end > len(body) {
		end = len(body)
	}
	idx := bytes.LastIndex(body[:end], needle)
	if idx < 0 {
		return 0, 0, false
	}
	return idx, idx + len(key) + 2, true // +2 = 起始" + key + 闭合"
}

// classifierSystemMatches 判断 system 字段（原始字节）是否以指定前缀开头。
// system 可能是字符串，也可能是 Anthropic 风格的 [{type,text}] 数组。
func classifierSystemMatches(body []byte, spans []fieldSpan, prefix string) bool {
	for _, s := range spans {
		if s.name != "system" {
			continue
		}
		sysRaw := body[s.valStart:s.valEnd]
		var str string
		if err := json.Unmarshal(sysRaw, &str); err == nil {
			return strings.HasPrefix(str, prefix)
		}
		var arr []map[string]interface{}
		if err := json.Unmarshal(sysRaw, &arr); err == nil {
			for _, m := range arr {
				if t, _ := m["text"].(string); strings.HasPrefix(t, prefix) {
					return true
				}
			}
		}
		return false
	}
	return false
}

// fieldDeleteRange 计算删除字段（含 key、:、value）的字节范围，并正确处理前后逗号：
// 字段后是 , 则连同尾逗号一起删；否则若字段前是 ,（跳过空白后）则从前逗号删起。
// 避免删除后留下 ,, / {, / ,} 等非法结构。
func fieldDeleteRange(body []byte, f *fieldSpan) (int, int) {
	start := f.keyStart
	end := f.valEnd
	if end < len(body) && body[end] == ',' {
		return start, end + 1 // 删到尾逗号后
	}
	// 字段后是 }（或空白+}）：向前找前一字段的尾逗号
	i := start - 1
	for i >= 0 && (body[i] == ' ' || body[i] == '\t' || body[i] == '\n' || body[i] == '\r') {
		i--
	}
	if i >= 0 && body[i] == ',' {
		return i, end
	}
	return start, end
}

// lastCloseBrace 返回 body 中最后一个 } 的位置（跳过 trailing 空白），找不到返回 -1。
func lastCloseBrace(body []byte) int {
	for i := len(body) - 1; i >= 0; i-- {
		if body[i] == '}' {
			return i
		}
		if body[i] != ' ' && body[i] != '\t' && body[i] != '\n' && body[i] != '\r' {
			return -1
		}
	}
	return -1
}

// applyClassifierEdits 在原 body 上做文本替换/删除/追加，只动目标字段，其余字节原样保留。
// thinking->{"type":"disabled"}、reasoning_effort->"none"、reasoning->删除、max_tokens->数字；
// 原不存在的字段追加到顶层闭合 } 之前。编辑按 start 升序基于原始 body 分段拼接，互不干扰。
func applyClassifierEdits(body []byte, spans []fieldSpan, maxTokens int) []byte {
	have := map[string]*fieldSpan{}
	for i := range spans {
		s := &spans[i]
		switch s.name {
		case "thinking", "reasoning_effort", "reasoning", "max_tokens":
			have[s.name] = s
		}
	}

	type edit struct {
		start, end int
		repl       []byte
	}
	var edits []edit

	// 替换/删除已存在的目标字段
	if r, ok := have["reasoning"]; ok {
		ds, de := fieldDeleteRange(body, r)
		edits = append(edits, edit{ds, de, nil})
	}
	if t, ok := have["thinking"]; ok {
		edits = append(edits, edit{t.valStart, t.valEnd, []byte(`{"type":"disabled"}`)})
	}
	if re, ok := have["reasoning_effort"]; ok {
		edits = append(edits, edit{re.valStart, re.valEnd, []byte(`"none"`)})
	}
	if maxTokens > 0 {
		if m, ok := have["max_tokens"]; ok {
			edits = append(edits, edit{m.valStart, m.valEnd, []byte(strconv.Itoa(maxTokens))})
		}
	}

	// 追加原 body 不存在的字段：插到顶层闭合 } 之前（保留原字段顺序，新字段在末尾）
	var appendFields []byte
	if _, ok := have["thinking"]; !ok {
		appendFields = append(appendFields, []byte(`"thinking":{"type":"disabled"}`)...)
	}
	if _, ok := have["reasoning_effort"]; !ok {
		if len(appendFields) > 0 {
			appendFields = append(appendFields, ',')
		}
		appendFields = append(appendFields, []byte(`"reasoning_effort":"none"`)...)
	}
	if maxTokens > 0 {
		if _, ok := have["max_tokens"]; !ok {
			if len(appendFields) > 0 {
				appendFields = append(appendFields, ',')
			}
			appendFields = append(appendFields, []byte(`"max_tokens":`+strconv.Itoa(maxTokens))...)
		}
	}
	if len(appendFields) > 0 {
		braceOff := bytes.IndexByte(body, '{')
		closeOff := lastCloseBrace(body)
		if braceOff < 0 || closeOff <= braceOff {
			return body
		}
		// object 为空（{ } 间只有空白）则插到 { 后；否则在 } 前加前导逗号插入
		empty := true
		for i := braceOff + 1; i < closeOff; i++ {
			if c := body[i]; c != ' ' && c != '\t' && c != '\n' && c != '\r' {
				empty = false
				break
			}
		}
		if empty {
			edits = append(edits, edit{braceOff + 1, braceOff + 1, appendFields})
		} else {
			edits = append(edits, edit{closeOff, closeOff, append([]byte{','}, appendFields...)})
		}
	}

	if len(edits) == 0 {
		return body
	}

	// 按 start 升序分段拼接（基于原始 body，编辑互不干扰）
	sort.Slice(edits, func(i, j int) bool { return edits[i].start < edits[j].start })
	var buf bytes.Buffer
	prev := 0
	for _, e := range edits {
		if e.start < prev {
			continue // 防御：编辑重叠则跳过
		}
		buf.Write(body[prev:e.start])
		buf.Write(e.repl)
		prev = e.end
	}
	buf.Write(body[prev:])
	return buf.Bytes()
}

// maybeRewriteClassifier 命中分类器请求时关掉 thinking，让分类快速返回。
// 用 bytes.Contains 预筛，正常请求（不含前缀子串）不做 JSON 解析，开销极低。
// 命中后用 json.Decoder 流式定位目标字段的字节位置，再做文本替换--不整体重序列化，
// 未改字段原样保留字节（含 key 顺序与格式），避免影响上游缓存命中。
func maybeRewriteClassifier(body []byte) []byte {
	c := cfg.Load()
	if !c.ClassifierThinkingDisabled || c.ClassifierSystemPrefix == "" {
		return body
	}
	if !bytes.Contains(body, []byte(c.ClassifierSystemPrefix)) {
		return body
	}
	spans, ok := locateTopFields(body)
	if !ok {
		return body
	}
	if !classifierSystemMatches(body, spans, c.ClassifierSystemPrefix) {
		return body
	}
	newBody := applyClassifierEdits(body, spans, c.ClassifierMaxTokens)
	if len(newBody) == len(body) && bytes.Equal(newBody, body) {
		return body
	}
	log.Printf("[改写] 命中分类器请求，关闭 thinking (body %d->%d字节)", len(body), len(newBody))
	stats.classifierRewrites.Add(1) // 实时状态行计数：分类器关 thinking 次数
	return newBody
}

// logRequestDetail 解析请求体打印 stream/tools/system 前缀，用于诊断分类器指纹。
// 仅在 log_request_detail=true 时调用，默认关闭。
func logRequestDetail(r *http.Request, body []byte) {
	var p map[string]interface{}
	if err := json.Unmarshal(body, &p); err != nil {
		log.Printf("[详情] %s %s (非JSON)", r.Method, r.URL.Path)
		return
	}
	stream, _ := p["stream"].(bool)
	tools := 0
	if arr, ok := p["tools"].([]interface{}); ok {
		tools = len(arr)
	}
	sysPrefix := ""
	switch s := p["system"].(type) {
	case string:
		sysPrefix = truncate(s, 60)
	case []interface{}:
		for _, blk := range s {
			if m, ok := blk.(map[string]interface{}); ok {
				if t, _ := m["text"].(string); t != "" {
					sysPrefix = truncate(t, 60)
					break
				}
			}
		}
	}
	log.Printf("[详情] %s %s stream=%v tools=%d sys=%q", r.Method, r.URL.Path, stream, tools, sysPrefix)
}

// forward 把响应透传给客户端：先补回已偷看的 head，再流式转发剩余内容。
// 转发同时按行解析 SSE 的 usage 字段，累加到全局 stats 供状态行显示。
func forward(w http.ResponseWriter, resp *http.Response, head []byte, br *bufio.Reader) {
	defer resp.Body.Close()
	copyHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)

	flusher, canFlush := w.(http.Flusher)

	// 计入活跃流，defer 退出时减一。
	stats.mu.Lock()
	stats.active++
	stats.mu.Unlock()
	defer func() {
		stats.mu.Lock()
		stats.active--
		stats.mu.Unlock()
	}()

	var lastOutput int64 // 本流 output_tokens 累积值，用于算增量
	// writeAndCount 转发一段字节并解析其中的 data: 行更新 stats。
	writeAndCount := func(data []byte) {
		if len(data) == 0 {
			return
		}
		w.Write(data)
		if canFlush {
			flusher.Flush()
		}
		stats.bytesForward.Add(int64(len(data))) // 实时流量（字节），每转发一段就涨
		for _, line := range bytes.Split(data, []byte("\n")) {
			parseSSEStats(line, &lastOutput)
		}
	}

	writeAndCount(head)
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			writeAndCount(line)
		}
		if err != nil {
			break
		}
	}
}

// handler 是所有请求的入口：缓冲请求体 → 命中分类器则关 thinking →
// 带重试地转发 → 流式透传响应。
// 重试三种情况：
//
//	A. 上游 HTTP 状态码本身就是 429/5xx；
//	B. 状态码 200，但错误藏在 SSE 响应体里（event:error / rate_limit 等）；
//	C. 正常响应，直接透传。
func handler(w http.ResponseWriter, r *http.Request) {
	c := cfg.Load()
	// 1. 缓冲请求体，以便重试时重放。
	body, err := io.ReadAll(r.Body)
	if err != nil {
		log.Printf("[请求] %s %s 读取body失败: %v", r.Method, r.URL.Path, err)
		http.Error(w, "read body failed", http.StatusBadGateway)
		return
	}
	r.Body.Close()

	if c.LogRequestDetail {
		logRequestDetail(r, body)
	}
	log.Printf("[请求] %s %s (body=%d字节) 来自 %s", r.Method, r.URL.Path, len(body), r.RemoteAddr)

	// 1.5 命中分类器请求就关掉 thinking，让分类快速返回。
	body = maybeRewriteClassifier(body)

	deadline := time.Now().Add(time.Duration(c.TotalBudgetMs) * time.Millisecond)

	for attempt := 0; attempt <= c.MaxRetries; attempt++ {
		// 2. 构造发往上游的请求。
		upReq, err := http.NewRequest(r.Method, c.Upstream+r.URL.Path, bytes.NewReader(body))
		if err != nil {
			log.Printf("[错误] 构造上游请求失败: %v", err)
			http.Error(w, "build request failed", http.StatusBadGateway)
			return
		}
		upReq.URL.RawQuery = r.URL.RawQuery
		copyHeaders(upReq.Header, r.Header)
		upReq.Header.Set("Accept-Encoding", "identity") // 关闭压缩，方便检查响应体
		upReq.Host = ""                                 // 让 Go 根据 Upstream 自动设置 Host

		// 已发上游、进入"等首字节"阶段：状态灯转黄（waiting++）。
		stats.mu.Lock()
		stats.waiting++
		stats.mu.Unlock()
		resp, err := client.Do(upReq)
		// 拿到响应（或超时/出错）即离开等待阶段。
		stats.mu.Lock()
		stats.waiting--
		stats.mu.Unlock()

		// 情况 0：网络层错误。
		if err != nil {
			log.Printf("[尝试 %d] 请求错误: %v", attempt+1, err)
			if attempt < c.MaxRetries && time.Now().Before(deadline) {
				wait := computeBackoff(nil, attempt)
				if !time.Now().Add(wait).After(deadline) {
					log.Printf("  → 等待 %v 后重试 (剩余预算 %v)", wait, time.Until(deadline).Round(time.Millisecond))
					time.Sleep(wait)
					continue
				}
			}
			// 超时/网络错误重试用尽：透传 503 给 Claude Code 自行重试（SDK 内置 5xx 重试）。
			log.Printf("  -> 超过超时等待时间或重试用尽，透传 503 给客户端自行处理: %v", err)
			http.Error(w, "upstream timeout/error: "+err.Error(), http.StatusServiceUnavailable)
			return
		}

		// 无条件打印状态码，方便诊断到底收到了什么。
		log.Printf("[尝试 %d] 上游响应状态码: %d", attempt+1, resp.StatusCode)

		// 情况 A：HTTP 状态码本身就要求重试（真正的 429/5xx）。
		if shouldRetry(resp.StatusCode) {
			if attempt < c.MaxRetries && time.Now().Before(deadline) {
				wait := computeBackoff(resp, attempt)
				if !time.Now().Add(wait).After(deadline) {
					log.Printf("  → 状态码 %d，等待 %v 后重试 (剩余预算 %v)", resp.StatusCode, wait, time.Until(deadline).Round(time.Millisecond))
					stats.statusRetries.Add(1) // 实时状态行计数：状态码重试次数
					resp.Body.Close()
					time.Sleep(wait)
					continue
				}
			}
			log.Printf("  → 次数或预算用尽，透传状态码 %d", resp.StatusCode)
			forward(w, resp, nil, bufio.NewReader(resp.Body))
			return
		}

		// 情况 B：状态码 200，但错误可能藏在响应体（SSE 流）里。
		br := bufio.NewReader(resp.Body)
		head := peekHead(br, 8192)

		if headHasError(head) {
			log.Printf("  → 状态码 200 但响应体含错误: %s", firstLine(head))
			if attempt < c.MaxRetries && time.Now().Before(deadline) {
				wait := computeBackoff(resp, attempt)
				if !time.Now().Add(wait).After(deadline) {
					log.Printf("  → 等待 %v 后重试 (剩余预算 %v)", wait, time.Until(deadline).Round(time.Millisecond))
					resp.Body.Close()
					time.Sleep(wait)
					continue
				}
			}
			log.Printf("  → 次数或预算用尽，透传体内错误")
			forward(w, resp, head, br)
			return
		}

		// 情况 C：正常响应，透传。
		log.Printf("  → 正常响应，透传 (共尝试 %d 次)", attempt+1)
		forward(w, resp, head, br)
		return
	}
}

func main() {
	configPath := flag.String("config", "config.json", "配置文件路径")
	flag.Parse()
	configFilePath = *configPath

	c, err := loadConfig(*configPath)
	if err != nil {
		log.Fatalf("读取 %s 失败: %v", *configPath, err)
	}
	cfg.Store(c)

	// 给上游请求设"首字节超时"：超过则认为请求卡住，内部重发（走情况0 重试）。
	// 用 ResponseHeaderTimeout 而非 client.Timeout，确保只限等首字节、不砍流式 Body。
	// 仅启动时设置一次；reload 不重建 Transport（避免与在跑请求并发改字段）。
	if c.UpstreamHeaderTimeoutMs > 0 {
		tr := http.DefaultTransport.(*http.Transport).Clone()
		tr.ResponseHeaderTimeout = time.Duration(c.UpstreamHeaderTimeoutMs) * time.Millisecond
		client.Transport = tr
	}

	// 实时状态行：未配置默认开，显式 live_stats=false 关闭；非终端（重定向到文件）自动关。
	liveStatsOn := true
	if c.LiveStats != nil {
		liveStatsOn = *c.LiveStats
	}
	liveStatsOn = liveStatsOn && isTerminal(os.Stderr)
	if liveStatsOn {
		if !enableVT() { // Windows 上启用控制台 VT；失败则 ANSI 原地刷新/清屏不生效
			log.Printf("[VT] 启用失败，实时状态行的 ANSI 刷新/清屏将不生效；若日志堆叠可关闭 live_stats")
		}
		// log 经过 clearLineWriter：写 log 前清状态行，让状态行始终停在最后一行。
		log.SetOutput(&clearLineWriter{w: os.Stderr})
		go statsPrinter()
	}

	log.Printf("代理启动: 监听 http://%s -> 转发到 %s (最多重试 %d 次, 分类器关thinking=%v, 实时状态行=%v)",
		c.Listen, c.Upstream, c.MaxRetries, c.ClassifierThinkingDisabled, liveStatsOn)
	go setupTray(*configPath) // Windows 托盘（最小化到托盘 + 右键菜单）；非 Windows 空操作
	http.HandleFunc("/", handler)
	log.Fatal(http.ListenAndServe(c.Listen, nil))
}
