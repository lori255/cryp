package procgroup

import "sync"

// TailBuffer bounds diagnostics captured from external tools. Commands may
// ignore log-level flags or emit repeated driver errors; retaining only the
// tail keeps useful failure context without allowing output to exhaust memory.
type TailBuffer struct {
	mu    sync.Mutex
	limit int
	buf   []byte
}

func NewTailBuffer(limit int) *TailBuffer {
	return &TailBuffer{limit: limit}
}

func (b *TailBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.limit <= 0 {
		return len(p), nil
	}
	if len(p) >= b.limit {
		b.buf = append(b.buf[:0], p[len(p)-b.limit:]...)
		return len(p), nil
	}
	b.buf = append(b.buf, p...)
	if len(b.buf) > b.limit {
		copy(b.buf, b.buf[len(b.buf)-b.limit:])
		b.buf = b.buf[:b.limit]
	}
	return len(p), nil
}

func (b *TailBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.buf)
}
