package nio

// WIP: Attempt to replicate a bit the style of Tokyo buffers and use channels instead of
// blocking Read/Write with lots of copy.

//// IOEvent is a struct wrapping an IO event and associated data.
//type IOEvent struct {
//	// Event type
//	Type int
//
//	Frame Buffer
//
//	// Val is the high-level value ( proto, struct, etc ) associated with the event
//	Val interface{}
//}
//
//const (
//	IO_READ = 0
//	IO_READ_META
//	IO_READ_FIN
//	IO_READ_RST
//	IO_WRITE
//	IO_WRITE_DONE
//)

// WIP: better support for zero copy and buffer passing ('bucket brigade').
// Backed by the general-purpose Buffer.

//// WBuf is a write buffer, used to serialize user data.
////
//// It has an underlying buffer with prefix space that can be used by
//// the implementation to avoid copy.
////
//// Based on http2/databuffer
//type WBuf struct {
//	Buffer
//
//	// Reserved space at the start of the buffer.
//	// gRPC framing: 5 bytes
//	// H2 Data frame header: 9 (14)
//	// TLS data header: 5 ( + 20 trailer ) (19)
//	//
//	// TCP/IO: 24  - if the implementation is backed by a TUN using soft IP.
//	// IP4: 24
//	// IP6: 40
//	// Total: 67 or 82
//	Prefix int
//
//	// Callback when the buffer has been flushed and is no longer needed.
//	OnWrite func(*WBuf)
//
//	// A linked list is likely better.
//	//next *WBuf
//
//	// TODO: take into account the 'prefix' space in each chunk.
//
//	chunks   [][]byte
//	r        int   // next byte to read is chunks[0][r]
//	w        int   // next byte to write is chunks[len(chunks)-1][w]
//	size     int   // total buffered bytes
//	expected int64 // we expect at least this many bytes in future Write calls (ignored if <= 0)
//}
//
//// dataBuffer is an io.ReadWriter backed by a list of data chunks.
//// Each dataBuffer is used to read DATA frames on a single stream.
//// The buffer is divided into chunks so the server can limit the
//// total memory used by a single connection without limiting the
//// request body size on any single stream.
//
//var errReadEmpty = errors.New("read from empty dataBuffer")
//
//// Read copies bytes from the buffer into p.
//// It is an error to read when no data is available.
//func (b *WBuf) Read(p []byte) (int, error) {
//	if b.size == 0 {
//		return 0, errReadEmpty
//	}
//	var ntotal int
//	for len(p) > 0 && b.size > 0 {
//		readFrom := b.bytesFromFirstChunk()
//		n := copy(p, readFrom)
//		p = p[n:]
//		ntotal += n
//		b.r += n
//		b.size -= n
//		// If the first chunk has been consumed, advance to the next chunk.
//		if b.r == len(b.chunks[0]) {
//			PutDataBufferChunk(b.chunks[0])
//			end := len(b.chunks) - 1
//			copy(b.chunks[:end], b.chunks[1:])
//			b.chunks[end] = nil
//			b.chunks = b.chunks[:end]
//			b.r = 0
//		}
//	}
//	return ntotal, nil
//}
//
//func (b *WBuf) bytesFromFirstChunk() []byte {
//	if len(b.chunks) == 1 {
//		return b.chunks[0][b.r:b.w]
//	}
//	return b.chunks[0][b.r:]
//}
//
//// Len returns the number of bytes of the unread portion of the buffer.
//func (b *WBuf) Len() int {
//	return b.size
//}
//
//// Write appends p to the buffer.
//func (b *WBuf) Write(p []byte) (int, error) {
//	ntotal := len(p)
//	for len(p) > 0 {
//		// If the last chunk is empty, allocate a new chunk. Try to allocate
//		// enough to fully copy p plus any additional bytes we expect to
//		// receive. However, this may allocate less than len(p).
//		want := int64(len(p))
//		if b.expected > want {
//			want = b.expected
//		}
//		chunk := b.lastChunkOrAlloc(want)
//		n := copy(chunk[b.w:], p)
//		p = p[n:]
//		b.w += n
//		b.size += n
//		b.expected -= int64(n)
//	}
//	return ntotal, nil
//}
//
//func (b *WBuf) lastChunkOrAlloc(want int64) []byte {
//	if len(b.chunks) != 0 {
//		last := b.chunks[len(b.chunks)-1]
//		if b.w < len(last) {
//			return last
//		}
//	}
//	chunk := GetDataBufferChunk(want)
//	b.chunks = append(b.chunks, chunk)
//	b.w = 0
//	return chunk
//}

type BufParent interface {
	Release(*Frame)
}

// Frame is a view into a NetBuf (input) or WBuf (output), with associated info about the
// connection and an event type.
//
// Frames can be passed in channels.
type Frame struct {
	Buffer

	// If the read buffer has space in front of off.
	Prefix int

	parent BufParent
}

// NetBuf is a buffer used by the lowest level network layer for reading
// from a blocking or non-blocking source.
type NetBuf struct {
	Buffer

	frameCnt int
}

func (nb *NetBuf) Release() {

}

//// NewBufferReader returns a buffer associated with a reader. This is a top level
//// buffer, handling multiple input frames.
//func NewNetBuf(in io.Reader) *NetBuf {
//	buf1 := bufferPoolCopy.Get().([]byte)
//	return &NetBuf{Buffer: Buffer{buf: buf1, Reader: in}}
//}
