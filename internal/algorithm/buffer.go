package algorithm

import (
	"bytes"
	"sync"
)

const MaxOutputBytes = 1024 * 1024
const MaxInputBytes = 1024 * 1024

// boundedBuffer is safe to read while os/exec is copying stderr.
type boundedBuffer struct {
	mu       sync.Mutex
	buffer   bytes.Buffer
	limit    int
	exceeded chan struct{}
	once     sync.Once
}

func newBoundedBuffer(limit int) *boundedBuffer {
	return &boundedBuffer{limit: limit, exceeded: make(chan struct{})}
}

func (b *boundedBuffer) Write(data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := len(data)
	remaining := b.limit - b.buffer.Len()
	if n > remaining {
		data = data[:remaining]
		b.once.Do(func() { close(b.exceeded) })
	}
	_, _ = b.buffer.Write(data)
	return n, nil
}

func (b *boundedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.String()
}

func (b *boundedBuffer) overflowed() bool {
	select {
	case <-b.exceeded:
		return true
	default:
		return false
	}
}
