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

package h2

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/costinm/hbone/h2/frame"
	"github.com/costinm/hbone/h2/grpcutil"
	"github.com/costinm/hbone/h2/hpack"
	"github.com/costinm/hbone/nio"
)

// clientConnectionCounter counts the number of connections a client has
// initiated (equal to the number of http2Clients created). Must be accessed
// atomically.
var clientConnectionCounter uint64

// H2Transport handles one TCP/TLS connection with a peer, implementing
// client and server H2Streams. The goal is to support the full HTTPTransport.
type H2Transport struct {
	// Time when last frame was received.
	LastRead int64 // Keep this field 64-bit aligned. Accessed atomically.

	//kpCount  int64
	// The number of streams that have started, including already finished ones.
	streamsStarted   int64
	streamsSucceeded int64
	streamsFailed    int64
	// lastStreamCreatedTime stores the timestamp that the last stream gets created. It is of int64 type
	// instead of time.Time since it's more costly to atomically update time.Time variable than int64
	// variable. The same goes for lastMsgSentTime and lastMsgRecvTime.
	LastStreamCreatedTime int64
	// For framed, long-lived protocols (grpc, websocket) - keeps track of the frames.
	// For streams - keeps track of Data frames
	msgSent         int64
	msgRecv         int64
	lastMsgSentTime int64
	lastMsgRecvTime int64

	connectionID uint64
	streamQuota  int64

	// idle is the time instant when the connection went idle.
	// This is either the beginning of the connection or when the number of
	// RPCs go down to 0.
	// When the connection is busy, this value is set to 0.
	idle  time.Time
	start time.Time

	//
	mu                sync.Mutex
	initialWindowSize int32
	activeStreams     map[uint32]*H2Stream

	conn   net.Conn // underlying communication channel
	ctx    context.Context
	cancel context.CancelFunc
	loopy  *loopyWriter

	// Similar to net.http structure.
	readerDone chan struct{} // sync point to enable testing.
	writerDone chan struct{} // sync point to enable testing.
	framer     *framer
	// controlBuf delivers all the control related tasks (e.g., window
	// updates, reset streams, and various settings) to the controller.
	controlBuf *controlBuffer
	done       chan struct{}
	fc         *trInFlow
	bdpEst     *bdpEstimator
	bufferPool *dataFramePool

	streamsQuotaAvailable chan struct{}
	waitingStreams        uint32

	// configured by peer through SETTINGS_MAX_HEADER_LIST_SIZE
	maxSendHeaderListSize *uint32

	FrameSize    uint32
	PeerSettings *frame.SettingsFrame

	closing bool
	closed  bool

	Events

	Handle   func(*H2Stream)
	TraceCtx func(context.Context, string) context.Context

	// ============= Server specific ====================
	// maxStreamMu guards the maximum stream WorkloadID
	// This lock may not be taken if mu is already held.
	maxStreamMu sync.Mutex
	maxStreamID uint32 // max stream WorkloadID ever seen
	// The max number of concurrent streams.
	maxStreams uint32

	// Keepalive and max-age parameters for the server.
	kp ServerParameters
	// Keepalive enforcement policy.
	kep EnforcementPolicy
	// The time instance last ping was received.
	lastPingAt time.Time
	// Number of times the client has violated keepalive ping policy so far.
	pingStrikes uint8
	// Flag to signify that number of ping strikes should be reset to 0.
	// This is set whenever data or header frames are sent.
	// 1 means yes.
	resetPingStrikes uint32 // Accessed atomically.

	// drainChan is initialized when Drain() is called the first time.
	// After which the server writes out the first GoAway(with WorkloadID 2^31-1) frame.
	// Then an independent goroutine will be launched to later send the second GoAway.
	// During this time we don't want to write another first GoAway(with WorkloadID 2^31 -1) frame.
	// Thus call to Drain() will be a no-op if drainChan is already initialized since draining is
	// already underway.
	drainChan chan struct{}

	// ======== Client specific
	nextID uint32
	// goAway is closed to notify the upper layer (i.e., addrConn.transportMonitor)
	// that the server sent GoAway on this transport.
	goAway chan struct{}
	// A condition variable used to signal when the keepalive goroutine should
	// go dormant. The condition for dormancy is based on the number of active
	// streams and the `PermitWithoutStream` keepalive client parameter. And
	// since the number of active streams is guarded by the above mutex, we use
	// the same for this condition variable as well.
	kpDormancyCond *sync.Cond
	// A boolean to track whether the keepalive goroutine is dormant or not.
	// This is checked before attempting to signal the above condition
	// variable.
	kpDormant bool

	// Error causing the close of the transport
	Error error
	opts  *H2Config

	// Time of the accept() or dial
	StartTime time.Time
}

// H2ClientTransport implements the ClientTransport interface with HTTP2.
type H2ClientTransport struct {
	H2Transport
	userAgent string

	// The scheme used: https if TLS is on, http otherwise.
	scheme string

	isSecure bool

	KeepaliveParams  ClientParameters
	keepaliveEnabled bool

	maxConcurrentStreams uint32

	// prevGoAway WorkloadID records the Last-H2Stream-WorkloadID in the previous GOAway frame.
	prevGoAwayID uint32
	// goAwayReason records the http2.ErrCode and debug data received with the
	// GoAway frame.
	goAwayReason GoAwayReason
	// goAwayDebugMessage contains a detailed human readable string about a
	// GoAway frame, useful for error messages.
	goAwayDebugMessage string
}

// newHTTP2Client constructs a connected ClientTransport to addr based on HTTP2
// and starts to receive messages on it. Non-nil error returns if construction
// fails.
func NewConnection(ctx context.Context, opts H2Config) (_ *H2ClientTransport, err error) {
	scheme := "http"
	ctx, cancel := context.WithCancel(ctx)
	defer func() {
		if err != nil {
			cancel()
		}
	}()

	kp := opts.KeepaliveParams
	// Validate keepalive parameters.
	if kp.Time == 0 {
		kp.Time = defaultClientKeepaliveTime
	}
	if kp.Timeout == 0 {
		kp.Timeout = defaultClientKeepaliveTimeout
	}
	keepaliveEnabled := false
	if kp.Time != infinity {
		//if err = syscall.SetTCPUserTimeout(conn, kp.Timeout); err != nil {
		//	return nil, connectionErrorf(false, err, "transport: failed to set TCP_USER_TIMEOUT: %v", err)
		//}
		keepaliveEnabled = true
	}
	isSecure := true
	//var (
	//	authInfo credentials.AuthInfo
	//)
	//perRPCCreds := opts.PerRPCCredentials

	done := make(chan struct{})
	h2m := H2Transport{
		ctx:                   ctx,
		readerDone:            make(chan struct{}),
		writerDone:            make(chan struct{}),
		done:                  done,
		initialWindowSize:     defaultWindowSize, // this is for incoming
		activeStreams:         make(map[uint32]*H2Stream),
		bufferPool:            newBufferPool(),
		idle:                  time.Now(),
		start:                 time.Now(),
		streamQuota:           defaultMaxStreamsClient,
		streamsQuotaAvailable: make(chan struct{}, 1),
		goAway:                make(chan struct{}),
		nextID:                1,
		StartTime:             time.Now(),
		opts:                  &opts,
	}

	t := &H2ClientTransport{
		H2Transport: h2m,
		userAgent:   opts.UserAgent,
		scheme:      scheme,
		isSecure:    isSecure,
		//perRPCCreds:           perRPCCreds,
		KeepaliveParams:      kp,
		maxConcurrentStreams: defaultMaxStreamsClient,
		keepaliveEnabled:     keepaliveEnabled,
	}

	dynamicWindow := true
	if opts.InitialConnWindowSize >= defaultWindowSize {
		dynamicWindow = false
	}
	if opts.InitialWindowSize >= defaultWindowSize {
		dynamicWindow = false
	}
	if dynamicWindow {
		t.bdpEst = &bdpEstimator{
			bdp:               initialWindowSize,
			updateFlowControl: t.updateFlowControl,
		}
	}
	return t, nil
}

func (t *H2ClientTransport) StartConn(conn net.Conn) (err error) {
	writeBufSize := t.opts.WriteBufferSize
	readBufSize := t.opts.ReadBufferSize

	maxHeaderListSize := defaultClientMaxHeaderListSize
	if t.opts.MaxHeaderListSize != nil {
		maxHeaderListSize = *t.opts.MaxHeaderListSize
	}
	fs := uint32(http2MaxFrameLen)
	if t.opts.MaxFrameSize != 0 {
		fs = t.opts.MaxFrameSize
	}

	t.conn = conn
	// fs is used for read, advertising
	t.framer = newFramer(conn, writeBufSize, readBufSize, maxHeaderListSize, fs)

	//
	// Any further errors will close the underlying connection
	defer func(conn net.Conn) {
		if err != nil {
			conn.Close()
		}
	}(conn)

	t.controlBuf = newControlBuffer(&t.H2Transport, t.done)

	if t.keepaliveEnabled {
		t.kpDormancyCond = sync.NewCond(&t.mu)
		go t.keepalive()
	}
	// RoundTripStart the reader goroutine for incoming message. Each transport has
	// a dedicated goroutine which reads HTTP2 frame from network. Then it
	// dispatches the frame to the corresponding stream entity.
	go t.reader()

	// Send connection preface to server.
	n, err := t.conn.Write(clientPreface)
	if err != nil {
		err = connectionErrorf(true, err, "transport: failed to write client preface: %v", err)
		t.Close(err)
		return err
	}
	if n != len(clientPreface) {
		err = connectionErrorf(true, nil, "transport: preface mismatch, wrote %d bytes; want %d", n, len(clientPreface))
		t.Close(err)
		return err
	}
	var ss []frame.Setting

	if t.opts.InitialWindowSize >= defaultWindowSize {
		ss = append(ss, frame.Setting{
			ID:  frame.SettingInitialWindowSize,
			Val: uint32(t.opts.InitialWindowSize),
		})
	}

	if t.opts.MaxFrameSize != 0 {
		ss = append(ss, frame.Setting{
			ID:  frame.SettingMaxFrameSize,
			Val: t.opts.MaxFrameSize,
		})
	}
	// needs to be updated by peer.
	t.FrameSize = http2MaxFrameLen

	if t.opts.MaxHeaderListSize != nil {
		ss = append(ss, frame.Setting{
			ID:  frame.SettingMaxHeaderListSize,
			Val: *t.opts.MaxHeaderListSize,
		})
	} else {
		ss = append(ss, frame.Setting{
			ID:  frame.SettingMaxHeaderListSize,
			Val: 10485760,
		})
	}
	err = t.framer.fr.WriteSettings(ss...)
	if err != nil {
		err = connectionErrorf(true, err, "transport: failed to write initial settings frame: %v", err)
		t.Close(err)
		return err
	}
	// Adjust the connection flow control window if needed.
	if t.opts.InitialConnWindowSize > defaultWindowSize {
		if delta := uint32(t.opts.InitialConnWindowSize - defaultWindowSize); delta > 0 {
			if err := t.framer.fr.WriteWindowUpdate(0, delta); err != nil {
				err = connectionErrorf(true, err, "transport: failed to write window update: %v", err)
				t.Close(err)
				return err
			}
		}
		t.fc = &trInFlow{limit: uint32(t.opts.InitialConnWindowSize)}
	} else {
		t.fc = &trInFlow{limit: uint32(defaultWindowSize)}
	}

	t.connectionID = atomic.AddUint64(&clientConnectionCounter, 1)

	if err := t.framer.writer.Flush(); err != nil {
		t.Close(err)
		return err
	}
	t.loopy = newLoopyWriter(&t.H2Transport, clientSide, t.framer, t.controlBuf, t.bdpEst, fs)
	go func() {
		err := t.loopy.run()
		if err != nil {
			log.Printf("transport: loopyWriter.run returning. Err: %v", err)
		}
		// Do not close the transport.  Let reader goroutine handle it since
		// there might be data in the buffers.
		t.conn.Close()
		t.controlBuf.finish()
		close(t.writerDone)
	}()
	return nil
}

func (t *H2Transport) createHeaderFields(ctx context.Context, r *http.Request) ([]hpack.HeaderField, error) {
	// TODO(mmukhi): Benchmark if the performance gets better if count the metadata and other header fields
	// first and create a slice of that exact size.
	// Make the slice of certain predictable size to reduce allocations made by append.
	hfLen := 7 // :method, :scheme, :path, :authority, content-type, user-agent, te
	if r.Host == "" {
		r.Host = r.URL.Host
	}
	headerFields := make([]hpack.HeaderField, 0, hfLen)
	headerFields = append(headerFields, hpack.HeaderField{Name: ":authority", Value: r.Host})
	headerFields = append(headerFields, hpack.HeaderField{Name: ":method", Value: r.Method})
	if r.Method != "CONNECT" {
		// RFC9113: "All HTTP/2 requests MUST include exactly one valid value for the
		//   ":method", ":scheme", and ":path" pseudo-header fields, unless they
		//   are CONNECT requests"
		if r.URL == nil {
			headerFields = append(headerFields, hpack.HeaderField{Name: ":path", Value: "/"})
		} else {
			headerFields = append(headerFields, hpack.HeaderField{Name: ":path", Value: r.URL.Path})
		}
		// Scheme is not restricted to http/https, can interact with other protocols
		headerFields = append(headerFields, hpack.HeaderField{Name: ":scheme", Value: "https"})
	} else {
		p := r.Header.Get(":protocol")
		if p != "" {
			headerFields = append(headerFields, hpack.HeaderField{Name: ":protocol", Value: p})
		}
	}
	ct := r.Header.Get("content-type")
	if ct != "" {
		headerFields = append(headerFields, hpack.HeaderField{Name: "content-type", Value: ct})
		if strings.HasPrefix(ct, "application/grpc") {
			headerFields = append(headerFields, hpack.HeaderField{Name: "te", Value: "trailers"})
		}
	}

	// Send it to all requests, even if grpc specific
	//if callHdr.PreviousAttempts > 0 {
	//	headerFields = append(headerFields, hpack.HeaderField{Name: "grpc-previous-rpc-attempts", Value: strconv.Itoa(callHdr.PreviousAttempts)})
	//}

	if dl, ok := ctx.Deadline(); ok {
		// Send out timeout regardless its value. The server can detect timeout context by itself.
		// TODO(mmukhi): Perhaps this field should be updated when actually writing out to the wire.
		timeout := time.Until(dl)
		headerFields = append(headerFields, hpack.HeaderField{Name: "grpc-timeout", Value: grpcutil.EncodeDuration(timeout)})
	}

	for k, vv := range r.Header {
		if isReservedHeaderReq(k) {
			continue
		}
		for _, v := range vv {
			headerFields = append(headerFields, hpack.HeaderField{Name: strings.ToLower(k), Value: encodeMetadataHeader(k, v)})
		}
	}
	return headerFields, nil
}

// NewStreamError wraps an error and reports additional information.  Typically
// NewStream errors result in transparent retry, as they mean nothing went onto
// the wire.  However, there are two notable exceptions:
//
//  1. If the stream headers violate the max header list size allowed by the
//     server.  It's possible this could succeed on another transport, even if
//     it's unlikely, but do not transparently retry.
//  2. If the credentials errored when requesting their headers.  In this case,
//     it's possible a retry can fix the problem, but indefinitely transparently
//     retrying is not appropriate as it is likely the credentials, if they can
//     eventually succeed, would need I/O to do so.
type NewStreamError struct {
	Err error

	AllowTransparentRetry bool
}

func (e NewStreamError) Error() string {
	return e.Err.Error()
}

// writeRequestHeaders send headers and registers the stream.
func (t *H2Transport) writeRequestHeaders(s *H2Stream, headerFields []hpack.HeaderField) error {
	// If it can't be sent or becomes orphan.
	cleanup := func(err error) {
		// TODO: stream.Close merge ?
		// The stream was unprocessed by the server.
		atomic.StoreUint32(&s.unprocessed, 1)
		t.closeStream(s, err, true, frame.ErrCodeStreamClosed)
	}
	hdr := &headerFrame{
		hf:        headerFields,
		endStream: false,
		initStream: func(id uint32) error {
			t.mu.Lock()
			if t.closing {
				t.mu.Unlock()
				// Do a quick cleanup.
				err := ErrConnClosing
				cleanup(err)
				return err
			}
			t.activeStreams[id] = s
			atomic.AddInt64(&t.streamsStarted, 1)
			atomic.StoreInt64(&t.LastStreamCreatedTime, time.Now().UnixNano())
			if t.kpDormant {
				t.kpDormancyCond.Signal()
			}
			t.mu.Unlock()
			return nil
		},
		onWrite: func() {
			t.streamEvent(Event_WroteHeaders, s)
		},
		onOrphaned: cleanup,
		wq:         s.outFlow,
	}
	firstTry := true
	var ch chan struct{}
	checkForStreamQuota := func(it interface{}) bool {
		if t.IsServer() {
			// No quota on server initiated streams.
			h := it.(*headerFrame)
			h.streamID = t.nextID
			t.nextID += 2
			s.Id = h.streamID
			s.inFlow = &inFlow{limit: uint32(t.initialWindowSize)}
			return true
		}
		if t.streamQuota <= 0 { // Can go negative if server decreases it.
			if firstTry {
				t.waitingStreams++
			}
			ch = t.streamsQuotaAvailable
			return false
		}
		if !firstTry {
			t.waitingStreams--
		}
		t.streamQuota--
		h := it.(*headerFrame)
		h.streamID = t.nextID
		t.nextID += 2
		s.Id = h.streamID
		s.inFlow = &inFlow{limit: uint32(t.initialWindowSize)}
		if t.streamQuota > 0 && t.waitingStreams > 0 {
			select {
			case t.streamsQuotaAvailable <- struct{}{}:
			default:
			}
		}
		return true
	}
	var hdrListSizeErr error
	checkForHeaderListSize := func(it interface{}) bool {
		if t.maxSendHeaderListSize == nil {
			return true
		}
		hdrFrame := it.(*headerFrame)
		var sz int64
		for _, f := range hdrFrame.hf {
			if sz += int64(f.Size()); sz > int64(*t.maxSendHeaderListSize) {
				hdrListSizeErr = &Status{Message: fmt.Sprintf("header list size to send violates the maximum size (%d bytes) set by server", *t.maxSendHeaderListSize)}
				return false
			}
		}
		return true
	}
	for {
		success, err := t.controlBuf.executeAndPut(func(it interface{}) bool {
			if !checkForStreamQuota(it) {
				return false
			}
			if !checkForHeaderListSize(it) {
				return false
			}
			return true
		}, hdr)
		if err != nil {
			// Connection closed.
			return &NewStreamError{Err: err, AllowTransparentRetry: true}
		}
		if success {
			break
		}
		if hdrListSizeErr != nil {
			return &NewStreamError{Err: hdrListSizeErr}
		}
		firstTry = false
		ctx := s.ctx
		select {
		case <-ch:
		case <-ctx.Done():
			return &NewStreamError{Err: ContextErr(ctx.Err())}
		case <-t.goAway:
			return &NewStreamError{Err: errStreamDrain, AllowTransparentRetry: true}
		case <-t.ctx.Done():
			return &NewStreamError{Err: ErrConnClosing, AllowTransparentRetry: true}
		}
	}
	return nil
}

// closeStream unblocks readBlocking/write, delete the stream from active, updates stats, etc.
// Will be called once (guarded by streamDone)
//
// For all error conditions:
// If rst and rstCode are set, will also send a RST frame.
//
//	for graceful close, part of stream.Close()
//
// Else it will send a RST frame if the reader has not received an EOF yet.
//
// err is the error causing the stream close - will be returned to Read() if blocking readBlocking is in
// progress.
func (t *H2Transport) closeStream(s *H2Stream, err error, rst bool, rstCode frame.ErrCode) {

	if s.swapState(streamDone) == streamDone {
		// If it was already done, return.  If multiple closeStream calls
		// happen simultaneously, wait for the first to finish.
		<-s.done
		return
	}

	if s.Error == nil && err != io.EOF {
		s.Error = err
	}

	// status and trailers can be updated here without any synchronization because the stream goroutine will
	// only readBlocking it after it sees an io.EOF error from readBlocking or write and we'll write those errors
	// only after updating this.
	if err != nil { //&& s.trReader.Err == nil {
		s.setReadClosed(2, err)
	}

	// If headerChan isn't closed, then close it.
	if s.headerChan != nil && atomic.CompareAndSwapUint32(&s.headerChanClosed, 0, 1) {
		s.noHeaders = true
		close(s.headerChan)
	}

	cleanup := &cleanupStream{
		streamID: s.Id,
		onWrite: func() {
			t.deleteStream(s)
		},
		rst:     rst,
		rstCode: rstCode,
	}

	addBackStreamQuota := func(interface{}) bool {
		t.streamQuota++
		if t.streamQuota > 0 && t.waitingStreams > 0 {
			select {
			case t.streamsQuotaAvailable <- struct{}{}:
			default:
			}
		}
		return true
	}
	t.controlBuf.executeAndPut(addBackStreamQuota, cleanup)
	// This will unblock write.

	if err == nil {
		atomic.AddInt64(&t.streamsSucceeded, 1)
	} else {
		atomic.AddInt64(&t.streamsFailed, 1)
	}

	// In case stream sending and receiving are invoked in separate
	// goroutines (e.g., bi-directional streaming), cancel needs to be
	// called to interrupt the potential blocking on other goroutines.
	if s.cancel != nil {
		s.cancel()
		s.cancel = nil
	}

	if s.done != nil {
		close(s.done)
	}
	s.StreamEvent(EventStreamClosed)
}

func (t *H2Transport) getStream(f frame.Frame) *H2Stream {
	t.mu.Lock()
	if t.activeStreams == nil {
		t.mu.Unlock()
		return nil
	}
	s := t.activeStreams[f.Header().StreamID]
	t.mu.Unlock()
	return s
}

func (t *H2Transport) adjustWindow(s *H2Stream, n uint32) {
	if w := s.inFlow.maybeAdjust(n); w > 0 {
		t.controlBuf.put(&outgoingWindowUpdate{streamID: s.Id, increment: w})
	}
}

// updateWindow adjusts the inbound quota for the stream.
// Window updates will be sent out when the cumulative quota
// exceeds the corresponding threshold.
func (t *H2Transport) UpdateWindow(s *H2Stream, n uint32) {
	if w := s.inFlow.onRead(n); w > 0 {
		t.controlBuf.put(&outgoingWindowUpdate{streamID: s.Id, increment: w})
	}
}

const SettingH2R = 0xF051

// updateFlowControl updates the incoming flow control windows
// for the transport and the stream based on the current bdp
// estimation. BPD is used to determine initial window size for new
// streams and connection based on the RTT. Only enabled if the initial
// window size is not set explicitly
func (t *H2Transport) updateFlowControl(n uint32) {
	t.mu.Lock()
	for _, s := range t.activeStreams {
		s.inFlow.newLimit(n)
	}
	t.initialWindowSize = int32(n)
	t.mu.Unlock()

	t.controlBuf.put(
		&outgoingWindowUpdate{streamID: 0, increment: t.fc.newLimit(n)})

	t.controlBuf.put(&outgoingSettings{
		ss: []frame.Setting{
			{
				ID:  frame.SettingInitialWindowSize,
				Val: n,
			},
		},
	})
}

func (t *H2Transport) handleData(f *frame.DataFrame) {
	size := f.Header().Length
	var sendBDPPing bool
	if t.bdpEst != nil {
		sendBDPPing = t.bdpEst.add(size)
	}
	// Decouple connection's flow control from application's readBlocking.
	// An update on connection's flow control should not depend on
	// whether user application has readBlocking the data or not. Such a
	// restriction is already imposed on the stream's flow control,
	// and therefore the sender will be blocked anyways.
	// Decoupling the connection flow control will prevent other
	// active(fast) streams from starving in presence of slow or
	// inactive streams.
	//
	if w := t.fc.onData(size); w > 0 {
		t.controlBuf.put(&outgoingWindowUpdate{
			streamID:  0,
			increment: w,
		})
	}
	if sendBDPPing {
		// Avoid excessive ping detection (e.g. in an L7 proxy)
		// by sending a window update prior to the BDP ping.
		if w := t.fc.reset(); w > 0 {
			t.controlBuf.put(&outgoingWindowUpdate{
				streamID:  0,
				increment: w,
			})
		}
		t.controlBuf.put(bdpPing)
	}
	// Select the right stream to dispatch.
	s := t.getStream(f)
	if s == nil {
		return
	}
	if s.getReadClosed() == 2 {
		t.closeStream(s, errStreamDone, true, frame.ErrCodeStreamClosed)
		return
	}
	if size > 0 {
		if err := s.inFlow.onData(size); err != nil {
			t.closeStream(s, err, true, frame.ErrCodeFlowControl)
			return
		}
		if f.Header().Flags.Has(frame.FlagDataPadded) {
			if w := s.inFlow.onRead(size - uint32(len(f.Data()))); w > 0 {
				t.controlBuf.put(&outgoingWindowUpdate{s.Id, w})
			}
		}

		if s.OnData != nil {
			s.OnData(f)
			if f.StreamEnded() {
				s.setReadClosed(1, io.EOF)
			}
			return
		}

		// Old code - with data frame reuse immediately:
		// TODO(bradfitz, zhaoq): A copy is required here because there is no
		// guarantee f.Data() is consumed before the arrival of next frame.
		// Can this copy be eliminated?
		if len(f.Data()) > 0 {
			//buffer := t.dataFramePool.waitOrConsumeQuota()
			//buffer.Reset()
			//buffer.Write(f.Data())

			s.RcvdBytes += len(f.Data())
			s.RcvdPackets++
			s.LastRead = time.Now()
			s.inFrameList.Put(nio.RecvMsg{Buffer: bytes.NewBuffer(f.Data())})
		}
	}
	if f.StreamEnded() {
		s.setReadClosed(1, io.EOF)
	}
}

func (t *H2Transport) handleRSTStream(f *frame.RSTStreamFrame) {
	s := t.getStream(f)
	if s == nil {
		// If the stream is already deleted from the active streams map, then Put a cleanupStream item into controlbuf to delete the stream from loopy writer's established streams map.
		t.controlBuf.put(&cleanupStream{
			streamID: f.Header().StreamID,
			rst:      false,
			rstCode:  0,
			onWrite:  func() {},
		})
		return
	}

	if s.getReadClosed() == 1 {
		// EOF received
		return
	}

	if f.ErrCode == frame.ErrCodeRefusedStream {
		// The stream was unprocessed by the server.
		atomic.StoreUint32(&s.unprocessed, 1)
	}

	statusCode, ok := http2ErrConvTab[f.ErrCode]
	if !ok {
		statusCode = Unknown
	}
	if statusCode == Canceled {
		if d, ok := s.ctx.Deadline(); ok && !d.After(time.Now()) {
			// Our deadline was already exceeded, and that was likely the cause
			// of this cancelation.  Alter the status code accordingly.
			statusCode = DeadlineExceeded
		}
	}

	s.setReadClosed(2, errStreamRST)

	if atomic.CompareAndSwapUint32(&s.headerChanClosed, 0, 1) {
		if s.headerChan != nil {
			close(s.headerChan)
		}
	}
}

func (t *H2ClientTransport) handleSettings(f *frame.SettingsFrame, isFirst bool) {
	if f.IsAck() {
		return
	}

	var maxStreams *uint32

	var ss []frame.Setting
	var updateFuncs []func()
	var fs *uint32

	f.ForeachSetting(func(s frame.Setting) error {
		switch s.ID {
		case frame.SettingMaxConcurrentStreams:
			maxStreams = new(uint32)
			*maxStreams = s.Val
		case frame.SettingMaxFrameSize:
			fs = new(uint32)
			*fs = s.Val
			rfs := *fs
			if t.FrameSize > rfs {
				t.FrameSize = rfs
			}
		case frame.SettingMaxHeaderListSize:
			updateFuncs = append(updateFuncs, func() {
				t.maxSendHeaderListSize = new(uint32)
				*t.maxSendHeaderListSize = s.Val
			})
		default:
			ss = append(ss, s)
		}
		return nil
	})
	if isFirst && maxStreams == nil {
		maxStreams = new(uint32)
		*maxStreams = math.MaxUint32
	}
	// InitialWindowSize and HeaderTableSize from peer - updated in loopy
	sf := &incomingSettings{
		ss: ss,
	}
	if maxStreams != nil {
		updateStreamQuota := func() {
			delta := int64(*maxStreams) - int64(t.maxConcurrentStreams)
			t.maxConcurrentStreams = *maxStreams
			t.streamQuota += delta
			if delta > 0 && t.waitingStreams > 0 {
				close(t.streamsQuotaAvailable) // wake all of them up.
				t.streamsQuotaAvailable = make(chan struct{}, 1)
			}
		}
		updateFuncs = append(updateFuncs, updateStreamQuota)
	}
	t.controlBuf.executeAndPut(func(interface{}) bool {
		for _, f := range updateFuncs {
			f()
		}
		return true
	}, sf)

	t.MuxEvent(Event_Settings)
}

func (t *H2ClientTransport) handlePing(f *frame.PingFrame) {
	if f.IsAck() {
		// Maybe it's a BDP ping.
		if t.bdpEst != nil {
			t.bdpEst.calculate(f.Data)
		}
		return
	}
	pingAck := &ping{ack: true}
	copy(pingAck.data[:], f.Data[:])
	t.controlBuf.put(pingAck)
}

func (t *H2ClientTransport) handleGoAway(f *frame.GoAwayFrame) {
	t.mu.Lock()
	if t.closing {
		t.mu.Unlock()
		return
	}
	id := f.LastStreamID
	if id > 0 && id%2 == 0 {
		t.mu.Unlock()
		t.Close(connectionErrorf(true, nil, "received goaway with non-zero even-numbered numbered stream id: %v", id))
		return
	}
	// A client can receive multiple GoAways from the server (see
	// https://github.com/grpc/grpc-go/issues/1387).  The idea is that the first
	// GoAway will be sent with an WorkloadID of MaxInt32 and the second GoAway will be
	// sent after an RTT delay with the WorkloadID of the last stream the server will
	// process.
	//
	// Therefore, when we waitOrConsumeQuota the first GoAway we don't necessarily close any
	// streams. While in case of second GoAway we close all streams created after
	// the GoAwayId. This way streams that were in-flight while the GoAway from
	// server was being sent don't waitOrConsumeQuota killed.
	select {
	case <-t.goAway: // t.goAway has been closed (i.e.,multiple GoAways).
		// If there are multiple GoAways the first one should always have an WorkloadID greater than the following ones.
		if id > t.prevGoAwayID {
			t.mu.Unlock()
			t.Close(connectionErrorf(true, nil, "received goaway with stream id: %v, which exceeds stream id of previous goaway: %v", id, t.prevGoAwayID))
			return
		}
	default:
		t.setGoAwayReason(f)
		close(t.goAway)
		t.controlBuf.put(&incomingGoAway{})
		// Notify the clientconn about the GOAWAY before we set the state to
		// draining, to allow the client to stop attempting to create streams
		// before disallowing new streams on this connection.
		t.MuxEvent(Event_GoAway)
		t.closing = true
	}
	// All streams with IDs greater than the GoAwayId
	// and smaller than the previous GoAway WorkloadID should be killed.
	upperLimit := t.prevGoAwayID
	if upperLimit == 0 { // This is the first GoAway Frame.
		upperLimit = math.MaxUint32 // Kill all streams after the GoAway WorkloadID.
	}
	for streamID, stream := range t.activeStreams {
		if streamID > id && streamID <= upperLimit {
			// The stream was unprocessed by the server.
			atomic.StoreUint32(&stream.unprocessed, 1)
			t.closeStream(stream, errStreamDrain, false, frame.ErrCodeNo)
		}
	}
	t.prevGoAwayID = id
	active := len(t.activeStreams)
	t.mu.Unlock()
	if active == 0 {
		t.Close(connectionErrorf(true, nil, "received goaway and there are no active streams"))
	}
}

// setGoAwayReason sets the value of t.goAwayReason based
// on the GoAway frame received.
// It expects a lock on transport's mutext to be held by
// the caller.
func (t *H2ClientTransport) setGoAwayReason(f *frame.GoAwayFrame) {
	t.goAwayReason = GoAwayNoReason
	switch f.ErrCode {
	case frame.ErrCodeEnhanceYourCalm:
		if string(f.DebugData()) == "too_many_pings" {
			t.goAwayReason = GoAwayTooManyPings
		}
	}
	if len(f.DebugData()) == 0 {
		t.goAwayDebugMessage = fmt.Sprintf("code: %s", f.ErrCode)
	} else {
		t.goAwayDebugMessage = fmt.Sprintf("code: %s, debug data: %q", f.ErrCode, string(f.DebugData()))
	}
}

func (t *H2ClientTransport) GetGoAwayReason() (GoAwayReason, string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.goAwayReason, t.goAwayDebugMessage
}

func (t *H2Transport) handleWindowUpdate(f *frame.WindowUpdateFrame) {
	t.controlBuf.put(&incomingWindowUpdate{
		streamID:  f.Header().StreamID,
		increment: f.Increment,
	})
}

func (t *H2Transport) IsServer() bool {
	return t.loopy.side != clientSide
}

// operateResponseHeaders takes action on the decoded for a response.
func (t *H2Transport) operateClientResponseHeaders(f *frame.MetaHeadersFrame) {
	if f.StreamID%2 == 0 && !t.IsServer() {
		// Push or h2r
		t.operateAcceptHeaders(f)
		return
	}
	t.operateResponseHeaders(f)
}

func (t *H2Transport) operateResponseHeaders(f *frame.MetaHeadersFrame) {
	if f.StreamID%2 == 0 {
		// Push or h2r

	}
	s := t.getStream(f)
	if s == nil {
		return
	}
	endStream := f.StreamEnded()
	atomic.StoreUint32(&s.bytesReceived, 1)
	initialHeader := atomic.LoadUint32(&s.headerChanClosed) == 0

	if !initialHeader && !endStream {
		// As specified by gRPC over HTTP2, a HEADERS frame (and associated CONTINUATION frames) can only appear at the start or end of a stream. Therefore, second HEADERS frame must have EOS bit set.
		st := &Status{Message: "a HEADERS frame cannot appear in the middle of a stream"}
		t.closeStream(s, st, true, frame.ErrCodeProtocol)
		return
	}

	// frame.Truncated is set to true when framer detects that the current header
	// list size hits MaxHeaderListSize limit.
	if f.Truncated {
		se := &Status{Message: "peer header list size exceeded limit"}
		t.closeStream(s, se, true, frame.ErrCodeFrameSize)
		return
	}

	trailer := false
	// If headerChan hasn't been closed yet
	if s.headerChanClosed != 0 {
		trailer = true
	}

	for _, hf := range f.Fields {
		switch hf.Name {
		case ":status":
			s.Response.Status = hf.Value
			s.Response.StatusCode, _ = strconv.Atoi(hf.Value)
		default:
			if trailer {
				v, err := decodeMetadataHeader(hf.Name, hf.Value)
				if err != nil {
					log.Printf("Failed to decode metadata header (%q, %q): %v", hf.Name, hf.Value, err)
					break
				}
				s.Response.Trailer.Add(hf.Name, v)
			} else {
				if isReservedHeader(hf.Name) && !isWhitelistedHeader(hf.Name) {
					break
				}
				v, err := decodeMetadataHeader(hf.Name, hf.Value)
				if err != nil {
					log.Printf("Failed to decode metadata header (%q, %q): %v", hf.Name, hf.Value, err)
					break
				}
				s.Response.Header.Add(hf.Name, v)
			}
		}
	}

	// If headerChan hasn't been closed yet
	if atomic.CompareAndSwapUint32(&s.headerChanClosed, 0, 1) {
		close(s.headerChan)
	}

	if !endStream {
		return
	}

	s.setReadClosed(1, io.EOF)
	s.queueReadBuf(nio.RecvMsg{Err: io.EOF})

	// Old: if client received END_STREAM from server while stream was still active, send RST_STREAM
}

// reader runs as a separate goroutine in charge of reading data from network
// connection.
//
// TODO(zhaoq): currently one reader per transport. Investigate whether this is
// optimal.
// TODO(zhaoq): Check the validity of the incoming frame sequence.
func (t *H2ClientTransport) reader() {
	defer close(t.readerDone)
	// Check the validity of server preface.
	f, err := t.framer.fr.ReadFrame()
	if err != nil {
		err = connectionErrorf(true, err, "error reading server preface: %v", err)
		t.Close(err) // this kicks off resetTransport, so must be last before return
		return
	}
	t.conn.SetReadDeadline(time.Time{}) // reset deadline once we waitOrConsumeQuota the settings frame (we didn't time out, yay!)
	atomic.StoreInt64(&t.LastRead, time.Now().UnixNano())
	sf, ok := f.(*frame.SettingsFrame)
	if !ok {
		// this kicks off resetTransport, so must be last before return
		t.Close(connectionErrorf(true, nil, "initial http2 frame from server is not a settings frame: %T", f))
		return
	}
	t.PeerSettings = sf
	t.handleSettings(sf, true)

	// loop to keep reading incoming messages on this transport.
	for {
		t.controlBuf.throttle()
		f, err := t.framer.fr.ReadFrame()
		atomic.StoreInt64(&t.LastRead, time.Now().UnixNano())
		if err != nil {
			// Abort an active stream if the http2.Framer returns a
			// http2.StreamError. This can happen only if the server's response
			// is malformed http2.
			if se, ok := err.(frame.StreamError); ok {
				t.mu.Lock()
				s := t.activeStreams[se.StreamID]
				t.mu.Unlock()
				if s != nil {
					// use error detail to provide better err message
					code := http2ErrConvTab[se.Code]
					errorDetail := t.framer.fr.ErrorDetail()
					var msg string
					if errorDetail != nil {
						msg = errorDetail.Error()
					} else {
						msg = "received invalid frame"
					}
					t.closeStream(s, &Status{Code: int(code), Message: msg}, true, frame.ErrCodeProtocol)
				}
				continue
			} else {
				// Transport error.
				t.Close(connectionErrorf(true, err, "error reading from server: %v", err))
				return
			}
		}
		switch frame := f.(type) {
		case *frame.MetaHeadersFrame:
			t.operateClientResponseHeaders(frame)
		case *frame.DataFrame:
			t.handleData(frame)
		case *frame.RSTStreamFrame:
			t.handleRSTStream(frame)
		case *frame.SettingsFrame:
			t.handleSettings(frame, false)
		case *frame.PingFrame:
			t.handlePing(frame)
		case *frame.GoAwayFrame:
			t.handleGoAway(frame)
		case *frame.WindowUpdateFrame:
			t.handleWindowUpdate(frame)
		default:
			log.Printf("transport: H2ClientTransport.reader got unhandled frame type %v.", frame)
		}
	}
}

func minTime(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}

// keepalive running in a separate goroutine makes sure the connection is alive by sending pings.
func (t *H2ClientTransport) keepalive() {
	p := &ping{data: [8]byte{}}
	// True iff a ping has been sent, and no data has been received since then.
	outstandingPing := false
	// Amount of time remaining before which we should receive an ACK for the
	// last sent ping.
	timeoutLeft := time.Duration(0)
	// Records the last value of t.lastRead before we go block on the timer.
	// This is required to check for readBlocking activity since then.
	prevNano := time.Now().UnixNano()
	timer := time.NewTimer(t.KeepaliveParams.Time)
	for {
		select {
		case <-timer.C:
			lastRead := atomic.LoadInt64(&t.LastRead)
			if lastRead > prevNano {
				// There has been readBlocking activity since the last time we were here.
				outstandingPing = false
				// Next timer should fire at kp.Time seconds from lastRead time.
				timer.Reset(time.Duration(lastRead) + t.KeepaliveParams.Time - time.Duration(time.Now().UnixNano()))
				prevNano = lastRead
				continue
			}
			if outstandingPing && timeoutLeft <= 0 {
				t.Close(connectionErrorf(true, nil, "keepalive ping failed to receive ACK within timeout"))
				return
			}
			t.mu.Lock()
			if t.closing {
				// If the transport is closing, we should exit from the
				// keepalive goroutine here. If not, we could have a race
				// between the call to Signal() from Close() and the call to
				// Wait() here, whereby the keepalive goroutine ends up
				// blocking on the condition variable which will never be
				// signalled again.
				t.mu.Unlock()
				return
			}
			if len(t.activeStreams) < 1 && !t.KeepaliveParams.PermitWithoutStream {
				// If a ping was sent out previously (because there were active
				// streams at that point) which wasn't acked and its timeout
				// hadn't fired, but we got here and are about to go dormant,
				// we should make sure that we unconditionally send a ping once
				// we awaken.
				outstandingPing = false
				t.kpDormant = true
				t.kpDormancyCond.Wait()
			}
			t.kpDormant = false
			t.mu.Unlock()

			// We waitOrConsumeQuota here either because we were dormant and a new stream was
			// created which unblocked the Wait() call, or because the
			// keepalive timer expired. In both cases, we need to send a ping.
			if !outstandingPing {
				//if channelz.IsOn() {
				//	atomic.AddInt64(&t.czData.kpCount, 1)
				//}
				t.controlBuf.put(p)
				timeoutLeft = t.KeepaliveParams.Timeout
				outstandingPing = true
			}
			// The amount of time to sleep here is the minimum of kp.Time and
			// timeoutLeft. This will ensure that we wait only for kp.Time
			// before sending out the next ping (for cases where the ping is
			// acked).
			sleepDuration := minTime(t.KeepaliveParams.Time, timeoutLeft)
			timeoutLeft -= sleepDuration
			timer.Reset(sleepDuration)
		case <-t.ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return
		}
	}
}

func (t *H2ClientTransport) Error() <-chan struct{} {
	return t.done
}

func (t *H2ClientTransport) GoAway() <-chan struct{} {
	return t.goAway
}

func (t *H2ClientTransport) CanTakeNewRequest() bool {
	return !t.closing
}

func (t *H2Transport) getOutFlowWindow() int64 {
	resp := make(chan uint32, 1)
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	t.controlBuf.put(&outFlowControlSizeRequest{resp})
	select {
	case sz := <-resp:
		return int64(sz)
	case <-t.done:
		return -1
	case <-timer.C:
		return -2
	}
}

func (t *H2Transport) Conn() net.Conn {
	return t.conn
}
