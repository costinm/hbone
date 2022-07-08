package nio

import (
	"encoding/binary"
	"io"
)

// StreamBuffer wraps a buffer and a Reader.
// The Fill method will populate the buffer by doing one or more Read() operations, up to buffer size.
// Read will first return data from the buffer, and if buffer is empty will read directly from the source reader.
// The buffer can be used for parsing.
type StreamBuffer struct {
	buf      []byte
	off, end int

	Reader io.Reader
}

func NewBufferReader(in io.Reader) *StreamBuffer {
	buf1 := bufferPoolCopy.Get().([]byte)
	return &StreamBuffer{buf: buf1, Reader: in}
}

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
func (s *StreamBuffer) Peek(i int) ([]byte, error) {
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

// Size return the number of unread bytes in the buffer.
func (b *StreamBuffer) Size() int {
	if b == nil {
		return 0
	}
	return b.end - b.off
}

// Discard will move the start with n bytes.
// TODO: if n > buffer, blocking read. Currently not used in the code.
func (b *StreamBuffer) Discard(n int) {
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
func (s *StreamBuffer) Read(d []byte) (int, error) {
	if s.end > 0 {
		bn := copy(d, s.buf[s.off:s.end])
		s.off += bn
		if s.off == s.end {
			s.end = 0
		}
		return bn, nil
	}
	return s.Reader.Read(d)
}

func (s *StreamBuffer) Close() error {
	if s.buf != nil {
		bufferPoolCopy.Put(s.buf)
		s.buf = nil
	}
	if c, ok := s.Reader.(io.Closer); ok {
		return c.Close()
	}
	return nil
}

func (s *StreamBuffer) ReadByte() (byte, error) {
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

//func (b *Buffer) Recycle() {
//	b.owner = bufferedConPool
//	bufferedConPool.Put(b)
//}

func (b *StreamBuffer) IsEmpty() bool {
	if b == nil {
		return true
	}
	return b.off >= b.end
}

func (b *StreamBuffer) WriteByte(d byte) {
	b.grow(1)
	b.buf[b.end] = d
	b.end++
}

func (b *StreamBuffer) WriteUnint32(i uint32) {
	b.grow(4)
	binary.LittleEndian.PutUint32(b.buf[b.end:], i)
	b.end += 4
}

func (b *StreamBuffer) WriteVarint(i int64) {
	b.grow(8)
	c := binary.PutVarint(b.buf[b.end:], i)
	b.end += c
}

func (b *StreamBuffer) grow(n int) {
	c := cap(b.buf)
	if c-b.end > n {
		return
	}
	buf := make([]byte, c*2)
	copy(buf, b.buf[b.off:b.end])
	b.buf = buf
	b.end = b.end - b.off
	b.off = 0
}

func (b *StreamBuffer) Compact() {
	copy(b.buf, b.buf[b.off:b.end])
	b.end = b.end - b.off
	b.off = 0
}

func (b *StreamBuffer) Write(p []byte) (n int, err error) {
	n = len(p)
	b.grow(n)
	copy(b.buf[b.end:], p)
	b.end += n
	return
}

// Return the unread portion of the buffer
func (b *StreamBuffer) Bytes() []byte {
	return b.buf[b.off:b.end]
}
