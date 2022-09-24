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
)

// clientConnectionCounter counts the number of connections a client has
// initiated (equal to the number of http2Clients created). Must be accessed
// atomically.
var clientConnectionCounter uint64

// HTTP2ClientMux implements the ClientTransport interface with HTTP2.
type HTTP2ClientMux struct {
	lastRead  int64 // Keep this field 64-bit aligned. Accessed atomically.
	ctx       context.Context
	cancel    context.CancelFunc
	ctxDone   <-chan struct{} // Cache the ctx.Done() chan.
	userAgent string
	conn      net.Conn // underlying communication channel
	loopy     *loopyWriter

	readerDone chan struct{} // sync point to enable testing.
	writerDone chan struct{} // sync point to enable testing.
	// goAway is closed to notify the upper layer (i.e., addrConn.transportMonitor)
	// that the server sent GoAway on this transport.
	goAway chan struct{}

	framer *framer
	// controlBuf delivers all the control related tasks (e.g., window
	// updates, reset streams, and various settings) to the controller.
	controlBuf *controlBuffer
	fc         *trInFlow
	// The scheme used: https if TLS is on, http otherwise.
	scheme string

	isSecure bool

	kp               ClientParameters
	keepaliveEnabled bool

	initialWindowSize int32

	// configured by peer through SETTINGS_MAX_HEADER_LIST_SIZE
	maxSendHeaderListSize *uint32

	bdpEst *bdpEstimator
	// onPrefaceReceipt is a callback that client transport calls upon
	// receiving server preface to signal that a succefull HTTP2
	// connection was established.
	onPrefaceReceipt func(settingsFrame *frame.SettingsFrame)

	maxConcurrentStreams  uint32
	streamQuota           int64
	streamsQuotaAvailable chan struct{}
	waitingStreams        uint32
	nextID                uint32

	mu            sync.Mutex // guard the following variables
	state         transportState
	activeStreams map[uint32]*Stream
	// prevGoAway ID records the Last-Stream-ID in the previous GOAway frame.
	prevGoAwayID uint32
	// goAwayReason records the http2.ErrCode and debug data received with the
	// GoAway frame.
	goAwayReason GoAwayReason
	// goAwayDebugMessage contains a detailed human readable string about a
	// GoAway frame, useful for error messages.
	goAwayDebugMessage string
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

	// Fields below are for channelz metric collection
	czData *channelzData

	onGoAway func(GoAwayReason)
	onClose  func()

	bufferPool *bufferPool

	connectionID uint64

	FrameSize uint32
}

func (t *HTTP2ClientMux) createHeaderFields(ctx context.Context,
	r *http.Request) ([]hpack.HeaderField, error) {
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
		headerFields = append(headerFields, hpack.HeaderField{Name: ":path", Value: r.URL.Path})
		headerFields = append(headerFields, hpack.HeaderField{Name: ":scheme", Value: "https"})
	}
	ct := r.Header.Get("content-type")
	headerFields = append(headerFields, hpack.HeaderField{Name: "content-type", Value: ct})

	if strings.HasPrefix(ct, "application/grpc") {
		headerFields = append(headerFields, hpack.HeaderField{Name: "te", Value: "trailers"})
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

func (t *HTTP2ClientMux) sendHeaders(s *Stream, headerFields []hpack.HeaderField) (*Stream, error) {
	cleanup := func(err error) {
		if s.swapState(streamDone) == streamDone {
			// If it was already done, return.
			return
		}
		// The stream was unprocessed by the server.
		atomic.StoreUint32(&s.unprocessed, 1)
		if s.getReadClosed() == 0 {
			s.queueReadBuf(recvMsg{err: err})
		}
		if s.getWriteClosed() == 0 {
			// TODO(costin)
		}
		close(s.done)
		// If headerChan isn't closed, then close it.
		if atomic.CompareAndSwapUint32(&s.headerChanClosed, 0, 1) {
			close(s.headerChan)
		}
	}
	hdr := &headerFrame{
		hf:        headerFields,
		endStream: false,
		initStream: func(id uint32) error {
			t.mu.Lock()
			if state := t.state; state != reachable {
				t.mu.Unlock()
				// Do a quick cleanup.
				var err error
				err = errStreamDrain
				if state == closing {
					err = ErrConnClosing
				}
				cleanup(err)
				return err
			}
			t.activeStreams[id] = s
			//if channelz.IsOn() {
			//	atomic.AddInt64(&t.czData.streamsStarted, 1)
			//	atomic.StoreInt64(&t.czData.lastStreamCreatedTime, time.Now().UnixNano())
			//}
			// If the keepalive goroutine has gone dormant, wake it up.
			if t.kpDormant {
				t.kpDormancyCond.Signal()
			}
			t.mu.Unlock()
			return nil
		},
		onOrphaned: cleanup,
		wq:         s.wq,
	}
	firstTry := true
	var ch chan struct{}
	checkForStreamQuota := func(it interface{}) bool {
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
		s.fc = &inFlow{limit: uint32(t.initialWindowSize)}
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
			return nil, &NewStreamError{Err: err, AllowTransparentRetry: true}
		}
		if success {
			break
		}
		if hdrListSizeErr != nil {
			return nil, &NewStreamError{Err: hdrListSizeErr}
		}
		firstTry = false
		ctx := s.ctx
		select {
		case <-ch:
		case <-ctx.Done():
			return nil, &NewStreamError{Err: ContextErr(ctx.Err())}
		case <-t.goAway:
			return nil, &NewStreamError{Err: errStreamDrain, AllowTransparentRetry: true}
		case <-t.ctx.Done():
			return nil, &NewStreamError{Err: ErrConnClosing, AllowTransparentRetry: true}
		}
	}
	return s, nil
}

// CloseStream clears the footprint of a stream when the stream is not needed any more.
// This must not be executed in reader's goroutine.
func (t *HTTP2ClientMux) CloseStream(s *Stream, err error) {
	var (
		rst     bool
		rstCode frame.ErrCode
	)
	if err != nil {
		rst = true
		rstCode = frame.ErrCodeCancel
	}
	t.closeStream(s, err, rst, rstCode, &Status{Err: err}, nil, false)
}

func (t *HTTP2ClientMux) closeStream(s *Stream, err error, rst bool, rstCode frame.ErrCode,
	st *Status, mdata map[string][]string, eosReceived bool) {

	//log.Println("Client closeStream", s.Id, err, rst, rstCode)

	// Set stream status to done.
	if s.swapState(streamDone) == streamDone {
		// If it was already done, return.  If multiple closeStream calls
		// happen simultaneously, wait for the first to finish.
		<-s.done
		return
	}

	// status and trailers can be updated here without any synchronization because the stream goroutine will
	// only read it after it sees an io.EOF error from read or write and we'll write those errors
	// only after updating this.
	s.Status = st
	if err != nil {
		s.setReadClosed(2)
		// This will unblock reads eventually.
		s.queueReadBuf(recvMsg{err: err})
	}
	// If headerChan isn't closed, then close it.
	if atomic.CompareAndSwapUint32(&s.headerChanClosed, 0, 1) {
		s.noHeaders = true
		close(s.headerChan)
	}
	cleanup := &cleanupStream{
		streamID: s.Id,
		onWrite: func() {
			t.mu.Lock()
			if t.activeStreams != nil {
				delete(t.activeStreams, s.Id)
			}
			t.mu.Unlock()
			//if channelz.IsOn() {
			//	if eosReceived {
			//		atomic.AddInt64(&t.czData.streamsSucceeded, 1)
			//	} else {
			//		atomic.AddInt64(&t.czData.streamsFailed, 1)
			//	}
			//}
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
	close(s.done)
	if s.DoneFunc != nil {
		s.DoneFunc(s)
	}
}

// Close kicks off the shutdown process of the transport. This should be called
// only once on a transport. Once it is called, the transport should not be
// accessed any more.
//
// This method blocks until the addrConn that initiated this transport is
// re-connected. This happens because t.onClose() begins reconnect logic at the
// addrConn level and blocks until the addrConn is successfully connected.
func (t *HTTP2ClientMux) Close(err error) {
	t.mu.Lock()
	// Make sure we only Close once.
	if t.state == closing {
		t.mu.Unlock()
		return
	}
	// Call t.onClose before setting the state to closing to prevent the client
	// from attempting to create new streams ASAP.
	t.onClose()
	t.state = closing
	streams := t.activeStreams
	t.activeStreams = nil
	if t.kpDormant {
		// If the keepalive goroutine is blocked on this condition variable, we
		// should unblock it so that the goroutine eventually exits.
		t.kpDormancyCond.Signal()
	}
	t.mu.Unlock()
	t.controlBuf.finish()
	t.cancel()
	t.conn.Close()
	//channelz.RemoveEntry(t.channelzID)
	// Append info about previous goaways if there were any, since this may be important
	// for understanding the root cause for this connection to be closed.
	_, goAwayDebugMessage := t.GetGoAwayReason()

	var st *Status
	if len(goAwayDebugMessage) > 0 {
		st = &Status{Code: int(Unavailable), Message: fmt.Sprintf("closing transport due to: %v, received prior goaway: %v", err, goAwayDebugMessage)}
		err = st
	} else {
		st = &Status{Code: int(Unavailable), Err: err}
	}

	// Notify all active streams.
	for _, s := range streams {
		t.closeStream(s, err, false, frame.ErrCodeNo, st, nil, false)
	}
	// TODO(telemetry): Handle conn end
}

// GracefulClose sets the state to draining, which prevents new streams from
// being created and causes the transport to be closed when the last active
// stream is closed.  If there are no active streams, the transport is closed
// immediately.  This does nothing if the transport is already draining or
// closing.
func (t *HTTP2ClientMux) GracefulClose() {
	t.mu.Lock()
	// Make sure we move to draining only from active.
	if t.state == draining || t.state == closing {
		t.mu.Unlock()
		return
	}
	t.state = draining
	active := len(t.activeStreams)
	t.mu.Unlock()
	if active == 0 {
		t.Close(ErrConnClosing)
		return
	}
	t.controlBuf.put(&incomingGoAway{})
}

// Write formats the data into HTTP2 data frame(s) and sends it out. The caller
// should proceed only if Write returns nil.
func (t *HTTP2ClientMux) Write(s *Stream, hdr []byte, data []byte, last bool) error {
	if s.getWriteClosed() != 0 {
		return errStreamDone // nil // already closed
	}

	if last {
		if s.getWriteClosed() != 0 {
			return nil // already closed
		}
		s.setWriteClosed(1)
	}

	df := &dataFrame{
		streamID:  s.Id,
		endStream: last,
		d:         data,
		onDone:    s.writeDoneChan,
	}
	if hdr != nil || data != nil { // If it's not an empty data frame, check quota.
		if err := s.wq.get(int32(len(hdr) + len(data))); err != nil {
			return err
		}
	}
	er := t.controlBuf.put(df)
	if er != nil {
		return er
	}
	<-s.writeDoneChan
	return nil
}

func (t *HTTP2ClientMux) getStream(f frame.Frame) *Stream {
	t.mu.Lock()
	s := t.activeStreams[f.Header().StreamID]
	t.mu.Unlock()
	return s
}

// adjustWindow sends out extra window update over the initial window size
// of stream if the application is requesting data larger in size than
// the window.
func (t *HTTP2ClientMux) adjustWindow(s *Stream, n uint32) {
	if w := s.fc.maybeAdjust(n); w > 0 {
		t.controlBuf.put(&outgoingWindowUpdate{streamID: s.Id, increment: w})
	}
}

// updateWindow adjusts the inbound quota for the stream.
// Window updates will be sent out when the cumulative quota
// exceeds the corresponding threshold.
func (t *HTTP2ClientMux) updateWindow(s *Stream, n uint32) {
	if w := s.fc.onRead(n); w > 0 {
		t.controlBuf.put(&outgoingWindowUpdate{streamID: s.Id, increment: w})
	}
}

const SettingH2R = 0xF051

// updateFlowControl updates the incoming flow control windows
// for the transport and the stream based on the current bdp
// estimation.
func (t *HTTP2ClientMux) updateFlowControl(n uint32) {
	t.mu.Lock()
	for _, s := range t.activeStreams {
		s.fc.newLimit(n)
	}
	t.mu.Unlock()
	updateIWS := func(interface{}) bool {
		t.initialWindowSize = int32(n)
		return true
	}
	t.controlBuf.executeAndPut(updateIWS, &outgoingWindowUpdate{streamID: 0, increment: t.fc.newLimit(n)})
	t.controlBuf.put(&outgoingSettings{
		ss: []frame.Setting{
			{
				ID:  frame.SettingInitialWindowSize,
				Val: n,
			},
			//{
			//	ID:  SettingH2R,
			//	Val: 1,
			//},
		},
	})
}

func (t *HTTP2ClientMux) handleData(f *frame.DataFrame) {
	size := f.Header().Length
	var sendBDPPing bool
	if t.bdpEst != nil {
		sendBDPPing = t.bdpEst.add(size)
	}
	// Decouple connection's flow control from application's read.
	// An update on connection's flow control should not depend on
	// whether user application has read the data or not. Such a
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
	if size > 0 {
		if err := s.fc.onData(size); err != nil {
			t.closeStream(s, io.EOF, true, frame.ErrCodeFlowControl, &Status{Err: err}, nil, false)
			return
		}
		if f.Header().Flags.Has(frame.FlagDataPadded) {
			if w := s.fc.onRead(size - uint32(len(f.Data()))); w > 0 {
				t.controlBuf.put(&outgoingWindowUpdate{s.Id, w})
			}
		}
		// TODO(bradfitz, zhaoq): A copy is required here because there is no
		// guarantee f.Data() is consumed before the arrival of next frame.
		// Can this copy be eliminated?
		if len(f.Data()) > 0 {
			buffer := t.bufferPool.get()
			buffer.Reset()
			buffer.Write(f.Data())
			s.RcvdBytes += len(f.Data())
			s.RcvdPackets++
			s.LastRead = time.Now()
			s.queueReadBuf(recvMsg{buffer: buffer})
		}
	}
	// The server has closed the stream without sending trailers.  Record that
	// the read direction is closed, and set the status appropriately.
	if f.StreamEnded() {
		s.setReadClosed(1)
		//log.Println("Client CLOSE received", f)
		s.queueReadBuf(recvMsg{err: io.EOF})
	}
}

func (t *HTTP2ClientMux) handleRSTStream(f *frame.RSTStreamFrame) {
	s := t.getStream(f)
	if s == nil {
		return
	}

	if s.getReadClosed() == 2 {
		return
	}
	log.Println("Client RST received", f, f.ErrCode, s.getReadClosed())

	if f.ErrCode == frame.ErrCodeRefusedStream {
		// The stream was unprocessed by the server.
		atomic.StoreUint32(&s.unprocessed, 1)
	}
	statusCode, ok := http2ErrConvTab[f.ErrCode]
	if !ok {
		log.Printf("transport: HTTP2ClientMux.handleRSTStream found no mapped gRPC status for the received http2 error %v", f.ErrCode)

		statusCode = Unknown
	}
	if statusCode == Canceled {
		if d, ok := s.ctx.Deadline(); ok && !d.After(time.Now()) {
			// Our deadline was already exceeded, and that was likely the cause
			// of this cancelation.  Alter the status code accordingly.
			statusCode = DeadlineExceeded
		}
	}

	s.setReadClosed(2)
	if atomic.CompareAndSwapUint32(&s.headerChanClosed, 0, 1) {
		close(s.headerChan)
	}

	//if s.headerChanClosed == 0 {
	s.queueReadBuf(recvMsg{err: errStreamRST})
	//} else {
	//	s.write(recvMsg{err: io.EOF})
	//}
	//t.closeStream(s, io.EOF, false, http2.ErrCodeNo, &Status{Code: int(statusCode), Message: fmt.Sprintf("stream terminated by RST_STREAM with error code: %v", f.ErrCode)}, nil, false)
}

func (t *HTTP2ClientMux) handleSettings(f *frame.SettingsFrame, isFirst bool) {
	if f.IsAck() {
		return
	}
	var maxStreams *uint32
	var fs *uint32
	var ss []frame.Setting
	var updateFuncs []func()
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
}

func (t *HTTP2ClientMux) handlePing(f *frame.PingFrame) {
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

func (t *HTTP2ClientMux) handleGoAway(f *frame.GoAwayFrame) {
	t.mu.Lock()
	if t.state == closing {
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
	// GoAway will be sent with an ID of MaxInt32 and the second GoAway will be
	// sent after an RTT delay with the ID of the last stream the server will
	// process.
	//
	// Therefore, when we get the first GoAway we don't necessarily close any
	// streams. While in case of second GoAway we close all streams created after
	// the GoAwayId. This way streams that were in-flight while the GoAway from
	// server was being sent don't get killed.
	select {
	case <-t.goAway: // t.goAway has been closed (i.e.,multiple GoAways).
		// If there are multiple GoAways the first one should always have an ID greater than the following ones.
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
		t.onGoAway(t.goAwayReason)
		t.state = draining
	}
	// All streams with IDs greater than the GoAwayId
	// and smaller than the previous GoAway ID should be killed.
	upperLimit := t.prevGoAwayID
	if upperLimit == 0 { // This is the first GoAway Frame.
		upperLimit = math.MaxUint32 // Kill all streams after the GoAway ID.
	}
	for streamID, stream := range t.activeStreams {
		if streamID > id && streamID <= upperLimit {
			// The stream was unprocessed by the server.
			atomic.StoreUint32(&stream.unprocessed, 1)
			t.closeStream(stream, errStreamDrain, false, frame.ErrCodeNo, statusGoAway, nil, false)
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
func (t *HTTP2ClientMux) setGoAwayReason(f *frame.GoAwayFrame) {
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

func (t *HTTP2ClientMux) GetGoAwayReason() (GoAwayReason, string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.goAwayReason, t.goAwayDebugMessage
}

func (t *HTTP2ClientMux) handleWindowUpdate(f *frame.WindowUpdateFrame) {
	t.controlBuf.put(&incomingWindowUpdate{
		streamID:  f.Header().StreamID,
		increment: f.Increment,
	})
}

// operateHeaders takes action on the decoded headers.
func (t *HTTP2ClientMux) operateHeaders(f *frame.MetaHeadersFrame) {
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
		t.closeStream(s, st, true, frame.ErrCodeProtocol, st, nil, false)
		return
	}

	// frame.Truncated is set to true when framer detects that the current header
	// list size hits MaxHeaderListSize limit.
	if f.Truncated {
		se := &Status{Message: "peer header list size exceeded limit"}
		t.closeStream(s, se, true, frame.ErrCodeFrameSize, se, nil, endStream)
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
		s.headerValid = true
		close(s.headerChan)
	}

	if !endStream {
		return
	}

	s.setReadClosed(1)
	s.queueReadBuf(recvMsg{err: io.EOF})

	// Old: if client received END_STREAM from server while stream was still active, send RST_STREAM
}

// reader runs as a separate goroutine in charge of reading data from network
// connection.
//
// TODO(zhaoq): currently one reader per transport. Investigate whether this is
// optimal.
// TODO(zhaoq): Check the validity of the incoming frame sequence.
func (t *HTTP2ClientMux) reader() {
	defer close(t.readerDone)
	// Check the validity of server preface.
	f, err := t.framer.fr.ReadFrame()
	if err != nil {
		err = connectionErrorf(true, err, "error reading server preface: %v", err)
		t.Close(err) // this kicks off resetTransport, so must be last before return
		return
	}
	t.conn.SetReadDeadline(time.Time{}) // reset deadline once we get the settings frame (we didn't time out, yay!)
	if t.keepaliveEnabled {
		atomic.StoreInt64(&t.lastRead, time.Now().UnixNano())
	}
	sf, ok := f.(*frame.SettingsFrame)
	if !ok {
		// this kicks off resetTransport, so must be last before return
		t.Close(connectionErrorf(true, nil, "initial http2 frame from server is not a settings frame: %T", f))
		return
	}
	t.handleSettings(sf, true)
	t.onPrefaceReceipt(sf)

	// loop to keep reading incoming messages on this transport.
	for {
		t.controlBuf.throttle()
		f, err := t.framer.fr.ReadFrame()
		if t.keepaliveEnabled {
			atomic.StoreInt64(&t.lastRead, time.Now().UnixNano())
		}
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
					t.closeStream(s, &Status{Code: int(code), Message: msg}, true, frame.ErrCodeProtocol, &Status{Code: int(code), Message: msg}, nil, false)
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
			t.operateHeaders(frame)
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
			log.Printf("transport: HTTP2ClientMux.reader got unhandled frame type %v.", frame)
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
func (t *HTTP2ClientMux) keepalive() {
	p := &ping{data: [8]byte{}}
	// True iff a ping has been sent, and no data has been received since then.
	outstandingPing := false
	// Amount of time remaining before which we should receive an ACK for the
	// last sent ping.
	timeoutLeft := time.Duration(0)
	// Records the last value of t.lastRead before we go block on the timer.
	// This is required to check for read activity since then.
	prevNano := time.Now().UnixNano()
	timer := time.NewTimer(t.kp.Time)
	for {
		select {
		case <-timer.C:
			lastRead := atomic.LoadInt64(&t.lastRead)
			if lastRead > prevNano {
				// There has been read activity since the last time we were here.
				outstandingPing = false
				// Next timer should fire at kp.Time seconds from lastRead time.
				timer.Reset(time.Duration(lastRead) + t.kp.Time - time.Duration(time.Now().UnixNano()))
				prevNano = lastRead
				continue
			}
			if outstandingPing && timeoutLeft <= 0 {
				t.Close(connectionErrorf(true, nil, "keepalive ping failed to receive ACK within timeout"))
				return
			}
			t.mu.Lock()
			if t.state == closing {
				// If the transport is closing, we should exit from the
				// keepalive goroutine here. If not, we could have a race
				// between the call to Signal() from Close() and the call to
				// Wait() here, whereby the keepalive goroutine ends up
				// blocking on the condition variable which will never be
				// signalled again.
				t.mu.Unlock()
				return
			}
			if len(t.activeStreams) < 1 && !t.kp.PermitWithoutStream {
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

			// We get here either because we were dormant and a new stream was
			// created which unblocked the Wait() call, or because the
			// keepalive timer expired. In both cases, we need to send a ping.
			if !outstandingPing {
				//if channelz.IsOn() {
				//	atomic.AddInt64(&t.czData.kpCount, 1)
				//}
				t.controlBuf.put(p)
				timeoutLeft = t.kp.Timeout
				outstandingPing = true
			}
			// The amount of time to sleep here is the minimum of kp.Time and
			// timeoutLeft. This will ensure that we wait only for kp.Time
			// before sending out the next ping (for cases where the ping is
			// acked).
			sleepDuration := minTime(t.kp.Time, timeoutLeft)
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

func (t *HTTP2ClientMux) Error() <-chan struct{} {
	return t.ctx.Done()
}

func (t *HTTP2ClientMux) GoAway() <-chan struct{} {
	return t.goAway
}

func (t *HTTP2ClientMux) IncrMsgSent() {
	atomic.AddInt64(&t.czData.msgSent, 1)
	atomic.StoreInt64(&t.czData.lastMsgSentTime, time.Now().UnixNano())
}

func (t *HTTP2ClientMux) IncrMsgRecv() {
	atomic.AddInt64(&t.czData.msgRecv, 1)
	atomic.StoreInt64(&t.czData.lastMsgRecvTime, time.Now().UnixNano())
}

func (t *HTTP2ClientMux) getOutFlowWindow() int64 {
	resp := make(chan uint32, 1)
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	t.controlBuf.put(&outFlowControlSizeRequest{resp})
	select {
	case sz := <-resp:
		return int64(sz)
	case <-t.ctxDone:
		return -1
	case <-timer.C:
		return -2
	}
}

func (t *HTTP2ClientMux) CanTakeNewRequest() bool {
	return t.state == reachable
}
