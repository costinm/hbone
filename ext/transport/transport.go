/*
 *
 * Copyright 2014 gRPC authors.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 *
 */

// Package transport defines and implements message oriented communication
// channel to complete various transactions (e.g., an RPC).  It is meant for
// grpc-internal usage and is not intended to be imported directly by users.
package transport

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	http2 "github.com/costinm/hbone/ext/transport/frame"
	"github.com/costinm/hbone/nio"
)

const logLevel = 2

type bufferPool struct {
	pool sync.Pool
}

func newBufferPool() *bufferPool {
	return &bufferPool{
		pool: sync.Pool{
			New: func() interface{} {
				return new(bytes.Buffer)
			},
		},
	}
}

func (p *bufferPool) get() *bytes.Buffer {
	return p.pool.Get().(*bytes.Buffer)
}

func (p *bufferPool) put(b *bytes.Buffer) {
	p.pool.Put(b)
}

// recvMsg represents the received msg from the transport. All transport
// protocol specific info has been removed.
type recvMsg struct {
	buffer *bytes.Buffer
	// nil: received some data
	// io.EOF: stream is completed. data is nil.
	// other non-nil error: transport failure. data is nil.
	err error
}

// recvBuffer is an unbounded channel of recvMsg structs.
//
// Note: recvBuffer differs from buffer.Unbounded only in the fact that it
// holds a channel of recvMsg structs instead of objects implementing "item"
// interface. recvBuffer is written to much more often and using strict recvMsg
// structs helps avoid allocation in "recvBuffer.put"
type recvBuffer struct {
	c       chan recvMsg
	mu      sync.Mutex
	backlog []recvMsg
	err     error
}

func newRecvBuffer() *recvBuffer {
	b := &recvBuffer{
		c: make(chan recvMsg, 1),
	}
	return b
}

func (b *recvBuffer) put(r recvMsg) {
	b.mu.Lock()
	if b.err != nil {
		b.mu.Unlock()
		// An error had occurred earlier, don't accept more
		// data or errors.
		return
	}
	b.err = r.err
	if len(b.backlog) == 0 {
		select {
		case b.c <- r:
			b.mu.Unlock()
			return
		default:
		}
	}
	b.backlog = append(b.backlog, r)
	b.mu.Unlock()
}

func (b *recvBuffer) load() {
	b.mu.Lock()
	if len(b.backlog) > 0 {
		select {
		case b.c <- b.backlog[0]:
			b.backlog[0] = recvMsg{}
			b.backlog = b.backlog[1:]
		default:
		}
	}
	b.mu.Unlock()
}

// get returns the channel that receives a recvMsg in the buffer.
//
// Upon receipt of a recvMsg, the caller should call load to send another
// recvMsg onto the channel if there is any.
func (b *recvBuffer) get() <-chan recvMsg {
	return b.c
}

// recvBufferReader implements io.Reader interface to read the data from
// recvBuffer.
type recvBufferReader struct {
	closeStream func(error) // Closes the client transport stream with the given error and nil trailer metadata.
	ctx         context.Context
	ctxDone     <-chan struct{} // cache of ctx.Done() (for performance).
	recv        *recvBuffer
	last        *bytes.Buffer // Stores the remaining data in the previous calls.
	err         error
	freeBuffer  func(*bytes.Buffer)
}

// Read reads the next len(p) bytes from last. If last is drained, it tries to
// read additional data from recv. It blocks if there no additional data available
// in recv. If Read returns any non-nil error, it will continue to return that error.
func (r *recvBufferReader) Read(p []byte) (n int, err error) {
	copied := 0
	if r.last != nil {
		// Read remaining data left in last call.
		copied, _ = r.last.Read(p)
		if r.last.Len() == 0 {
			r.freeBuffer(r.last)
			r.last = nil
		}
	}
	// copy more data if already received and space in the p
	more := true
	for copied < len(p) && more {
		select {
		case m := <-r.recv.get():
			r.recv.load()
			if m.err != nil {
				return 0, m.err
			}
			copied1, _ := m.buffer.Read(p[copied:])
			if m.buffer.Len() == 0 {
				r.freeBuffer(m.buffer)
				r.last = nil
			} else {
				r.last = m.buffer
				copied += copied1
				break // we filled the buffer
			}
			copied += copied1
			continue
		default:
			more = false
			break
		}
	}

	if copied > 0 || copied == len(p) {
		return copied, nil
	}

	if r.err != nil {
		return 0, r.err
	}

	if r.closeStream != nil {
		n, r.err = r.readClient(p[copied:])
	} else {
		n, r.err = r.read(p[copied:])
	}
	return copied + n, r.err
}

// used in h2 server, blocks
func (r *recvBufferReader) read(p []byte) (n int, err error) {
	select {
	case <-r.ctxDone:
		return 0, ContextErr(r.ctx.Err())
	case m := <-r.recv.get():
		return r.readAdditional(m, p)
	}
}

// used in h2 client - closeStream is set to send FIN/RST on the other side
func (r *recvBufferReader) readClient(p []byte) (n int, err error) {
	// If the context is canceled, then closes the stream with nil metadata.
	// closeStream writes its error parameter to r.recv as a recvMsg.
	// r.readAdditional acts on that message and returns the necessary error.
	select {
	case <-r.ctxDone:
		// Note that this adds the ctx error to the end of recv buffer, and
		// reads from the head. This will delay the error until recv buffer is
		// empty, thus will delay ctx cancellation in Recv().
		//
		// It's done this way to fix a race between ctx cancel and trailer. The
		// race was, stream.Recv() may return ctx error if ctxDone wins the
		// race, but stream.Trailer() may return a non-nil md because the stream
		// was not marked as done when trailer is received. This closeStream
		// call will mark stream as done, thus fix the race.
		//
		// TODO: delaying ctx error seems like a unnecessary side effect. What
		// we really want is to mark the stream as done, and return ctx error
		// faster.
		r.closeStream(ContextErr(r.ctx.Err()))
		m := <-r.recv.get()
		return r.readAdditional(m, p)
	case m := <-r.recv.get():
		return r.readAdditional(m, p)
	}
}

func (r *recvBufferReader) readAdditional(m recvMsg, p []byte) (n int, err error) {
	r.recv.load()
	if m.err != nil {
		return 0, m.err
	}
	copied, _ := m.buffer.Read(p)
	if m.buffer.Len() == 0 {
		r.freeBuffer(m.buffer)
		r.last = nil
	} else {
		r.last = m.buffer
	}
	return copied, nil
}

type streamState uint32

const (
	streamActive streamState = iota
	streamDone               // the entire stream is finished.
)

// Stream represents an RPC in the transport layer.
type Stream struct {
	Id uint32

	st *HTTP2ServerMux // nil for client side Stream
	ct *HTTP2ClientMux // nil for server side Stream

	ctx    context.Context // the associated context of the stream
	cancel context.CancelFunc

	done     chan struct{}   // closed at the end of stream to unblock writers. On the client side.
	doneFunc func()          // invoked at the end of stream on client side.
	ctxDone  <-chan struct{} // same as done chan but for server side. Cache of ctx.Done() (for performance)

	Path string // the associated RPC Path of the stream.

	recvCompress string
	//sendCompress string

	buf      *recvBuffer
	trReader *transportReader
	fc       *inFlow
	wq       *writeQuota

	// Callback to state application's intentions to read data. This
	// is used to adjust flow control, if needed.
	requestRead func(int)

	writeDoneChan       chan *dataFrame
	writeDoneChanClosed uint32

	headerChan       chan struct{} // closed to indicate the end of header metadata.
	headerChanClosed uint32        // set when headerChan is closed. Used to avoid closing headerChan multiple times.

	// headerValid indicates whether a valid header was received.  Only
	// meaningful after headerChan is closed (always call waitOnHeader() before
	// reading its value).  Not valid on server side.
	headerValid bool

	// hdrMu protects header and trailer metadata on the server-side.
	hdrMu sync.Mutex

	Request  *http.Request
	Response *http.Response

	noHeaders bool // set if the client never received headers (set only after the stream is done).

	// On the server-side, headerSent is atomically set to 1 when the headers are sent out.
	headerSent uint32

	// Old state - doesn't work if read/write can be closed independently.
	state streamState

	// A stream may still be in used when read and write are closed - client or server may process
	// headers.

	// Set when FIN or RST received. No frame should be received after.
	// Also set after a trailer header
	readClosed uint32

	// Set when FIN or RST were sent. No frame should be written after.
	// Also set after sending a trailer.
	writeClosed uint32

	status *Status

	bytesReceived uint32 // indicates whether any bytes have been received on this stream
	unprocessed   uint32 // set if the server sends a refused stream or GOAWAY including this stream

	readDeadline  time.Time
	writeDeadline time.Time

	// grpc is set if the stream is working in grpc mode, based on content type.
	// Close will use trailer instead of end data, ordering of headers.
	grpc bool
}

func (s *Stream) CloseError(code uint32) {
	// On error we remove the stream from active.
	// In and out are also closed, s is canceled
	if s.st != nil {
		s.st.closeStream(s, true, http2.ErrCode(code), true)
	} else {
		s.ct.closeStream(s, nil, true, http2.ErrCode(code), nil, nil, true)
	}
}

// Part of ResponseStream Body (ReaderCloser).
// Should this should send a RST if EOF was not received ?
// Use CloseWrite for a sending FIN
func (s *Stream) Close() error {
	// TODO(costin): RST
	if s.st == nil {
		log.Println("Client Stream Read Close()", s.Id)

		s.ct.Write(s, nil, nil, true)
		s.ct.CloseStream(s, nil)
	} else {
		log.Println("Server stream Close()", s.Id)
		if s.grpc {
			return s.st.WriteStatus(s, &Status{})
		} else {
			s.CloseWrite()
			s.st.closeStream(s, true, http2.ErrCode(0), true)
		}
	}
	// Unblock writes waiting to send
	if s.writeDoneChanClosed == 0 {
		s.writeDoneChanClosed = 1
		select {
		case s.writeDoneChan <- nil:
		default:
		}
	}
	return nil
}

// Return the underlying connection ( typically tcp ), for remote addr or other things.
func (s *Stream) Conn() net.Conn {
	if s.st == nil {
		return s.ct.conn
	}
	return s.st.conn
}

func (s *Stream) Write(d []byte) (int, error) {
	if s.st == nil {
		// Client mode, RoundTrip with request Body
		err := s.ct.Write(s, nil, d, false)
		if err != nil {
			return 0, err
		}
		return len(d), err
	}
	err := s.st.Write(s, nil, d, false)
	if err != nil {
		return 0, err
	}
	return len(d), err
}

// Normal write close.
//
// Client: send FIN
// Server: send trailers and FIN
func (s *Stream) CloseWrite() error {
	if s.st == nil {
		// Client mode, RoundTrip with request Body
		if nio.Debug {
			log.Println("Client sends CloseWrite()", s.Id)
		}
		return s.ct.Write(s, nil, nil, true)
	}
	// TODO: use this only if trailer are present
	if false {
		if nio.Debug {
			log.Println("Server sends CloseWrite() with trailer", s.Id)
		}
		return s.st.WriteStatus(s, &Status{})
	} else {
		if nio.Debug {
			log.Println("Server sends CloseWrite()", s.Id)
		}
		return s.st.Write(s, nil, nil, true)
	}
}

// isHeaderSent is only valid on the server-side.
func (s *Stream) isHeaderSent() bool {
	return atomic.LoadUint32(&s.headerSent) == 1
}

// updateHeaderSent updates headerSent and returns true
// if it was alreay set. It is valid only on server-side.
func (s *Stream) updateHeaderSent() bool {
	return atomic.SwapUint32(&s.headerSent, 1) == 1
}

func (s *Stream) swapState(st streamState) streamState {
	return streamState(atomic.SwapUint32((*uint32)(&s.state), uint32(st)))
}

func (s *Stream) setReadClosed(mode uint32) bool {
	return atomic.SwapUint32(&s.readClosed, mode) == mode
}

func (s *Stream) setWriteClosed(mode uint32) bool {
	return atomic.SwapUint32(&s.writeClosed, 1) == 1
}

func (s *Stream) getWriteClosed() uint32 {
	return atomic.LoadUint32((*uint32)(&s.writeClosed))
}

func (s *Stream) getReadClosed() uint32 {
	return atomic.LoadUint32((*uint32)(&s.readClosed))
}

func (s *Stream) getState() streamState {
	return streamState(atomic.LoadUint32((*uint32)(&s.state)))
}

func (s *Stream) waitOnHeader() {
	if s.headerChan == nil {
		// On the server headerChan is always nil since a stream originates
		// only after having received headers.
		return
	}
	select {
	case <-s.ctx.Done():
		// Close the stream to prevent headers/trailers from changing after
		// this function returns.
		s.ct.CloseStream(s, ContextErr(s.ctx.Err()))
		// headerChan could possibly not be closed yet if closeStream raced
		// with operateHeaders; wait until it is closed explicitly here.
		<-s.headerChan
	case <-s.headerChan:
	}
}

// RecvCompress returns the compression algorithm applied to the inbound
// message. It is empty string if there is no compression applied.
func (s *Stream) RecvCompress() string {
	s.waitOnHeader()
	return s.recvCompress
}

// Done returns a channel which is closed when it receives the final status
// from the server.
func (s *Stream) Done() <-chan struct{} {
	return s.done
}

// Context returns the context of the stream.
func (s *Stream) Context() context.Context {
	return s.ctx
}

// Called when data frames are received (with a COPY of the data !!)
func (s *Stream) write(m recvMsg) {
	s.buf.put(m)
}

func (s *Stream) LocalAddr() net.Addr {
	return s.Conn().LocalAddr()
}

func (s *Stream) RemoteAddr() net.Addr {
	return s.Conn().RemoteAddr()
}

func (s *Stream) SetDeadline(t time.Time) error {
	return nil
}
func (s *Stream) SetReadDeadline(t time.Time) error {
	s.readDeadline = t
	return nil
}
func (s *Stream) SetWriteDeadline(t time.Time) error {
	s.writeDeadline = t
	return nil
}

// Read reads all p bytes from the wire for this stream.
func (s *Stream) Read(p []byte) (n int, err error) {
	rc := s.getReadClosed()
	if rc != 0 {
		if rc == 1 {
			return 0, io.EOF
		}
		return 0, errStreamRST
	}
	// Don't request a read if there was an error earlier
	if s.trReader.er != nil {
		return 0, s.trReader.er
	}
	// effectively t.adjustWindow(len(p)).
	// This is clearly not right - read may be smaller.
	// TODO(costin): fix, in gRPC Read reads the full p !
	//s.requestRead(len(p))
	return s.trReader.Read(p)
}

type h2transport interface {
	updateWindow(*Stream, uint32)
}

// tranportReader reads all the data available for this Stream from the transport and
// passes them into the decoder, which converts them into a gRPC message stream.
// The error is io.EOF when the stream is done or another non-nil error if
// the stream broke.
type transportReader struct {
	s      *Stream
	reader *recvBufferReader
	// The handler to control the window update procedure for both this
	// particular stream and the associated transport.
	windowHandler h2transport
	er            error
}

func (t *transportReader) Read(p []byte) (n int, err error) {
	n, err = t.reader.Read(p)
	if err != nil {
		t.er = err
		return
	}
	t.windowHandler.updateWindow(t.s, uint32(n))
	return
}

// BytesReceived indicates whether any bytes have been received on this stream.
func (s *Stream) BytesReceived() bool {
	return atomic.LoadUint32(&s.bytesReceived) == 1
}

// Unprocessed indicates whether the server did not process this stream --
// i.e. it sent a refused stream or GOAWAY including this stream ID.
func (s *Stream) Unprocessed() bool {
	return atomic.LoadUint32(&s.unprocessed) == 1
}

// GoString is implemented by Stream so context.String() won't
// race when printing %#v.
func (s *Stream) GoString() string {
	return fmt.Sprintf("<stream: %p, %v>", s, s.Request.URL)
}

// state of transport
type transportState int

const (
	reachable transportState = iota
	closing
	draining
)

// ServerConfig consists of all the configurations to establish a server transport.
type ServerConfig struct {
	MaxStreams            uint32
	MaxFrameSize          uint32
	KeepaliveParams       ServerParameters
	KeepalivePolicy       EnforcementPolicy
	InitialWindowSize     int32
	InitialConnWindowSize int32
	WriteBufferSize       int
	ReadBufferSize        int
	MaxHeaderListSize     *uint32
	HeaderTableSize       *uint32
}

// ConnectOptions covers all relevant options for communicating with the server.
type ConnectOptions struct {
	// UserAgent is the application user agent.
	UserAgent string
	// Dialer specifies how to dial a network address.
	Dialer func(context.Context, string) (net.Conn, error)
	// FailOnNonTempDialError specifies if gRPC fails on non-temporary dial errors.
	FailOnNonTempDialError bool
	// KeepaliveParams stores the keepalive parameters.
	KeepaliveParams ClientParameters
	// InitialWindowSize sets the initial window size for a stream.
	InitialWindowSize int32
	// InitialConnWindowSize sets the initial window size for a connection.
	InitialConnWindowSize int32
	// WriteBufferSize sets the size of write buffer which in turn determines how much data can be batched before it's written on the wire.
	WriteBufferSize int
	// ReadBufferSize sets the size of read buffer, which in turn determines how much data can be read at most for one read syscall.
	ReadBufferSize int
	// MaxHeaderListSize sets the max (uncompressed) size of header list that is prepared to be received.
	MaxHeaderListSize *uint32

	MaxFrameSize uint32

	// UseProxy specifies if a proxy should be used.
	UseProxy bool
}

// ClientParameters is used to set keepalive parameters on the client-side.
// These configure how the client will actively probe to notice when a
// connection is broken and send pings so intermediaries will be aware of the
// liveness of the connection. Make sure these parameters are set in
// coordination with the keepalive policy on the server, as incompatible
// settings can result in closing of connection.
type ClientParameters struct {
	// After a duration of this time if the client doesn't see any activity it
	// pings the server to see if the transport is still alive.
	// If set below 10s, a minimum value of 10s will be used instead.
	Time time.Duration // The current default value is infinity.
	// After having pinged for keepalive check, the client waits for a duration
	// of Timeout and if no activity is seen even after that the connection is
	// closed.
	Timeout time.Duration // The current default value is 20 seconds.
	// If true, client sends keepalive pings even with no active RPCs. If false,
	// when there are no active RPCs, Time and Timeout will be ignored and no
	// keepalive pings will be sent.
	PermitWithoutStream bool // false by default.
}

// ServerParameters is used to set keepalive and max-age parameters on the
// server-side.
type ServerParameters struct {
	// MaxConnectionIdle is a duration for the amount of time after which an
	// idle connection would be closed by sending a GoAway. Idleness duration is
	// defined since the most recent time the number of outstanding RPCs became
	// zero or the connection establishment.
	MaxConnectionIdle time.Duration // The current default value is infinity.
	// MaxConnectionAge is a duration for the maximum amount of time a
	// connection may exist before it will be closed by sending a GoAway. A
	// random jitter of +/-10% will be added to MaxConnectionAge to spread out
	// connection storms.
	MaxConnectionAge time.Duration // The current default value is infinity.
	// MaxConnectionAgeGrace is an additive period after MaxConnectionAge after
	// which the connection will be forcibly closed.
	MaxConnectionAgeGrace time.Duration // The current default value is infinity.
	// After a duration of this time if the server doesn't see any activity it
	// pings the client to see if the transport is still alive.
	// If set below 1s, a minimum value of 1s will be used instead.
	Time time.Duration // The current default value is 2 hours.
	// After having pinged for keepalive check, the server waits for a duration
	// of Timeout and if no activity is seen even after that the connection is
	// closed.
	Timeout time.Duration // The current default value is 20 seconds.
}

// EnforcementPolicy is used to set keepalive enforcement policy on the
// server-side. Server will close connection with a client that violates this
// policy.
type EnforcementPolicy struct {
	// MinTime is the minimum amount of time a client should wait before sending
	// a keepalive ping.
	MinTime time.Duration // The current default value is 5 minutes.
	// If true, server allows keepalive pings even when there are no active
	// streams(RPCs). If false, and client sends ping when there are no active
	// streams, server will send GOAWAY and close the connection.
	PermitWithoutStream bool // false by default.
}

// Options provides additional hints and information for message
// transmission.
type Options struct {
	// Last indicates whether this write is the last piece for
	// this stream.
	Last bool
}

// CallHdr carries the information of a particular RPC.
type CallHdr struct {
	Req *http.Request

	PreviousAttempts int // value of grpc-previous-rpc-attempts header to set

	DoneFunc func() // called when the stream is finished
}

// connectionErrorf creates an ConnectionError with the specified error description.
func connectionErrorf(temp bool, e error, format string, a ...interface{}) ConnectionError {
	return ConnectionError{
		Desc: fmt.Sprintf(format, a...),
		temp: temp,
		err:  e,
	}
}

// ConnectionError is an error that results in the termination of the
// entire connection and the retry of all the active streams.
type ConnectionError struct {
	Desc string
	temp bool
	err  error
}

func (e ConnectionError) Error() string {
	return fmt.Sprintf("connection error: desc = %q", e.Desc)
}

// Temporary indicates if this connection error is temporary or fatal.
func (e ConnectionError) Temporary() bool {
	return e.temp
}

// Origin returns the original error of this connection error.
func (e ConnectionError) Origin() error {
	// Never return nil error here.
	// If the original error is nil, return itself.
	if e.err == nil {
		return e
	}
	return e.err
}

// Unwrap returns the original error of this connection error or nil when the
// origin is nil.
func (e ConnectionError) Unwrap() error {
	return e.err
}

var (
	// ErrConnClosing indicates that the transport is closing.
	ErrConnClosing = connectionErrorf(true, nil, "transport is closing")
	// errStreamDrain indicates that the stream is rejected because the
	// connection is draining. This could be caused by goaway or balancer
	// removing the address.
	errStreamDrain = &Status{Code: int(Unavailable), Message: "the connection is draining"}
	// errStreamDone is returned from write at the client side to indiacte application
	// layer of an error.
	errStreamDone = errors.New("the stream is done")
	errStreamRST  = errors.New("the stream is RST")
	// StatusGoAway indicates that the server sent a GOAWAY that included this
	// stream's ID in unprocessed RPCs.
	statusGoAway = &Status{Code: int(Unavailable), Message: "the stream is rejected because server is draining the connection"}
)

// GoAwayReason contains the reason for the GoAway frame received.
type GoAwayReason uint8

const (
	// GoAwayInvalid indicates that no GoAway frame is received.
	GoAwayInvalid GoAwayReason = 0
	// GoAwayNoReason is the default value when GoAway frame is received.
	GoAwayNoReason GoAwayReason = 1
	// GoAwayTooManyPings indicates that a GoAway frame with
	// ErrCodeEnhanceYourCalm was received and that the debug data said
	// "too_many_pings".
	GoAwayTooManyPings GoAwayReason = 2
)

// channelzData is used to store channelz related data for HTTP2ClientMux and HTTP2ServerMux.
// These fields cannot be embedded in the original structs (e.g. HTTP2ClientMux), since to do atomic
// operation on int64 variable on 32-bit machine, user is responsible to enforce memory alignment.
// Here, by grouping those int64 fields inside a struct, we are enforcing the alignment.
type channelzData struct {
	kpCount int64
	// The number of streams that have started, including already finished ones.
	streamsStarted int64
	// Client side: The number of streams that have ended successfully by receiving
	// EoS bit set frame from server.
	// Server side: The number of streams that have ended successfully by sending
	// frame with EoS bit set.
	streamsSucceeded int64
	streamsFailed    int64
	// lastStreamCreatedTime stores the timestamp that the last stream gets created. It is of int64 type
	// instead of time.Time since it's more costly to atomically update time.Time variable than int64
	// variable. The same goes for lastMsgSentTime and lastMsgRecvTime.
	lastStreamCreatedTime int64
	msgSent               int64
	msgRecv               int64
	lastMsgSentTime       int64
	lastMsgRecvTime       int64
}

// ContextErr converts the error from context package into a status error.
func ContextErr(err error) error {
	switch err {
	case context.DeadlineExceeded:
		return &Status{Code: int(DeadlineExceeded), Message: err.Error()}
	case context.Canceled:
		return &Status{Code: int(Canceled), Message: err.Error()}
	}
	return &Status{Code: int(Internal), Message: fmt.Sprintf("Unexpected error from context packet: %v", err)}
}

type Status struct {

	// The status code, which should be an enum value of [google.rpc.Code][google.rpc.Code].
	Code int
	// A developer-facing error message, which should be in English. Any
	// user-facing error message should be localized and sent in the
	// [google.rpc.Status.details][google.rpc.Status.details] field, or localized by the client.
	Message string
	// A list of messages that carry the error details.  There is a common set of
	// message types for APIs to use.
	Details [][]byte

	Err error
}

func (s *Status) Error() string {
	if s.Err != nil {
		return s.Err.Error()
	}
	return s.Message
}

// A Code is an unsigned 32-bit error code as defined in the gRPC spec.
type Code uint32

const (
	// OK is returned on success.
	OK Code = 0

	// Canceled indicates the operation was canceled (typically by the caller).
	//
	// The gRPC framework will generate this error code when cancellation
	// is requested.
	Canceled Code = 1

	// Unknown error. An example of where this error may be returned is
	// if a Status value received from another address space belongs to
	// an error-space that is not known in this address space. Also
	// errors raised by APIs that do not return enough error information
	// may be converted to this error.
	//
	// The gRPC framework will generate this error code in the above two
	// mentioned cases.
	Unknown Code = 2

	// InvalidArgument indicates client specified an invalid argument.
	// Note that this differs from FailedPrecondition. It indicates arguments
	// that are problematic regardless of the state of the system
	// (e.g., a malformed file name).
	//
	// This error code will not be generated by the gRPC framework.
	InvalidArgument Code = 3

	// DeadlineExceeded means operation expired before completion.
	// For operations that change the state of the system, this error may be
	// returned even if the operation has completed successfully. For
	// example, a successful response from a server could have been delayed
	// long enough for the deadline to expire.
	//
	// The gRPC framework will generate this error code when the deadline is
	// exceeded.
	DeadlineExceeded Code = 4

	// NotFound means some requested entity (e.g., file or directory) was
	// not found.
	//
	// This error code will not be generated by the gRPC framework.
	NotFound Code = 5

	// AlreadyExists means an attempt to create an entity failed because one
	// already exists.
	//
	// This error code will not be generated by the gRPC framework.
	AlreadyExists Code = 6

	// PermissionDenied indicates the caller does not have permission to
	// execute the specified operation. It must not be used for rejections
	// caused by exhausting some resource (use ResourceExhausted
	// instead for those errors). It must not be
	// used if the caller cannot be identified (use Unauthenticated
	// instead for those errors).
	//
	// This error code will not be generated by the gRPC core framework,
	// but expect authentication middleware to use it.
	PermissionDenied Code = 7

	// ResourceExhausted indicates some resource has been exhausted, perhaps
	// a per-user quota, or perhaps the entire file system is out of space.
	//
	// This error code will be generated by the gRPC framework in
	// out-of-memory and server overload situations, or when a message is
	// larger than the configured maximum size.
	ResourceExhausted Code = 8

	// FailedPrecondition indicates operation was rejected because the
	// system is not in a state required for the operation's execution.
	// For example, directory to be deleted may be non-empty, an rmdir
	// operation is applied to a non-directory, etc.
	//
	// A litmus test that may help a service implementor in deciding
	// between FailedPrecondition, Aborted, and Unavailable:
	//  (a) Use Unavailable if the client can retry just the failing call.
	//  (b) Use Aborted if the client should retry at a higher-level
	//      (e.g., restarting a read-modify-write sequence).
	//  (c) Use FailedPrecondition if the client should not retry until
	//      the system state has been explicitly fixed. E.g., if an "rmdir"
	//      fails because the directory is non-empty, FailedPrecondition
	//      should be returned since the client should not retry unless
	//      they have first fixed up the directory by deleting files from it.
	//  (d) Use FailedPrecondition if the client performs conditional
	//      REST Get/Update/Delete on a resource and the resource on the
	//      server does not match the condition. E.g., conflicting
	//      read-modify-write on the same resource.
	//
	// This error code will not be generated by the gRPC framework.
	FailedPrecondition Code = 9

	// Aborted indicates the operation was aborted, typically due to a
	// concurrency issue like sequencer check failures, transaction aborts,
	// etc.
	//
	// See litmus test above for deciding between FailedPrecondition,
	// Aborted, and Unavailable.
	//
	// This error code will not be generated by the gRPC framework.
	Aborted Code = 10

	// OutOfRange means operation was attempted past the valid range.
	// E.g., seeking or reading past end of file.
	//
	// Unlike InvalidArgument, this error indicates a problem that may
	// be fixed if the system state changes. For example, a 32-bit file
	// system will generate InvalidArgument if asked to read at an
	// offset that is not in the range [0,2^32-1], but it will generate
	// OutOfRange if asked to read from an offset past the current
	// file size.
	//
	// There is a fair bit of overlap between FailedPrecondition and
	// OutOfRange. We recommend using OutOfRange (the more specific
	// error) when it applies so that callers who are iterating through
	// a space can easily look for an OutOfRange error to detect when
	// they are done.
	//
	// This error code will not be generated by the gRPC framework.
	OutOfRange Code = 11

	// Unimplemented indicates operation is not implemented or not
	// supported/enabled in this service.
	//
	// This error code will be generated by the gRPC framework. Most
	// commonly, you will see this error code when a method implementation
	// is missing on the server. It can also be generated for unknown
	// compression algorithms or a disagreement as to whether an RPC should
	// be streaming.
	Unimplemented Code = 12

	// Internal errors. Means some invariants expected by underlying
	// system has been broken. If you see one of these errors,
	// something is very broken.
	//
	// This error code will be generated by the gRPC framework in several
	// internal error conditions.
	Internal Code = 13

	// Unavailable indicates the service is currently unavailable.
	// This is a most likely a transient condition and may be corrected
	// by retrying with a backoff. Note that it is not always safe to retry
	// non-idempotent operations.
	//
	// See litmus test above for deciding between FailedPrecondition,
	// Aborted, and Unavailable.
	//
	// This error code will be generated by the gRPC framework during
	// abrupt shutdown of a server process or network connection.
	Unavailable Code = 14

	// DataLoss indicates unrecoverable data loss or corruption.
	//
	// This error code will not be generated by the gRPC framework.
	DataLoss Code = 15

	// Unauthenticated indicates the request does not have valid
	// authentication credentials for the operation.
	//
	// The gRPC framework will generate this error code when the
	// authentication metadata is invalid or a Credentials callback fails,
	// but also expect authentication middleware to generate it.
	Unauthenticated Code = 16

	_maxCode = 17
)
