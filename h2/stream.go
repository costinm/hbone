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
package h2

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/costinm/hbone/h2/frame"
	"github.com/costinm/hbone/nio"
)

type dataFramePool struct {
	pool sync.Pool
}

func newBufferPool() *dataFramePool {
	return &dataFramePool{
		pool: sync.Pool{
			New: func() interface{} {
				return new(bytes.Buffer)
			},
		},
	}
}

func (p *dataFramePool) get() *bytes.Buffer {
	return p.pool.Get().(*bytes.Buffer)
}

func (p *dataFramePool) put(b *bytes.Buffer) {
	p.pool.Put(b)
}

type streamState uint32

const (
	streamActive streamState = iota
	streamDone               // the entire stream is finished.
)

// H2Stream represents an H2 stream.
//
// H2Strem implements the net.Conn, context.Context interfaces
// It exposes read and write quota and other low-level H2 concepts.
type H2Stream struct {
	// Stream ID - odd for streams initiated from server (push and reverse)
	Id uint32

	st *H2Transport // nil for client side H2Stream
	ct *H2Transport // nil for server side H2Stream

	// the associated context of the stream
	// All blocking methods should check for ctxDone
	ctx     context.Context
	ctxDone <-chan struct{} // same as done chan but for server side. Cache of ctx.Done() (for performance)
	cancel  context.CancelFunc

	// If set, all data frames will be sent to this method and bypass the reader queue.
	// User is required to call s.Transport().UpdateWindow(s, uint32(n)) explicitly to receive
	// more data.
	OnData func(*frame.DataFrame)

	// H2Stream implements the Context interface. Closed at the end.
	// transport.closeStream closes this.
	done chan struct{} // closed at the end of stream to unblock.

	Events

	// List of DATA frame bufferss received, channel to waitOrConsumeQuota more (blocking)
	inFrameList *nio.RecvBufferReader

	inFlow  *inFlow
	outFlow *writeQuota

	writeDoneChan       chan *dataFrame
	writeDoneChanClosed uint32

	headerChan       chan struct{} // closed to indicate the end of header metadata.
	headerChanClosed uint32        // set when headerChan is closed. Used to avoid closing headerChan multiple times.

	// hdrMu protects header and trailer metadata on the server-side.
	hdrMu sync.Mutex

	Request  *http.Request
	Response *http.Response

	// Error causing the close of the stream - stream reset, connection errors, etc
	// trReader.Err contains any read error - including io.EOF, which indicates successful read close.
	Error error

	noHeaders bool // set if the client never received headers (set only after the stream is done).

	// On the server-side, headerSent is atomically set to 1 when the headers are sent out.
	headerSent uint32

	// Old state - doesn't work if readBlocking/write can be closed independently.
	state streamState

	// A stream may still be in used when readBlocking and write are closed - client or server may process
	// headers.

	// Set when FIN or RST received. No frame should be received after.
	// Also set after a trailer header
	readClosed uint32

	// Set when FIN or RST were sent. No frame should be written after.
	// Also set after sending a trailer.
	writeClosed uint32

	bytesReceived uint32 // indicates whether any bytes have been received on this stream
	unprocessed   uint32 // set if the server sends a refused stream or GOAWAY including this stream

	writeDeadline time.Time

	// grpc is set if the stream is working in grpc mode, based on content type.
	// Close will use trailer instead of end data, ordering of headers.
	grpc bool

	nio.Stats

	// Values exposed as part of Context interface.
	userValues map[string]interface{}
}

// AdjustWindow sends out extra window update over the initial window size
// of stream if the application is requesting data larger in size than
// the window. For example if Read([]buf) is called with a large buffer, we
// notify the other side that we have buffer space. This is a temporary
// adjustment.
func (s *H2Stream) AdjustWindow(n uint32) {
	s.Transport().adjustWindow(s, n)

}

// Header is part of the ResponseWriter interface. It returns the

// Response headers.
func (s *H2Stream) Header() http.Header {
	return s.Response.Header
}

// WriteHeader implements the ResponseWriter interface. Used to send a
// response back to the initiator.
func (s *H2Stream) WriteHeader(statusCode int) {
	if statusCode == 0 {
		s.Response.Status = "200"
	} else {
		s.Response.Status = strconv.Itoa(statusCode)
	}

	if !s.isHeaderSent() { // Headers haven't been written yet.
		if err := s.Transport().WriteHeader(s); err != nil {
			log.Println("WriteHeader", err)
			return
		}
	}
}

func (s *H2Stream) RequestHeader() http.Header {
	return s.Request.Header
}

// Send the request headers.
// Does not wait for status.
func (s *H2Stream) Do() error {
	_, err := s.Transport().Dial(s.Request)
	return err
}

// CloseError will close the stream with an error code.
// This will result in a RST sent, reader will be closed as well.
//
// Will unblock Read and Write and cancel the stream context.
func (s *H2Stream) CloseError(code uint32) {
	// On error we remove the stream from active.
	// In and out are also closed, s is canceled
	s.Transport().closeStream(s, nil, true, frame.ErrCode(code))
}

// Send EOS/FIN.
//
// Client: send FIN
// Server: send trailers and FIN
func (s *H2Stream) CloseWrite() error {
	if !s.updateHeaderSent() { // No headers have been sent.
		if err := s.Transport().writeResponseHeaders(s); err != nil {
			return err
		}
	}

	// TODO: use this only if trailer are present
	if len(s.Response.Trailer) > 0 {
		if nio.Debug {
			log.Println("Server sends CloseWrite() with trailer", s.Id)
		}
		return s.Transport().writeTrailer(s)
	} else {
		if nio.Debug {
			log.Println("Server sends CloseWrite()", s.Id)
		}
		return s.Transport().Write(s, nil, nil, true)
	}
}

// TCP/IP shutdown allows control over closing the stream:
//   - 0 unblock readBlocking() with err, in TCP any further received data is rejected with RST
//     but if we received a FIN - all is good.
//   - 1 same with CloseWrite()
//   - 2 both directions.

// Close() is one of the most complicated methods for H2 and TCP.
//
// Recommended close:
//   - CloseWrite will send a FIN (data frame or trailer headers). Read continues
//     until it receives FIN, at which point stream is closed. Close() is called internally.
//   - CloseError will send a RST explicitly, with the given code. Read is closed as well,
//     and Close() is called internally.
//
// Repeated calls to Close() have no effects.
//
// Unfortunately many net.Conn users treat Close() as CloseWrite() - since there
// is no explicit method. TLS Conn does send closeNotify, and TCP Conn appears to
// also send FIN. RST in TCP is only sent if data is received after close().
//
// The expectation is that Read() will also be unblocked by Close, if EOF not already
// received - but it should not return EOF since it's not the real cause. Like
// os close, there is no lingering by default.
//
// net.Close doc: Close closes the connection.
// Any blocked Read or Write operations will be unblocked and return errors.
func (s *H2Stream) Close() error {
	if s.getWriteClosed() == 0 {
		// No call to CloseWrite, calling it now, assuming Close() is meant to do a
		// graceful close, without having the interface.
		s.CloseWrite()
	}

	// Send a END_STREAM (if not already sent)

	// TODO: if write buffers are not empty, still send RST, empty write buf and unblock.
	log.Println("Stream Close()", s.Id)

	s.Transport().closeStream(s, nil, false, frame.ErrCode(0))

	if s.readClosed == 0 {
		s.setReadClosed(2, errStreamRST)
	}
	// Will unblock reads/write waiting
	if s.cancel != nil {
		s.cancel()
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
func (s *H2Stream) Conn() net.Conn {
	return s.Transport().conn
}

func (hc *H2Stream) Transport() *H2Transport {
	if hc.ct != nil {
		return hc.ct
	}
	return hc.st
}

func (s *H2Stream) Write(d []byte) (int, error) {
	err := s.Transport().Write(s, nil, d, false)
	if err != nil {
		return 0, err
	}
	return len(d), err
}

// isHeaderSent is only valid on the server-side.
func (s *H2Stream) isHeaderSent() bool {
	return atomic.LoadUint32(&s.headerSent) == 1
}

// updateHeaderSent updates headerSent and returns true
func (s *H2Stream) updateHeaderSent() bool {
	return atomic.SwapUint32(&s.headerSent, 1) == 1
}

func (s *H2Stream) swapState(st streamState) streamState {
	return streamState(atomic.SwapUint32((*uint32)(&s.state), uint32(st)))
}

// setReadClosed marks the readBlocking part of the stream as closed.
// It will not unblock readBlocking - you need to queue a frame to do so.
func (s *H2Stream) setReadClosed(mode uint32, err error) {
	old := atomic.SwapUint32(&s.readClosed, mode)
	if old != 0 {
		log.Println("setReadClose: Double close", s.Error, s.inFrameList.Err)
		return
	}
	//if s.trReader.Err == nil {
	//	s.trReader.Err = err
	//}
	s.queueReadBuf(nio.RecvMsg{Err: err})
}

func (s *H2Stream) setWriteClosed(mode uint32) bool {
	return atomic.SwapUint32(&s.writeClosed, 1) == 1
}

func (s *H2Stream) getWriteClosed() uint32 {
	return atomic.LoadUint32((*uint32)(&s.writeClosed))
}

func (s *H2Stream) getReadClosed() uint32 {
	return atomic.LoadUint32((*uint32)(&s.readClosed))
}

func (s *H2Stream) getState() streamState {
	return streamState(atomic.LoadUint32((*uint32)(&s.state)))
}

func (s *H2Stream) WaitHeaders() {
	if s.headerChan == nil {
		// On the server headerChan is always nil since a stream originates
		// only after having received headers.
		return
	}
	select {
	case <-s.ctx.Done():
		// Close the stream to prevent headers/trailers from changing after
		// this function returns.
		s.Transport().closeStream(s, ContextErr(s.ctx.Err()), true, frame.ErrCodeCancel)
		// headerChan could possibly not be closed yet if closeStream raced
		// with operateResponseHeaders; wait until it is closed explicitly here.
		<-s.headerChan
	case <-s.headerChan:
	}
}

// Context returns the context of the stream.
func (s *H2Stream) Context() context.Context {
	return s.ctx
}

// Called when data frames are received (with a COPY of the data !!)
func (s *H2Stream) queueReadBuf(m nio.RecvMsg) {
	s.inFrameList.Put(m)
}

func (s *H2Stream) LocalAddr() net.Addr {
	return s.Conn().LocalAddr()
}

func (s *H2Stream) RemoteAddr() net.Addr {
	return s.Conn().RemoteAddr()
}

func (s *H2Stream) SetDeadline(t time.Time) error {
	return nil
}
func (s *H2Stream) SetReadDeadline(t time.Time) error {
	s.inFrameList.ReadDeadline = t
	return nil
}
func (s *H2Stream) SetWriteDeadline(t time.Time) error {
	s.writeDeadline = t
	return nil
}

// Recycle a buffer for data frames.
func (s *H2Stream) RecycleDataFrame(bb *bytes.Buffer) {

}

// Recv returns the next data frame buffer and updates the window.
// The buffer should be recycled when done, or written to a different stream.
// Post write the buffers are recycled automatically.
func (s *H2Stream) RecvRaw() (bb *bytes.Buffer, err error) {
	bb, err = s.inFrameList.Recv()
	if bb != nil {
		s.Transport().UpdateWindow(s, uint32(bb.Len()))
	}
	return
}

// Read reads all p bytes from the wire for this stream.
func (s *H2Stream) Read(p []byte) (n int, err error) {
	n, err = s.inFrameList.Read(p)
	if n > 0 {
		s.Transport().UpdateWindow(s, uint32(n))
	}
	return
}

func (s *H2Stream) readOnly(p []byte) (n int, err error) {
	n, err = s.inFrameList.Read(p)
	return
}

// BytesReceived indicates whether any bytes have been received on this stream.
func (s *H2Stream) BytesReceived() bool {
	return atomic.LoadUint32(&s.bytesReceived) == 1
}

// GoString is implemented by H2Stream so context.String() won't
// race when printing %#v.
func (s *H2Stream) GoString() string {
	return fmt.Sprintf("<stream: %p, %v>", s, s.Request.URL)
}

// Deadline returns the time when work done on behalf of this context
// should be canceled. Deadline returns ok==false when no deadline is
// set. Successive calls to Deadline return the same results.
//
// This method always returns 0, false and is only present to make
// RequestCtx implement the context interface.
func (ctx *H2Stream) Deadline() (deadline time.Time, ok bool) {
	return
}

// Done returns a channel which is closed when it receives the final status
// from the server.
//
// Context: Done returns a channel that's closed when work done on behalf of this
// context should be canceled. Done may return nil if this context can
// never be canceled. Successive calls to Done return the same value.
func (ctx *H2Stream) Done() <-chan struct{} {
	return ctx.done
}

// Err returns a non-nil error value after Done is closed,
// successive calls to Err return the same error.
// If Done is not yet closed, Err returns nil.
// If Done is closed, Err returns a non-nil error explaining why:
// Canceled if the context was canceled (via server Shutdown)
// or DeadlineExceeded if the context's deadline passed.
func (ctx *H2Stream) Err() error {
	select {
	case <-ctx.done:
		return context.Canceled
	default:
		if ctx.Error != nil {
			return ctx.Error
		}
		if ctx.inFrameList.Err != io.EOF {
			return ctx.inFrameList.Err
		}
		return nil
	}
}

// Value returns the value associated with this context for key, or nil
// if no value is associated with key. Successive calls to Value with
// the same key returns the same result.
//
// This method is present to make RequestCtx implement the context interface.
// This method is the same as calling ctx.UserValue(key)
func (ctx *H2Stream) Value(key interface{}) interface{} {
	if keyString, ok := key.(string); ok && ctx.userValues != nil {
		return ctx.userValues[keyString]
	}
	return ctx.ctx.Value(key)
}

func (ctx *H2Stream) SetValue(key string, val interface{}) interface{} {
	if ctx.userValues == nil {
		ctx.userValues = map[string]interface{}{}
	}
	ctx.userValues[key] = val
	return nil
}

type H2Config struct {
	// InitialConnWindowSize sets the initial window size for a connection.
	InitialConnWindowSize int32

	// InitialWindowSize sets the initial window size for a stream.
	InitialWindowSize int32

	MaxFrameSize uint32

	// MaxHeaderListSize sets the max (uncompressed) size of header list that is prepared to be received.
	MaxHeaderListSize *uint32

	// WriteBufferSize sets the size of write buffer which in turn determines how
	// much data can be batched before it's written on the wire.
	WriteBufferSize int

	// ReadBufferSize sets the size of readBlocking buffer, which in turn determines how much data can
	// be readBlocking at most for one readBlocking syscall.
	ReadBufferSize int

	// ====== Client specific =========
	// UserAgent is the application user agent.
	UserAgent string

	// FailOnNonTempDialError specifies if gRPC fails on non-temporary dial errors.
	FailOnNonTempDialError bool

	// KeepaliveParams stores the keepalive parameters.
	KeepaliveParams ClientParameters
}

// ServerConfig consists of all the configurations to establish a server transport.
type ServerConfig struct {
	H2Config

	MaxStreams      uint32
	HeaderTableSize *uint32
	KeepaliveParams ServerParameters
	KeepalivePolicy EnforcementPolicy
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
	// stream's WorkloadID in unprocessed RPCs.
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

// channelzData is used to store channelz related data for H2ClientTransport and HTTP2ServerMux.
// These fields cannot be embedded in the original structs (e.g. H2ClientTransport), since to do atomic
// operation on int64 variable on 32-bit machine, user is responsible to enforce memory alignment.
// Here, by grouping those int64 fields inside a struct, we are enforcing the alignment.
type channelzData struct {
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

// Status is the gRPC trailer. It can also be used in regular H2 (despite the grpc keys)
// instead of inventing new names. It implements error interface.
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
	//      (e.g., restarting a readBlocking-modify-write sequence).
	//  (c) Use FailedPrecondition if the client should not retry until
	//      the system state has been explicitly fixed. E.g., if an "rmdir"
	//      fails because the directory is non-empty, FailedPrecondition
	//      should be returned since the client should not retry unless
	//      they have first fixed up the directory by deleting files from it.
	//  (d) Use FailedPrecondition if the client performs conditional
	//      REST Get/Update/Delete on a resource and the resource on the
	//      server does not match the condition. E.g., conflicting
	//      readBlocking-modify-write on the same resource.
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
	// system will generate InvalidArgument if asked to readBlocking at an
	// offset that is not in the range [0,2^32-1], but it will generate
	// OutOfRange if asked to readBlocking from an offset past the current
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

//func (hc *Stream) Close() error {
//	// TODO: send trailers if server !!!
//	//hc.W.Close()
//	if cw, ok := hc.Out.(io.Closer); ok {
//		cw.Close()
//	}
//	if cw, ok := hc.In.(io.Closer); ok {
//		return cw.Close()
//	}
//
//	return nil
//}

// Return a buffer with reserved front space to be used for appending.
// If using functions like proto.Marshal, b.UpdateForAppend should be called
// with the new []byte. App should not touch the prefix.
func (hc *H2Stream) GetWriteFrame(sz int) *nio.Buffer {
	b := nio.GetBuffer(5+9, sz)
	return b
}

// Send is an alternative to Write(), where the buffer ownership is passed to
// the stream and the method does not wait for the buffer to be sent.
func (hc *H2Stream) SendRaw(b []byte, start, end int, last bool) error {
	return hc.Transport().Send(hc, b, start, end, last)
}

// Framed sending/receiving.
func (hc *H2Stream) Send(b *nio.Buffer) error {

	hc.SentPackets++

	frameLen := b.Len()

	b.SetUnint32BE(b.Start()-4, uint32(frameLen))
	b.SetByte(b.Start()-5, 0)

	_, err := hc.Write(b.Buffer()[b.Start()-5 : b.End()])
	//hc.Flush()

	b.Recycle()

	return err
}

func (s *H2Stream) SetTransport(tr *H2Transport, client bool) {
	if client {
		s.ct = tr
	} else {
		s.st = tr
	}
}
