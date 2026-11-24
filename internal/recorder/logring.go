package recorder

import (
	"sync"
)

// maxLogBuf is the log ring buffer's max line count, and the scrollback limit of the web "view logs" page.
const MaxLogBuf = 500

// logRing keeps the most recent log lines, the bounded scrollback behind the web console's Logs tab.
type logRing struct {
	Mu    sync.Mutex
	Lines []string
	Head  int // Next write position
	Len   int // Current stored count
}

var LogBuf logRing

func (r *logRing) append(line string) {
	r.Mu.Lock()
	defer r.Mu.Unlock()
	if r.Len < cap(r.Lines) {
		r.Lines = append(r.Lines, line)
		r.Len++
	} else {
		r.Lines[r.Head] = line
		r.Head = (r.Head + 1) % cap(r.Lines)
	}
}

// clear drops all stored logs (keeping the underlying array's capacity).
func (r *logRing) Clear() {
	r.Mu.Lock()
	defer r.Mu.Unlock()
	r.Lines = r.Lines[:0]
	r.Head = 0
	r.Len = 0
}

// window returns n log lines starting offset lines up from the bottom (chronological, oldest to newest).
// offset=0 takes the newest n lines; offset>0 skips the newest offset lines before taking n.
func (r *logRing) Window(offset, n int) []string {
	r.Mu.Lock()
	defer r.Mu.Unlock()
	total := r.Len
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
	capN := cap(r.Lines)
	for i := start; i < end; i++ {
		idx := (r.Head + i) % capN
		out = append(out, r.Lines[idx])
	}
	return out
}

// logBufAppender only stores logs into the ring buffer (polled by the web 「查看日志」 page), writing to no stream.
// The program has no terminal UI (tray + web only); all logs go into the buffer for the web; whether they also land on disk is up to log_file.
type LogBufAppender struct{}

func (LogBufAppender) Write(p []byte) (int, error) {
	LogBuf.append(string(p))
	return len(p), nil
}
