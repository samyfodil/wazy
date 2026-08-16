package wasmedge_test

import (
	"bytes"
	"sync"
)

// syncBuffer is a bytes.Buffer safe for the guest to write while the test
// reads, which the server case needs: the guest prints its port and then
// blocks in accept.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
