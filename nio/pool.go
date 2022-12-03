package nio

import (
	"sync"
)

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

// Get a raw buffer with approximate size. Used by framer.
func GetDataBufferChunk(size int64) []byte {
	i := 0
	for ; i < len(dataChunkSizeClasses)-1; i++ {
		if size <= int64(dataChunkSizeClasses[i]) {
			return dataChunkPools[i].Get().([]byte)
		}
	}

	return make([]byte, size)
}

// Return a chunk to the pool.
// Called after write is completed or the buffer is no longer needed.
func PutDataBufferChunk(p []byte) {
	for i, n := range dataChunkSizeClasses {
		if len(p) == n {
			dataChunkPools[i].Put(p)
			return
		}
	}
	// odd buffers sizes will go to GC
}

// Old style buffer pool

var (
	// createBuffer to get a buffer. io.Copy uses 32k.
	bufferPoolCopy = sync.Pool{New: func() interface{} {
		return make([]byte, 16*64*1024) // 1M
	}}
)

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

func (bp *BufferPool) GetBuffer() *Buffer {
	br := bp.pool.Get().(*Buffer)
	br.off = 0
	br.end = 0
	return br
}
