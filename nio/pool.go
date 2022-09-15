package nio

import (
	"sync"
)

var bufSize = 32 * 1024

// Buffer pool - based on http2/databuffer and valyala/bytebufferpool

// Buffer chunks are allocated from a pool to reduce pressure on GC.
// The maximum wasted space per dataBuffer is 2x the largest size class,
// which happens when the dataBuffer has multiple chunks and there is
// one unread byte in both the first and last chunks. We use a few size
// classes to minimize overheads for servers that typically receive very
// small request bodies.
var (
	dataChunkSizeClasses = []int{
		1 << 10,
		2 << 10,
		4 << 10,
		8 << 10,
		16 << 10,
	}
	dataChunkPools = [...]sync.Pool{
		{New: func() interface{} { return make([]byte, 1<<10) }},
		{New: func() interface{} { return make([]byte, 2<<10) }},
		{New: func() interface{} { return make([]byte, 4<<10) }},
		{New: func() interface{} { return make([]byte, 8<<10) }},
		{New: func() interface{} { return make([]byte, 16<<10) }},
	}
)

func getDataBufferChunk(size int64) []byte {
	i := 0
	for ; i < len(dataChunkSizeClasses)-1; i++ {
		if size <= int64(dataChunkSizeClasses[i]) {
			break
		}
	}
	return dataChunkPools[i].Get().([]byte)
}

func putDataBufferChunk(p []byte) {
	for i, n := range dataChunkSizeClasses {
		if len(p) == n {
			dataChunkPools[i].Put(p)
			return
		}
	}
}

var (
	// createBuffer to get a buffer. io.Copy uses 32k.
	// experimental use shows ~20k max read with Firefox.
	bufferPoolCopy = sync.Pool{New: func() interface{} {
		return make([]byte, 16*64*1024) // 1M
	}}
)

// TODO: add an owner ( reader or conn)
func GetBuffer() *Buffer {
	return ioPool.GetBuffer()
}

var ioPool = &BufferPool{
	pool: sync.Pool{New: func() interface{} {
		// Should hold a TLS handshake message
		return &Buffer{
			buf: make([]byte, bufSize),
		}
	}},
}

type BufferPool struct {
	pool sync.Pool
}

func (*BufferPool) Read([]byte) (int, error) {
	return 0, nil
}

func (bp *BufferPool) GetBuffer() *Buffer {
	br := bp.pool.Get().(*Buffer)
	br.off = 0
	br.end = 0
	br.Reader = nil
	return br
}
