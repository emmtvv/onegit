package runner

import (
	"context"
	"strings"
	"sync"
	"time"
)

const (
	flushInterval   = time.Second
	heartbeatEvery  = 3 * time.Second
	maxChunk        = 256 << 10
	maxLogBytes     = 32 << 20
	partialLineWait = 64 << 10
)

// logStream buffers job output and ships it in chunks. Only complete lines
// are sent (unless a line grows very long), so a secret is never split
// across chunks and always gets masked.
type logStream struct {
	mu       sync.Mutex
	buf      []byte
	masks    []string
	seq      int
	sent     int
	dropped  bool
	lastSend time.Time
	send     func(seq int, data string) bool
	done     chan struct{}
	stopped  chan struct{}
}

func newLogStream(masks []string, send func(int, string) bool) *logStream {
	return &logStream{masks: masks, send: send, done: make(chan struct{}), stopped: make(chan struct{}), lastSend: time.Now()}
}

func (l *logStream) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.sent+len(l.buf) > maxLogBytes {
		if !l.dropped {
			l.buf = append(l.buf, "\n[log truncated: the job printed more than 32 MiB]\n"...)
			l.dropped = true
		}
		return len(p), nil
	}
	l.buf = append(l.buf, p...)
	return len(p), nil
}

func (l *logStream) header(name string) {
	_, _ = l.Write([]byte("\n##[step] " + name + "\n"))
}

// pump flushes periodically until close is called.
func (l *logStream) pump(ctx context.Context) {
	defer close(l.stopped)
	t := time.NewTicker(flushInterval)
	defer t.Stop()
	for {
		select {
		case <-l.done:
			l.flush(true)
			return
		case <-t.C:
			l.flush(false)
		}
	}
}

func (l *logStream) close() {
	close(l.done)
	<-l.stopped
}

func (l *logStream) flush(all bool) {
	l.mu.Lock()
	n := len(l.buf)
	if !all {
		if i := strings.LastIndexByte(string(l.buf), '\n'); i >= 0 {
			n = i + 1
		} else if len(l.buf) < partialLineWait {
			n = 0
		}
	}
	n = min(n, maxChunk)
	chunk := string(l.buf[:n])
	l.mu.Unlock()

	if chunk == "" {
		if time.Since(l.lastSend) >= heartbeatEvery {
			if l.send(-1, "") {
				l.lastSend = time.Now()
			}
		}
		return
	}
	for _, m := range l.masks {
		chunk = strings.ReplaceAll(chunk, m, "***")
	}
	if !l.send(l.seq, chunk) {
		return // keep the data; retry on the next tick
	}
	l.seq++
	l.lastSend = time.Now()
	l.mu.Lock()
	l.sent += n
	l.buf = l.buf[n:]
	more := len(l.buf) > 0
	l.mu.Unlock()
	if all && more {
		l.flush(true)
	}
}
