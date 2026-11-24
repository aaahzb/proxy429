package recorder

import (
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// client has no total timeout (streaming responses can be long); first-byte timeout is Transport.ResponseHeaderTimeout
// (set in main from upstream_header_timeout_s); a timeout goes to case-0 resend. Retry pacing is governed by total_budget_s.
var Client = &http.Client{
	Timeout: 0,
}

// trafficRT counts upstream traffic for the 发给上游/上游返回 status cards at the single shared client's transport
// layer, so every upstream call is covered (main forwarding, search sub-requests, count_tokens probes; each retry
// attempt counts its own bytes): request-body bytes as sent (bytesUp), response-body bytes as read (bytesDown).
type TrafficRT struct{ Base http.RoundTripper }

func (t TrafficRT) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Body != nil {
		r2 := new(http.Request)
		*r2 = *req
		r2.Body = &countingReadCloser{rc: req.Body, add: func(n int64) { Stats.BytesUp.Add(n); HeatAdd(time.Now(), 0, 0, 0, n, 0) }}
		req = r2
	}
	resp, err := t.Base.RoundTrip(req)
	if err != nil || resp == nil {
		return resp, err
	}
	resp.Body = &countingReadCloser{rc: resp.Body, add: func(n int64) { Stats.BytesDown.Add(n); HeatAdd(time.Now(), 0, 0, 0, 0, n) }}
	return resp, nil
}

// countingReadCloser adds every read's byte count into add (upstream traffic stats); Close passes through.
type countingReadCloser struct {
	rc  io.ReadCloser
	add func(int64)
}

func (c *countingReadCloser) Read(p []byte) (int, error) {
	n, err := c.rc.Read(p)
	if n > 0 {
		c.add(int64(n))
	}
	return n, err
}

func (c *countingReadCloser) Close() error { return c.rc.Close() }

// throughputSample records one successful stream's streaming duration and output, windowed at stream end for the status row's token/s.
// First-token latency uses a separate []int64 ring buffer, windowed on the first body byte, so "first token" updates in real time without waiting for stream end.
type throughputSample struct {
	streamMs     int64 // Streaming duration: first byte -> last byte
	outputTokens int64 // This stream's output_tokens
}

// modelUsage is one real upstream model's cumulative token usage, aggregated by model name for the status page breakdown.
type modelUsage struct {
	cacheRead     int64 // Cache-hit tokens (cache_read_input_tokens)
	cacheCreation int64 // Cache-write tokens (cache_creation_input_tokens)
	input         int64 // Miss tokens (input_tokens)
	output        int64 // Output tokens
	retries       int64 // Retries triggered for this model (network errors/status codes/in-body errors)
}

// liveStats aggregates counters across all streams; the web console and tray status light read it for live state.
type liveStats struct {
	Mu                 sync.Mutex
	Active             int                    // Streams currently being passed through
	Waiting            int                    // Requests sent upstream, awaiting first byte (status light yellow)
	CacheRead          int64                  // Cumulative cache-hit tokens (cache_read_input_tokens)
	CacheCreation      int64                  // Cumulative cache-write tokens (cache_creation_input_tokens)
	InputTokens        int64                  // Cumulative input tokens
	OutputTokens       int64                  // Cumulative output tokens (sum of each stream's current accumulation, grows with streams)
	ModelStats         map[string]*modelUsage // Token usage aggregated by real upstream model name
	BytesUp            atomic.Int64           // Cumulative request-body bytes sent upstream (each retry attempt counted), for the 发给上游 card
	BytesDown          atomic.Int64           // Cumulative response-body bytes returned from upstream (counted as read), for the 上游返回 card
	StatusRetries      atomic.Int64           // Retries since startup (status codes/timeouts/network errors/in-body errors, +1 per retry)
	ClassifierRewrites atomic.Int64           // Classifier hits with thinking rewritten (off or low) since startup
	ClassifierHits     atomic.Int64           // Requests matching a classifier signature since startup: counted whether or not rerouted/rewritten

	// Per-source split of the two classifier counters above (Claude Code security monitor vs Codex guardian).
	ClassifierHitsCC        atomic.Int64 // Claude Code (security-monitor prefix) hits
	ClassifierHitsCodex     atomic.Int64 // Codex guardian (approval-assessment prefix) hits
	ClassifierRewritesCC    atomic.Int64 // Claude Code thinking rewrites (off or low)
	ClassifierRewritesCodex atomic.Int64 // Codex guardian thinking rewrites (off or low)

	sampleMu  sync.Mutex         // Guards the sliding windows below (separate from mu, so long streams don't hold the lock)
	fbSamples []int64            // First-token latency ring buffer (pushed on first byte; the status row's "first token" updates live)
	fbHead    int                // fbSamples next write position
	tpSamples []throughputSample // Throughput ring buffer (pushed at stream end), for token/s
	tpHead    int                // tpSamples next write position
	sampleCap int                // Ring buffer capacity (= RecentSampleWindow, shared by both windows)
}

var Stats liveStats

// resetSampleCap resets the sliding-window capacity and clears existing samples. Shared by main startup and reload:
// capacity follows the config, so ring-buffer indices must reset in sync.
func (s *liveStats) ResetSampleCap(cap int) {
	s.sampleMu.Lock()
	defer s.sampleMu.Unlock()
	s.fbSamples = nil
	s.fbHead = 0
	s.tpSamples = nil
	s.tpHead = 0
	s.sampleCap = cap
}

// addModelUsage adds one stream's token usage under its real upstream model name (called once at stream end).
func (s *liveStats) AddModelUsage(model string, in, cr, cc, out int64) {
	if model == "" || (in == 0 && cr == 0 && cc == 0 && out == 0) {
		return
	}
	s.Mu.Lock()
	defer s.Mu.Unlock()
	if s.ModelStats == nil {
		s.ModelStats = make(map[string]*modelUsage)
	}
	m := s.ModelStats[model]
	if m == nil {
		m = &modelUsage{}
		s.ModelStats[model] = m
	}
	m.input += in
	m.cacheRead += cr
	m.cacheCreation += cc
	m.output += out
}

// addModelRetry adds one retry under its real upstream model name (called on network error/status code/in-body error).
func (s *liveStats) AddModelRetry(model string) {
	if model == "" {
		return
	}
	s.Mu.Lock()
	defer s.Mu.Unlock()
	if s.ModelStats == nil {
		s.ModelStats = make(map[string]*modelUsage)
	}
	m := s.ModelStats[model]
	if m == nil {
		m = &modelUsage{}
		s.ModelStats[model] = m
	}
	m.retries++
}

// modelUsageEntry is a JSON snapshot entry of modelStats, feeding the web per-model cache-hit breakdown.
type ModelUsageEntry struct {
	Model         string `json:"model"`
	CacheRead     int64  `json:"cacheRead"`     // Cache-hit tokens
	CacheCreation int64  `json:"cacheCreation"` // Cache-write tokens (part of the hit-rate denominator)
	Input         int64  `json:"input"`         // Miss tokens
	Output        int64  `json:"output"`        // Output tokens
	Retries       int64  `json:"retries"`       // Retry count
}

// snapshotModelStats returns usage snapshots aggregated by model, sorted by total tokens (input+cacheRead+cacheCreation+output) descending.
func (s *liveStats) SnapshotModelStats() []ModelUsageEntry {
	s.Mu.Lock()
	defer s.Mu.Unlock()
	if len(s.ModelStats) == 0 {
		return nil
	}
	out := make([]ModelUsageEntry, 0, len(s.ModelStats))
	for name, m := range s.ModelStats {
		out = append(out, ModelUsageEntry{Model: name, CacheRead: m.cacheRead, CacheCreation: m.cacheCreation, Input: m.input, Output: m.output, Retries: m.retries})
	}
	sort.Slice(out, func(i, j int) bool {
		ti := out[i].Input + out[i].CacheRead + out[i].CacheCreation + out[i].Output
		tj := out[j].Input + out[j].CacheRead + out[j].CacheCreation + out[j].Output
		return ti > tj
	})
	return out
}

// pushFirstByte writes first-token latency into the ring buffer. Called on the first body byte,
// so the status row's "first token" updates the moment streaming starts, without waiting for the whole stream.
func (s *liveStats) PushFirstByte(firstByteMs int64) {
	s.sampleMu.Lock()
	defer s.sampleMu.Unlock()
	if s.sampleCap <= 0 {
		return
	}
	if len(s.fbSamples) < s.sampleCap {
		s.fbSamples = append(s.fbSamples, firstByteMs)
		return
	}
	s.fbSamples[s.fbHead] = firstByteMs
	s.fbHead = (s.fbHead + 1) % s.sampleCap
}

// pushThroughput writes streaming duration and output_tokens into the ring buffer; called at stream end, feeds token/s.
func (s *liveStats) PushThroughput(streamMs, outputTokens int64) {
	s.sampleMu.Lock()
	defer s.sampleMu.Unlock()
	if s.sampleCap <= 0 {
		return
	}
	sm := throughputSample{streamMs: streamMs, outputTokens: outputTokens}
	if len(s.tpSamples) < s.sampleCap {
		s.tpSamples = append(s.tpSamples, sm)
		return
	}
	s.tpSamples[s.tpHead] = sm
	s.tpHead = (s.tpHead + 1) % s.sampleCap
}

// recentLatency computes the average first-token latency (ms) and token/s over the last X samples (weighted throughput:
// ΣoutputTokens / ΣstreamDuration). First-token latency comes from fbSamples (windowed on first byte),
// token/s from tpSamples (windowed at stream end). Missing samples or zero total duration yield 0 for the respective field.
func (s *liveStats) RecentLatency() (avgFirstByteMs float64, tps float64) {
	s.sampleMu.Lock()
	defer s.sampleMu.Unlock()
	if len(s.fbSamples) > 0 {
		var sumFB int64
		for _, v := range s.fbSamples {
			sumFB += v
		}
		avgFirstByteMs = float64(sumFB) / float64(len(s.fbSamples))
	}
	var sumST, sumOut int64
	for _, sm := range s.tpSamples {
		sumST += sm.streamMs
		sumOut += sm.outputTokens
	}
	if sumST > 0 {
		tps = float64(sumOut) / (float64(sumST) / 1000.0)
	}
	return
}

// humanNum compresses large numbers to 1.2k / 3.40M forms for single-line display.
func HumanNum(n int64) string {
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

// humanBytes compresses byte counts to 2.3KB / 1.20MB forms for live traffic display.
func HumanBytes(n int64) string {
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

// cacheHitRate returns the cache hit rate, matching Claude Code's cache-hit algorithm:
// cache_read / (input + cache_read + cache_creation).
// The denominator is total input (fresh + hit + written): cache writes aren't hits but consume input; omitting them inflates the rate.
// A zero denominator (no usage data, e.g. non-streaming responses or unparsed usage) returns "-".
func CacheHitRate(cacheRead, input, cacheCreation int64) string {
	denom := input + cacheRead + cacheCreation
	if denom == 0 {
		return "-"
	}
	return fmt.Sprintf("%.1f%%", float64(cacheRead)*100/float64(denom))
}

// fmtMs formats milliseconds as a seconds string; ≤0 (unrecorded) returns "-".
func FmtMs(ms int64) string {
	if ms <= 0 {
		return "-"
	}
	return fmt.Sprintf("%.2fs", float64(ms)/1000)
}

// fmtFirstByte formats first-token latency milliseconds as a seconds string; ≤0 (unrecorded, e.g. error fallback streams) returns "-".
func FmtFirstByte(ms int64) string {
	return FmtMs(ms)
}

// fmtTps formats tok/s as a string; ≤0 (no output or unrecorded) returns "-".
func FmtTps(tps float64) string {
	if tps <= 0 {
		return "-"
	}
	return fmt.Sprintf("%.1f", tps)
}
