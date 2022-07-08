package nio

import (
	"encoding/binary"
)

// Buffer is a buffer associated with a stream that can be used to sniff data and to
// reuse the read buffers.
//
// Similar with reading in a buffer for sniffing and using:
//
// ```
// io.MultiReader(bytes.NewReader(buffer.Bytes()), io.TeeReader(source, buffer))
// ```
//
type Buffer struct {
	//bb bytes.Buffer
	// b has end and capacity, set at creation to the size of the buffer
	// using end and off as pointers to data
	// Deprecated - use the methods
	buf []byte

	// read so far from buffer. Unread data in off:End
	off int

	// last bytes with data in the buffer. bytes.Buffer uses len(buf)
	end int

	// WIP: avoid copy, linked list of buffers.
	//next *Buffer
	// WIP: ownership
	owner interface{}
}

//// Reader returns a bytes.Reader for v.
//func (v *Buffer) Reader() bytes.Reader {
//	var r bytes.Reader
//	r.Reset(v.buf[v.off:v.end])
//	return r
//}

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
	b.owner = bufferedConPool
	bufferedConPool.Put(b)
}

func (b *Buffer) IsEmpty() bool {
	if b == nil {
		return true
	}
	return b.off >= b.end
}

func (b *Buffer) Size() int {
	if b == nil {
		return 0
	}
	return b.end - b.off
}

func (b *Buffer) WriteByte(d byte) {
	b.grow(1)
	b.buf[b.end] = d
	b.end++
}

func (b *Buffer) WriteUnint32(i uint32) {
	b.grow(4)
	binary.LittleEndian.PutUint32(b.buf[b.end:], i)
	b.end += 4
}

func (b *Buffer) SetUnint32(pos int, i uint32) {
	binary.LittleEndian.PutUint32(b.buf[pos:], i)
}

func (b *Buffer) WriteVarint(i int64) {
	b.grow(8)
	c := binary.PutVarint(b.buf[b.end:], i)
	b.end += c
}

func (b *Buffer) grow(n int) {
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

func (b *Buffer) Write(p []byte) (n int, err error) {
	n = len(p)
	b.grow(n)
	copy(b.buf[b.end:], p)
	b.end += n
	return
}

// Return the unread portion of the buffer
func (b *Buffer) Bytes() []byte {
	return b.buf[b.off:b.end]
}

func (b *Buffer) Out() []byte {
	return b.buf[b.end:cap(b.buf)]
}

func (b *Buffer) UpdateAppend(bout []byte) {
	b.buf = bout
	b.end = len(bout)
}
