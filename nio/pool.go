package nio

import "sync"

var bufSize = 32 * 1024

var (
	// createBuffer to get a buffer. io.Copy uses 32k.
	// experimental use shows ~20k max read with Firefox.
	bufferPoolCopy = sync.Pool{New: func() interface{} {
		return make([]byte, 0, 32*1024)
	}}
)

var bufferedConPool = sync.Pool{New: func() interface{} {
	// Should hold a TLS handshake message
	return &Buffer{
		buf: make([]byte, bufSize),
	}
}}

func GetBuffer() *Buffer {
	br := bufferedConPool.Get().(*Buffer)
	br.off = 0
	br.end = 0
	br.owner = nil
	return br
}
