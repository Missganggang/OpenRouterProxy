package nodeclient

import (
	"bytes"
	"io"
	"sync"
)

type ptySession interface {
	io.ReadWriteCloser
	Resize(cols, rows int) error
}

// Both stdout and stderr may write concurrently. Preserve a bounded prefix while
// continuing to consume output so a verbose child cannot block on a full pipe.
type boundedOutput struct {
	mu        sync.Mutex
	buffer    bytes.Buffer
	truncated bool
}

func (w *boundedOutput) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := len(data)
	remaining := 8000 - w.buffer.Len()
	if remaining < len(data) {
		data = data[:remaining]
		w.truncated = true
	}
	w.buffer.Write(data)
	return n, nil
}
func (w *boundedOutput) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.truncated {
		return w.buffer.String() + "\n[output truncated]"
	}
	return w.buffer.String()
}
