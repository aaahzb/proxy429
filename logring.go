package main

import (
	"sync"
)

// maxLogBuf is the log ring buffer's max line count, and the scrollback limit of the web "view logs" page.
const maxLogBuf = 500

// logRing keeps the most recent log lines, for redrawing the lower half of the screen on terminal resizes.
type logRing struct {
	mu    sync.Mutex
	lines []string
	head  int // Next write position
	len   int // Current stored count
}

var logBuf logRing

func (r *logRing) append(line string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.len < cap(r.lines) {
		r.lines = append(r.lines, line)
		r.len++
	} else {
		r.lines[r.head] = line
		r.head = (r.head + 1) % cap(r.lines)
	}
}

// length returns the number of stored log lines.
func (r *logRing) length() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.len
}

// clear drops all stored logs (keeping the underlying array's capacity).
func (r *logRing) clear() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lines = r.lines[:0]
	r.head = 0
	r.len = 0
}

// window returns n log lines starting offset lines up from the bottom (chronological, oldest to newest).
// offset=0 is equivalent to recent(n); offset>0 skips the newest offset lines before taking n.
func (r *logRing) window(offset, n int) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	total := r.len
	if total == 0 || n <= 0 {
		return nil
	}
	if offset >= total {
		offset = total - 1
	}
	end := total - offset // Window's right end (exclusive, counted from the oldest)
	start := end - n
	if start < 0 {
		start = 0
	}
	count := end - start
	if count <= 0 {
		return nil
	}
	out := make([]string, 0, count)
	capN := cap(r.lines)
	for i := start; i < end; i++ {
		idx := (r.head + i) % capN
		out = append(out, r.lines[idx])
	}
	return out
}

// logBufAppender only stores logs into the ring buffer (polled by the web 「查看日志」 page), writing to no stream.
// The program has no terminal UI (tray + web only); all logs go into the buffer for the web; whether they also land on disk is up to log_file.
type logBufAppender struct{}

func (logBufAppender) Write(p []byte) (int, error) {
	logBuf.append(string(p))
	return len(p), nil
}
