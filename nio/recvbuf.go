package nio

import (
	"bytes"
	"context"
	"io"
	"log"
	"sync"
	"time"
)

// NewRecvBuffer creates a receive buffer.
//
// # Will hold frames, represented as bytes.Buffer, added with Put
//
// The Read() method will first return existing data, then block.
//
// recycleBuffer, if set, will be called after the buffer has been
// copied and can be reused.
// closeStream is called if there is any error except io.EOF or deadline exceeded.
func NewRecvBuffer(ctxDone <-chan struct{},
	recycleBuffer func(*bytes.Buffer), closeStream func(err error)) *RecvBufferReader {

	return &RecvBufferReader{
		ctxDone:       ctxDone,
		recycleBuffer: recycleBuffer,
		c:             make(chan RecvMsg, 1),
		closeStream:   closeStream,
	}
}

// RecvBufferReader implements io.Reader interface to readBlocking the frame data.
// Frames are added to the backlog or sent (non-blocking) to the channel from the
// reader thread.
//
// The blocking readBlocking is pretty complicated, attempts to consume all availalbe data
// first and returns it - before doing a blocking receive on the channel.
//
// TODO: WIP to pass data frames and avoid one copy.
//
// Orig RecvBuffer is an unbounded channel of RecvMsg structs.
// It can grow up to window size - flow control protects it.
//
// Note: RecvBuffer differs from buffer.Unbounded only in the fact that it
// holds a channel of RecvMsg structs instead of objects implementing "item"
// interface. RecvBuffer is written to much more often and using strict RecvMsg
// structs helps avoid allocation in "RecvBuffer.Put"
type RecvBufferReader struct {
	closeStream func(error) // Closes the client transport stream with the given error and nil trailer metadata.

	ctxDone <-chan struct{} // cache of ctx.Done() (for performance).

	// RecvMsg from the IO thread.
	c  chan RecvMsg
	mu sync.Mutex

	backlog []RecvMsg

	// Err is set when a buffer with that error is Put. backlog may have additional data,
	// but no new data will be received.
	// May be io.EOF
	Err error

	last *bytes.Buffer // Stores the remaining data in the previous calls.

	// Recycle the buffer
	recycleBuffer func(*bytes.Buffer)
	ReadDeadline  time.Time
}

// RecvMsg represents the received msg from the transport. All transport
// protocol specific info has been removed.
type RecvMsg struct {
	Buffer *bytes.Buffer
	// nil: received some data
	// io.EOF: stream is completed. data is nil.
	// other non-nil error: transport failure. data is nil.
	Err error
}

// Put adds the buffer to either the chan or backlog.
// Reads on chan most be followed by reloadChannel.
func (b *RecvBufferReader) Put(r RecvMsg) {
	b.mu.Lock()
	if b.Err != nil {
		b.mu.Unlock()
		// An error had occurred earlier, don't accept more
		// data or errors. Including io.EOF
		return
	}
	if r.Err != nil {
		b.Err = r.Err
	}

	if len(b.backlog) == 0 {
		select {
		// There is space in the chan
		case b.c <- r:
			if b.Err != nil {
				close(b.c)
			}
			b.mu.Unlock()
			return
		default:
		}
	}

	// No space in the chan - add it to backlog
	b.backlog = append(b.backlog, r)
	b.mu.Unlock()
}

// reloadChannel will send the first item from the backlog to the channel.
// If backlog is empty - nothing.
// Next receive on the chan will have a buffer.
//
// Upon receipt of a RecvMsg, the caller MUST call reloadChannel.
func (b *RecvBufferReader) reloadChannel() {
	b.mu.Lock()
	if len(b.backlog) > 0 {
		select {
		case b.c <- b.backlog[0]:
			b.backlog[0] = RecvMsg{}
			b.backlog = b.backlog[1:]
		default:
		}
	}
	b.mu.Unlock()
}

// RecvNonBlocking is like Recv, but won't block
func (r *RecvBufferReader) RecvNB() (bb *bytes.Buffer, err error) {
	// Previous Read()
	if r.last != nil {
		if r.last.Len() == 0 {
			r.last = nil
		} else {
			return r.last, nil
		}
	}

	if r.Err != nil && r.Err != io.EOF {
		return nil, r.Err
	}

	// Get the next buffer
	select {
	case m := <-r.c:
		r.reloadChannel() // so next read works

		return m.Buffer, m.Err
	case <-r.ctxDone:
		return nil, context.Canceled
		return
	default:
		break
	}

	return nil, nil
}

// Recv returns next data frame buffer, or block until a new buffer is available.
func (r *RecvBufferReader) Recv() (bb *bytes.Buffer, err error) {
	// Previous Read()
	if r.last != nil {
		if r.last.Len() == 0 {
			r.last = nil
		} else {
			return r.last, nil
		}
	}

	if r.Err != nil && r.Err != io.EOF {
		return nil, r.Err
	}

	if d := r.ReadDeadline; !d.IsZero() {
		now := time.Now()
		if d.Before(now) {
			return nil, ErrDeadlineExceeded
		}
		timer := time.NewTimer(d.Sub(now))
		// TODO: use Reset(max)
		defer timer.Stop()
		select {
		case <-timer.C:
			// This does not close the channel or causes error to be
			// preserved
			err = ErrDeadlineExceeded
			return
		case <-r.ctxDone:
			err = context.Canceled
			return
		case m := <-r.c:
			r.reloadChannel()
			return m.Buffer, m.Err
		}
	}

	// Get the next buffer
	select {
	case m := <-r.c:
		r.reloadChannel() // so next read works

		return m.Buffer, m.Err
	case <-r.ctxDone:
		return nil, context.Canceled
		return
	}

	return nil, io.EOF
}

// Read reads the next len(p) bytes from last. If last is drained, it tries to
// readBlocking additional data from recv. It blocks if there no additional data available
// in recv. If Read returns any non-nil error, it will continue to return that error.
func (r *RecvBufferReader) Read(p []byte) (n int, err error) {
	copied := 0
	if r.last != nil {
		// Read remaining data left in last call.
		copied, _ = r.last.Read(p)
		if r.last.Len() == 0 {
			r.recycleBuffer(r.last)
			r.last = nil
		}
		if copied == len(p) {
			return copied, nil
		}
	}
	// copy more data if already received and space in the p
	// Not blocking, just gets all available data.
	more := true
	for copied < len(p) && more && (r.Err == nil || r.Err == io.EOF) {
		select {
		case m := <-r.c:
			r.reloadChannel() // so r.recv works

			if m.Buffer != nil {
				copied1, _ := m.Buffer.Read(p[copied:])
				if m.Buffer.Len() == 0 {
					r.recycleBuffer(m.Buffer)
					r.last = nil
				} else {
					r.last = m.Buffer
					copied += copied1
					break // we filled the buffer
				}
				copied += copied1
				if copied == len(p) {
					return copied, nil
				}
			}
			if m.Err != nil {
				return copied, m.Err
			}
			continue
		default:
			more = false
			break
		}
	}

	if copied > 0 || copied == len(p) {
		return copied, nil
	}

	// Done with the backlog, we'll not get any more data.

	// Short circuit RST, context cancelations, etc - don't attempt
	// to block or read.
	if r.Err != nil && r.Err != io.EOF {
		return 0, r.Err
	}

	if r.Err == io.EOF {
		log.Println("EOF", len(r.backlog))
	}

	n, err = r.readBlocking(p[copied:])
	if err == ErrDeadlineExceeded {
		// Do not close stream or persiste the error - it is transient
		return copied + n, err
	}
	if err != nil && r.Err == nil {
		r.Err = err
	}
	if r.Err == io.EOF && copied+n > 0 {
		// Do not close stream or persiste the error - it is transient
		return copied + n, nil
	}
	if r.Err == io.EOF {
		// Do not close stream or persiste the error - it is transient
		return 0, io.EOF
	}

	if r.Err != nil && r.closeStream != nil && r.Err != io.EOF {
		r.closeStream(r.Err)
	}

	if copied+n > 0 {
		return copied + n, nil
	} else {
		return 0, r.Err
	}
}

// used in h2 server, blocks
func (r *RecvBufferReader) readBlocking(p []byte) (n int, err error) {

	//if d := r.readDeadline; !d.IsZero() {
	//	n := time.Now()
	//	if d.Before(n) {
	//		return 0, nio.ErrDeadlineExceeded
	//	}
	//	timer := time.AfterFunc(d.Sub(time.Now()), func() {
	//		select {
	//		case r.c <- RecvMsg{err: nio.ErrDeadlineExceeded}:
	//		default:
	//		}
	//	})
	//	// TODO: use Reset(max)
	//	defer timer.Stop()
	//}

	if d := r.ReadDeadline; !d.IsZero() {
		now := time.Now()
		if d.Before(now) {
			return 0, ErrDeadlineExceeded
		}
		timer := time.NewTimer(d.Sub(now))
		// TODO: use Reset(max)
		defer timer.Stop()
		select {
		case <-timer.C:
			// This does not close the channel or causes error to be
			// preserved
			return 0, ErrDeadlineExceeded
		case <-r.ctxDone:
			return 0, context.Canceled
		case m := <-r.c:
			r.reloadChannel()
			return r.readAdditional(m, p)
		}
	}

	select {
	case <-r.ctxDone:
		return 0, context.Canceled
	case m := <-r.c:
		r.reloadChannel()
		return r.readAdditional(m, p)
	}
}

// readAdditional tries to copy more data from a RecvMsg to p
func (r *RecvBufferReader) readAdditional(m RecvMsg, p []byte) (n int, err error) {
	if m.Buffer != nil {
		copied, _ := m.Buffer.Read(p)
		if m.Buffer.Len() == 0 {
			r.recycleBuffer(m.Buffer)
			r.last = nil
		} else {
			r.last = m.Buffer
		}
		return copied, m.Err
	} else if m.Err == nil {
		return 0, ErrDeadlineExceeded
	}
	return 0, m.Err
}
