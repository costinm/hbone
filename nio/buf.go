package nio

import (
	"encoding/binary"
	"io"
)

// Similar uses:
// - gvisor View and Prepandable, PacketBuffer - not sharing same buffer.
// - fasthttp - still copies from buf.Reader to frame payload.
// - https://github.com/cloudwego/netpoll -
// 		- nocopy.go: Next,Peek,Skip(n), Until(delim) - blocking, Release()
//    -   malloc(n), MallocAck, Flush

// - https://github.com/tidwall/evio
//      -
// - gnet
// - https://github.com/lesismal/nbio -> includes TLS !
//  	- buffer valid in onData, onReadBufferFree called after - but it can be no-op
//    - caller can recycle the buffer independently
//    - not clear how TLS layer is handling the buffers.
//    - after TLS, data frame buffer can be passed to h2, de-framed
//    - on write - need to prepend grpc, h2, tls headers.

// Buffer is a buffer associated with a stream that can be used to sniff data and to
// reuse the read buffers and frames.
//
// The Fill method will populate the buffer by doing one or more Read() operations, up to buffer size.
// Read will first return data from the buffer, and if buffer is empty will read directly from the source reader.
// The buffer can be used for parsing.
type Buffer struct {
	// b has end and capacity, set at creation to the size of the buffer
	// using end and off as pointers to data
	buf []byte

	// read so far from buffer. Unread data in off:End
	off int

	// last bytes with data in the buffer. bytes.Buffer uses len(buf)
	end int

	// WIP: avoid copy, linked list of buffers.
	//next *Buffer
	// WIP: ownership
	Reader io.Reader

	// prefix int
	// frames int
}

// NewBufferReader returns a buffer associated with a reader. This is a top level
// buffer, handling multiple input frames.
func NewBufferReader(in io.Reader) *Buffer {
	buf1 := bufferPoolCopy.Get().([]byte)
	return &Buffer{buf: buf1, Reader: in}
}

// Return a subset (view) of a real read buffer
func (b *Buffer) Frame(start, end int) *Buffer {
	return &Buffer{buf: b.buf, off: b.off + start, end: b.off + end}
}

// WriteUint32 adds a little endian uint32 to the buffer.
func (b *Buffer) WriteUnint32(i uint32) {
	b.Grow(4)
	binary.LittleEndian.PutUint32(b.buf[b.end:], i)
	b.end += 4
}

func (b *Buffer) SetUnint32(pos int, i uint32) {
	binary.LittleEndian.PutUint32(b.buf[pos:], i)
}

func (b *Buffer) WriteVarint(i int64) {
	b.Grow(8)
	c := binary.PutVarint(b.buf[b.end:], i)
	b.end += c
}

func (b *Buffer) WriteByte(d byte) {
	b.grow(1)
	b.buf[b.end] = d
	b.end++
}

func (b *Buffer) Write(p []byte) (n int, err error) {
	n = len(p)
	b.grow(n)
	copy(b.buf[b.end:], p)
	b.end += n
	return
}

func (b *Buffer) Out() []byte {
	return b.buf[b.end:cap(b.buf)]
}

// UpdateAppend should be called if any append operation may resize and replace
// the buffer - for example protobuf case.
func (b *Buffer) UpdateAppend(bout []byte) {
	// TODO: if buffer is different, recycle the old one
	b.buf = bout
	b.end = len(bout)
}

// ========= Buffer management

func (b *Buffer) Skip(count int) int {
	b.off += count
	if b.off >= b.end {
		skipped := b.end - b.off
		b.off = 0
		b.end = 0
		return skipped
	}
	return count
}

func (b *Buffer) Recycle() {
	b.Reader = ioPool
	ioPool.pool.Put(b)
}

// Grow enough for n additional bytes
func (b *Buffer) Grow(n int) {
	c := cap(b.buf)
	if c-b.end > n {
		return
	}
	buf := make([]byte, c*2)
	copy(buf, b.buf[b.off:b.end])
	b.buf = buf
	// TODO: recycle
	b.end = b.end - b.off
	b.off = 0
}

// Size return the number of unread bytes in the buffer.
func (b *Buffer) Size() int {
	if b == nil {
		return 0
	}
	return b.end - b.off
}

func (s *Buffer) Close() error {
	if s.buf != nil {
		bufferPoolCopy.Put(s.buf)
		s.buf = nil
	}
	if c, ok := s.Reader.(io.Closer); ok {
		return c.Close()
	}
	return nil
}

func (b *Buffer) IsEmpty() bool {
	if b == nil {
		return true
	}
	return b.off >= b.end
}

func (b *Buffer) grow(n int) {
	c := cap(b.buf)
	if c-b.end > n {
		return
	}
	if n < c*2 {
		n = c * 2
	}
	buf := make([]byte, n)
	copy(buf, b.buf[b.off:b.end])
	b.buf = buf
	b.end = b.end - b.off
	b.off = 0
}

func (b *Buffer) Compact() {
	if b.off == b.end {
		b.off = 0
		b.end = 0
		return
	}
	copy(b.buf, b.buf[b.off:b.end])
	b.end = b.end - b.off
	b.off = 0
}

// Return the unread portion of the buffer
func (b *Buffer) Bytes() []byte {
	return b.buf[b.off:b.end]
}

// ========= Read support: will move end and possibly grow.

// Peek returns the next n bytes without advancing the reader. The bytes stop
// being valid at the next read call. If Peek returns fewer than n bytes, it
// also returns an error explaining why the read is short.
//
// Unlike bufio.Reader, if n is larger than buffer size the buffer is resized.
//
// Peek ensures at least i bytes are read. Blocking.
//
// Returns the buffer with all readable data, may be more than i
// If i==0, does one Read.
func (s *Buffer) Peek(i int) ([]byte, error) {
	if i == 0 {
		if cap(s.buf)-s.end < 1024 {
			s.grow(1024)
		}
		n, err := s.Reader.Read(s.buf[s.end:cap(s.buf)])
		s.end += n
		if err != nil {
			return s.buf[s.off:s.end], err
		}
		return s.buf[s.off:s.end], nil
	}

	if i > cap(s.buf)-s.end {
		s.grow(i)
	}

	// We have data
	if s.end-s.off >= i {
		return s.buf[s.off:s.end], nil
	}

	// Fill
	for {
		n, err := s.Reader.Read(s.buf[s.end:cap(s.buf)])
		s.end += n
		if err != nil {
			return s.buf[s.off:s.end], err
		}
		if s.end-s.off >= i {
			return s.buf[s.off:s.end], nil
		}
	}
}

// Discard will move the start with n bytes.
// TODO: if n > buffer, blocking read. Currently not used in the code.
func (b *Buffer) Discard(n int) {
	if n > b.Size() {
		n -= b.Size()
		b.off = 0
		b.end = 0
		// Now need to read and skip n
		for {
			bb, err := b.Peek(0)
			if err != nil {
				return
			}
			if len(bb) < n {
				n -= len(bb)
				b.off = 0
				b.end = 0
				continue
			} else if len(bb) == n {
				b.off = 0
				b.end = 0
				return
			} else {
				b.off = n
				return
			}
		}
	}
	b.off += n
	if b.off == b.end {
		b.off = 0
		b.end = 0
	}
}

// Read will first return the buffered data, then read.
// For SNI routing we don't actually need this - in is a TcpConn and
// we'll use in.ReadFrom to take advantage of splice.
func (s *Buffer) Read(d []byte) (int, error) {
	if s.end-s.off > 0 {
		bn := copy(d, s.buf[s.off:s.end])
		s.off += bn
		return bn, nil
	}
	return s.Reader.Read(d)
}

func (s *Buffer) ReadByte() (byte, error) {
	if s.IsEmpty() {
		_, err := s.Peek(0)
		if err != nil {
			return 0, err
		}
	}
	r := s.buf[s.off]
	s.off++
	return r, nil
}
