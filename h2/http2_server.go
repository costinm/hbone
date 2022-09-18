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
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	frame2 "github.com/costinm/hbone/h2/frame"
	"github.com/costinm/hbone/h2/hpack"
)

var (
	// ErrIllegalHeaderWrite indicates that setting header is illegal because of
	// the stream's state.
	ErrIllegalHeaderWrite = errors.New("transport: SendHeader called multiple times")
	// ErrHeaderListSizeLimitViolation indicates that the header list size is larger
	// than the limit set by peer.
	ErrHeaderListSizeLimitViolation = errors.New("transport: trying to send header list size larger than the limit set by peer")
)

// serverConnectionCounter counts the number of connections a server has seen
// (equal to the number of http2Servers created). Must be accessed atomically.
var serverConnectionCounter uint64

// HTTP2ServerMux implements the ServerTransport interface with HTTP2.
type HTTP2ServerMux struct {
	lastRead   int64 // Keep this field 64-bit aligned. Accessed atomically.
	ctx        context.Context
	done       chan struct{}
	conn       net.Conn
	loopy      *loopyWriter
	readerDone chan struct{} // sync point to enable testing.
	writerDone chan struct{} // sync point to enable testing.
	remoteAddr net.Addr
	localAddr  net.Addr
	framer     *framer
	// The max number of concurrent streams.
	maxStreams uint32
	// controlBuf delivers all the control related tasks (e.g., window
	// updates, reset streams, and various settings) to the controller.
	controlBuf *controlBuffer
	fc         *trInFlow
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
	resetPingStrikes      uint32 // Accessed atomically.
	initialWindowSize     int32
	bdpEst                *bdpEstimator
	maxSendHeaderListSize *uint32

	mu sync.Mutex // guard the following

	// drainChan is initialized when Drain() is called the first time.
	// After which the server writes out the first GoAway(with ID 2^31-1) frame.
	// Then an independent goroutine will be launched to later send the second GoAway.
	// During this time we don't want to write another first GoAway(with ID 2^31 -1) frame.
	// Thus call to Drain() will be a no-op if drainChan is already initialized since draining is
	// already underway.
	drainChan     chan struct{}
	state         transportState
	activeStreams map[uint32]*Stream
	// idle is the time instant when the connection went idle.
	// This is either the beginning of the connection or when the number of
	// RPCs go down to 0.
	// When the connection is busy, this value is set to 0.
	idle time.Time

	// Fields below are for channelz metric collection.
	czData     *channelzData
	bufferPool *bufferPool

	connectionID uint64

	// maxStreamMu guards the maximum stream ID
	// This lock may not be taken if mu is already held.
	maxStreamMu sync.Mutex
	maxStreamID uint32 // max stream ID ever seen
	FrameSize   uint32
}

// NewServerTransport creates a http2 transport with conn and configuration
// options from config.
//
// It returns a non-nil transport and a nil error on success. On failure, it
// returns a nil transport and a non-nil error. For a special case where the
// underlying conn gets closed before the client preface could be read, it
// returns a nil transport and a nil error.
func NewServerTransport(conn net.Conn, config *ServerConfig) (_ *HTTP2ServerMux, err error) {
	writeBufSize := config.WriteBufferSize
	readBufSize := config.ReadBufferSize
	maxHeaderListSize := defaultServerMaxHeaderListSize
	if config.MaxHeaderListSize != nil {
		maxHeaderListSize = *config.MaxHeaderListSize
	}
	fs := uint32(http2MaxFrameLen)
	if config.MaxFrameSize != 0 {
		fs = config.MaxFrameSize
	}
	framer := newFramer(conn, writeBufSize, readBufSize, maxHeaderListSize, fs)
	// Send initial settings as connection preface to client.
	isettings := []frame2.Setting{{
		ID:  frame2.SettingMaxFrameSize,
		Val: uint32(fs),
	}}
	// TODO(zhaoq): Have a better way to signal "no limit" because 0 is
	// permitted in the HTTP2 spec.
	maxStreams := config.MaxStreams
	if maxStreams == 0 {
		maxStreams = math.MaxUint32
	} else {
		isettings = append(isettings, frame2.Setting{
			ID:  frame2.SettingMaxConcurrentStreams,
			Val: maxStreams,
		})
	}
	dynamicWindow := true
	iwz := int32(initialWindowSize)
	if config.InitialWindowSize >= defaultWindowSize {
		iwz = config.InitialWindowSize
		dynamicWindow = false
	}
	icwz := int32(initialWindowSize)
	if config.InitialConnWindowSize >= defaultWindowSize {
		icwz = config.InitialConnWindowSize
		dynamicWindow = false
	}
	if iwz != defaultWindowSize {
		isettings = append(isettings, frame2.Setting{
			ID:  frame2.SettingInitialWindowSize,
			Val: uint32(iwz)})
	}
	if config.MaxHeaderListSize != nil {
		isettings = append(isettings, frame2.Setting{
			ID:  frame2.SettingMaxHeaderListSize,
			Val: *config.MaxHeaderListSize,
		})
	}
	if config.HeaderTableSize != nil {
		isettings = append(isettings, frame2.Setting{
			ID:  frame2.SettingHeaderTableSize,
			Val: *config.HeaderTableSize,
		})
	}
	if err := framer.fr.WriteSettings(isettings...); err != nil {
		return nil, connectionErrorf(false, err, "transport: %v", err)
	}
	// Adjust the connection flow control window if needed.
	if delta := uint32(icwz - defaultWindowSize); delta > 0 {
		if err := framer.fr.WriteWindowUpdate(0, delta); err != nil {
			return nil, connectionErrorf(false, err, "transport: %v", err)
		}
	}
	kp := config.KeepaliveParams
	if kp.MaxConnectionIdle == 0 {
		kp.MaxConnectionIdle = defaultMaxConnectionIdle
	}
	if kp.MaxConnectionAge == 0 {
		kp.MaxConnectionAge = defaultMaxConnectionAge
	}
	if kp.MaxConnectionAgeGrace == 0 {
		kp.MaxConnectionAgeGrace = defaultMaxConnectionAgeGrace
	}
	if kp.Time == 0 {
		kp.Time = defaultServerKeepaliveTime
	}
	if kp.Timeout == 0 {
		kp.Timeout = defaultServerKeepaliveTimeout
	}
	kep := config.KeepalivePolicy
	if kep.MinTime == 0 {
		kep.MinTime = defaultKeepalivePolicyMinTime
	}

	done := make(chan struct{})
	t := &HTTP2ServerMux{
		ctx:        context.Background(),
		done:       done,
		conn:       conn,
		remoteAddr: conn.RemoteAddr(),
		localAddr:  conn.LocalAddr(),
		framer:     framer,
		readerDone: make(chan struct{}),
		writerDone: make(chan struct{}),
		maxStreams: maxStreams,
		//inTapHandle:       config.InTapHandle,
		fc:            &trInFlow{limit: uint32(icwz)},
		state:         reachable,
		activeStreams: make(map[uint32]*Stream),
		//stats:             config.StatsHandler,
		kp:                kp,
		idle:              time.Now(),
		kep:               kep,
		initialWindowSize: iwz,
		czData:            new(channelzData),
		bufferPool:        newBufferPool(),
		FrameSize:         fs,
	}
	t.controlBuf = newControlBuffer(t.done)
	if dynamicWindow {
		t.bdpEst = &bdpEstimator{
			bdp:               initialWindowSize,
			updateFlowControl: t.updateFlowControl,
		}
	}
	// TODO(telemetry): register mux start
	if err != nil {
		return nil, err
	}

	t.connectionID = atomic.AddUint64(&serverConnectionCounter, 1)
	t.framer.writer.Flush()

	defer func() {
		if err != nil {
			t.Close()
		}
	}()

	// Check the validity of client preface.
	preface := make([]byte, len(clientPreface))
	if _, err := io.ReadFull(t.conn, preface); err != nil {
		// In deployments where a gRPC server runs behind a cloud load balancer
		// which performs regular TCP level health checks, the connection is
		// closed immediately by the latter.  Returning io.EOF here allows the
		// grpc server implementation to recognize this scenario and suppress
		// logging to reduce spam.
		if err == io.EOF {
			return nil, io.EOF
		}
		return nil, connectionErrorf(false, err, "transport: HTTP2ServerMux.HandleStreams failed to receive the preface from client: %v", err)
	}
	if !bytes.Equal(preface, clientPreface) {
		return nil, connectionErrorf(false, nil, "transport: HTTP2ServerMux.HandleStreams received bogus greeting from client: %q", preface)
	}

	frame, err := t.framer.fr.ReadFrame()
	if err == io.EOF || err == io.ErrUnexpectedEOF {
		return nil, err
	}
	if err != nil {
		return nil, connectionErrorf(false, err, "transport: HTTP2ServerMux.HandleStreams failed to read initial settings frame: %v", err)
	}
	atomic.StoreInt64(&t.lastRead, time.Now().UnixNano())
	sf, ok := frame.(*frame2.SettingsFrame)
	if !ok {
		return nil, connectionErrorf(false, nil, "transport: HTTP2ServerMux.HandleStreams saw invalid preface type %T from client", frame)
	}
	t.handleSettings(sf)

	go func() {
		t.loopy = newLoopyWriter(serverSide, t.framer, t.controlBuf, t.bdpEst, fs)
		t.loopy.ssGoAwayHandler = t.outgoingGoAwayHandler
		if err := t.loopy.run(); err != nil {
			log.Printf("transport: loopyWriter.run returning. Err: %v", err)
		}
		t.conn.Close()
		t.controlBuf.finish()
		close(t.writerDone)
	}()
	go t.keepalive()
	return t, nil
}

// operateHeader takes action on the decoded headers.
func (t *HTTP2ServerMux) operateHeaders(frame *frame2.MetaHeadersFrame, handle func(*Stream), traceCtx func(context.Context, string) context.Context) (fatal bool) {
	// Acquire max stream ID lock for entire duration
	t.maxStreamMu.Lock()
	defer t.maxStreamMu.Unlock()

	streamID := frame.Header().StreamID

	// frame.Truncated is set to true when framer detects that the current header
	// list size hits MaxHeaderListSize limit.
	if frame.Truncated {
		t.controlBuf.put(&cleanupStream{
			streamID: streamID,
			rst:      true,
			rstCode:  frame2.ErrCodeFrameSize,
			onWrite:  func() {},
		})
		return false
	}

	if streamID%2 != 1 || streamID <= t.maxStreamID {
		// illegal gRPC stream id.
		log.Printf("transport: HTTP2ServerMux.HandleStreams received an illegal stream id: %v", streamID)

		return true
	}
	t.maxStreamID = streamID

	hreq, _ := http.NewRequestWithContext(t.ctx, "POST", "https:///", nil)
	buf := newRecvBuffer()
	s := &Stream{
		Id:            streamID,
		st:            t,
		buf:           buf,
		writeDoneChan: make(chan *dataFrame),
		fc:            &inFlow{limit: uint32(t.initialWindowSize)},
		Request:       hreq,
		Response: &http.Response{
			Header:  http.Header{},
			Trailer: http.Header{},
			Request: hreq,
		},
	}
	var (
		headerError bool

		timeoutSet bool
		timeout    time.Duration
	)

	for _, hf := range frame.Fields {
		switch hf.Name {
		case "content-type":
			s.Request.Header.Add(hf.Name, hf.Value)
			if strings.HasPrefix(hf.Value, "application/grpc") {
				s.grpc = true
			}
		case "grpc-encoding":
			s.recvCompress = hf.Value
		case ":method":
			s.Request.Method = hf.Value
		case ":path":
			s.Request.URL.Path = hf.Value
		case ":authority":
			s.Request.Host = hf.Value
			s.Request.URL.Host = hf.Value
		case "host":
			s.Request.Host = hf.Value
			s.Request.URL.Host = hf.Value
		case "grpc-timeout":
			timeoutSet = true
			var err error
			if timeout, err = decodeTimeout(hf.Value); err != nil {
				headerError = true
			}
		// "Transports must consider requests containing the Connection header
		default:
			v, err := decodeMetadataHeader(hf.Name, hf.Value)
			if err != nil {
				headerError = true
				log.Printf("Failed to decode metadata header (%q, %q): %v", hf.Name, hf.Value, err)
				break
			}
			s.Request.Header.Add(hf.Name, v)
		}
	}

	if /*!isGRPC || */ headerError {
		t.controlBuf.put(&cleanupStream{
			streamID: streamID,
			rst:      true,
			rstCode:  frame2.ErrCodeProtocol,
			onWrite:  func() {},
		})
		return false
	}

	if frame.StreamEnded() {
		s.setReadClosed(1)
	}
	if timeoutSet {
		s.ctx, s.cancel = context.WithTimeout(t.ctx, timeout)
	} else {
		s.ctx, s.cancel = context.WithCancel(t.ctx)
	}
	t.mu.Lock()
	if t.state != reachable {
		t.mu.Unlock()
		s.cancel()
		return false
	}
	if uint32(len(t.activeStreams)) >= t.maxStreams {
		t.mu.Unlock()
		t.controlBuf.put(&cleanupStream{
			streamID: streamID,
			rst:      true,
			rstCode:  frame2.ErrCodeRefusedStream,
			onWrite:  func() {},
		})
		s.cancel()
		return false
	}
	t.activeStreams[streamID] = s
	if len(t.activeStreams) == 1 {
		t.idle = time.Time{}
	}
	t.mu.Unlock()
	// TODO(telemetry): server stream start
	s.requestRead = func(n int) {
		t.adjustWindow(s, uint32(n))
	}
	s.ctx = traceCtx(s.ctx, s.Path)
	s.ctxDone = s.ctx.Done()
	s.wq = newWriteQuota(defaultWriteQuota, s.ctxDone)
	s.trReader = &transportReader{
		reader: &recvBufferReader{
			ctx:        s.ctx,
			ctxDone:    s.ctxDone,
			recv:       s.buf,
			freeBuffer: t.bufferPool.put,
		},
		windowHandler: t,
		s:             s,
	}
	// Register the stream with loopy.
	t.controlBuf.put(&registerStream{
		streamID: s.Id,
		wq:       s.wq,
	})
	handle(s)
	return false
}

// HandleStreams receives incoming streams using the given handler. This is
// typically run in a separate goroutine.
// traceCtx attaches trace to ctx and returns the new context.
func (t *HTTP2ServerMux) HandleStreams(handle func(*Stream), traceCtx func(context.Context, string) context.Context) {
	defer close(t.readerDone)
	for {
		t.controlBuf.throttle()
		frame, err := t.framer.fr.ReadFrame()
		atomic.StoreInt64(&t.lastRead, time.Now().UnixNano())
		if err != nil {
			if se, ok := err.(frame2.StreamError); ok {
				log.Printf("transport: HTTP2ServerMux.HandleStreams encountered http2.StreamError: %v", se)

				t.mu.Lock()
				s := t.activeStreams[se.StreamID]
				t.mu.Unlock()
				if s != nil {
					t.closeStream(s, true, se.Code, false)
				} else {
					t.controlBuf.put(&cleanupStream{
						streamID: se.StreamID,
						rst:      true,
						rstCode:  se.Code,
						onWrite:  func() {},
					})
				}
				continue
			}
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				t.Close()
				return
			}
			log.Printf("transport: HTTP2ServerMux.HandleStreams failed to read frame: %v", err)

			t.Close()
			return
		}
		switch frame := frame.(type) {
		case *frame2.MetaHeadersFrame:
			if t.operateHeaders(frame, handle, traceCtx) {
				t.Close()
				break
			}
		case *frame2.DataFrame:
			t.handleData(frame)
		case *frame2.RSTStreamFrame:
			t.handleRSTStream(frame)
		case *frame2.SettingsFrame:
			t.handleSettings(frame)
		case *frame2.PingFrame:
			t.handlePing(frame)
		case *frame2.WindowUpdateFrame:
			t.handleWindowUpdate(frame)
		case *frame2.GoAwayFrame:
			// TODO: Handle GoAway from the client appropriately.
		default:
			log.Printf("transport: HTTP2ServerMux.HandleStreams found unhandled frame type %v.", frame)

		}
	}
}

func (t *HTTP2ServerMux) getStream(f frame2.Frame) (*Stream, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.activeStreams == nil {
		// The transport is closing.
		return nil, false
	}
	s, ok := t.activeStreams[f.Header().StreamID]
	if !ok {
		// The stream is already done.
		return nil, false
	}
	return s, true
}

// adjustWindow sends out extra window update over the initial window size
// of stream if the application is requesting data larger in size than
// the window.
func (t *HTTP2ServerMux) adjustWindow(s *Stream, n uint32) {
	if w := s.fc.maybeAdjust(n); w > 0 {
		t.controlBuf.put(&outgoingWindowUpdate{streamID: s.Id, increment: w})
	}

}

// updateWindow adjusts the inbound quota for the stream and the transport.
// Window updates will deliver to the controller for sending when
// the cumulative quota exceeds the corresponding threshold.
func (t *HTTP2ServerMux) updateWindow(s *Stream, n uint32) {
	if w := s.fc.onRead(n); w > 0 {
		t.controlBuf.put(&outgoingWindowUpdate{streamID: s.Id,
			increment: w,
		})
	}
}

// updateFlowControl updates the incoming flow control windows
// for the transport and the stream based on the current bdp
// estimation.
func (t *HTTP2ServerMux) updateFlowControl(n uint32) {
	t.mu.Lock()
	for _, s := range t.activeStreams {
		s.fc.newLimit(n)
	}
	t.initialWindowSize = int32(n)
	t.mu.Unlock()
	t.controlBuf.put(&outgoingWindowUpdate{
		streamID:  0,
		increment: t.fc.newLimit(n),
	})
	t.controlBuf.put(&outgoingSettings{
		ss: []frame2.Setting{
			{
				ID:  frame2.SettingInitialWindowSize,
				Val: n,
			},
		},
	})

}

func (t *HTTP2ServerMux) handleData(f *frame2.DataFrame) {
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
	s, ok := t.getStream(f)
	if !ok {
		return
	}
	if s.getReadClosed() != 0 {
		t.closeStream(s, true, frame2.ErrCodeStreamClosed, false)
		return
	}
	if size > 0 {
		if err := s.fc.onData(size); err != nil {
			t.closeStream(s, true, frame2.ErrCodeFlowControl, false)
			return
		}
		if f.Header().Flags.Has(frame2.FlagDataPadded) {
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
			s.write(recvMsg{buffer: buffer})
		}
	}
	if f.StreamEnded() {
		// Received the end of stream from the client.
		s.setReadClosed(1)
		s.write(recvMsg{err: io.EOF})
	}
}

func (t *HTTP2ServerMux) handleRSTStream(f *frame2.RSTStreamFrame) {
	// If the stream is not deleted from the transport's active streams map, then do a regular close stream.
	log.Println("Server RST received", f)
	if s, ok := t.getStream(f); ok {
		s.setReadClosed(2)
		s.write(recvMsg{err: errStreamRST})

		return
	}
	// If the stream is already deleted from the active streams map, then put a cleanupStream item into controlbuf to delete the stream from loopy writer's established streams map.
	t.controlBuf.put(&cleanupStream{
		streamID: f.Header().StreamID,
		rst:      false,
		rstCode:  0,
		onWrite:  func() {},
	})
}

func (t *HTTP2ServerMux) handleSettings(f *frame2.SettingsFrame) {
	if f.IsAck() {
		return
	}
	var ss []frame2.Setting
	var updateFuncs []func()
	var fs *uint32
	f.ForeachSetting(func(s frame2.Setting) error {
		switch s.ID {
		case frame2.SettingMaxFrameSize:
			fs = new(uint32)
			*fs = s.Val
			rfs := *fs
			if t.FrameSize > 0 && t.FrameSize > rfs {
				t.FrameSize = rfs
			}
		case frame2.SettingMaxHeaderListSize:
			updateFuncs = append(updateFuncs, func() {
				t.maxSendHeaderListSize = new(uint32)
				*t.maxSendHeaderListSize = s.Val
			})
		default:
			ss = append(ss, s)
		}
		return nil
	})
	t.controlBuf.executeAndPut(func(interface{}) bool {
		for _, f := range updateFuncs {
			f()
		}
		return true
	}, &incomingSettings{
		ss: ss,
	})
}

const (
	maxPingStrikes     = 2
	defaultPingTimeout = 2 * time.Hour
)

func (t *HTTP2ServerMux) handlePing(f *frame2.PingFrame) {
	if f.IsAck() {
		if f.Data == goAwayPing.data && t.drainChan != nil {
			close(t.drainChan)
			return
		}
		// Maybe it's a BDP ping.
		if t.bdpEst != nil {
			t.bdpEst.calculate(f.Data)
		}
		return
	}
	pingAck := &ping{ack: true}
	copy(pingAck.data[:], f.Data[:])
	t.controlBuf.put(pingAck)

	now := time.Now()
	defer func() {
		t.lastPingAt = now
	}()
	// A reset ping strikes means that we don't need to check for policy
	// violation for this ping and the pingStrikes counter should be set
	// to 0.
	if atomic.CompareAndSwapUint32(&t.resetPingStrikes, 1, 0) {
		t.pingStrikes = 0
		return
	}
	t.mu.Lock()
	ns := len(t.activeStreams)
	t.mu.Unlock()
	if ns < 1 && !t.kep.PermitWithoutStream {
		// Keepalive shouldn't be active thus, this new ping should
		// have come after at least defaultPingTimeout.
		if t.lastPingAt.Add(defaultPingTimeout).After(now) {
			t.pingStrikes++
		}
	} else {
		// Check if keepalive policy is respected.
		if t.lastPingAt.Add(t.kep.MinTime).After(now) {
			t.pingStrikes++
		}
	}

	if t.pingStrikes > maxPingStrikes {
		// Send goaway and close the connection.
		log.Printf("transport: Got too many pings from the client, closing the connection.")

		t.controlBuf.put(&goAway{code: frame2.ErrCodeEnhanceYourCalm, debugData: []byte("too_many_pings"), closeConn: true})
	}
}

func (t *HTTP2ServerMux) handleWindowUpdate(f *frame2.WindowUpdateFrame) {
	t.controlBuf.put(&incomingWindowUpdate{
		streamID:  f.Header().StreamID,
		increment: f.Increment,
	})
}

func appendHeaderFieldsFromMD(headerFields []hpack.HeaderField, md map[string][]string) []hpack.HeaderField {
	for k, vv := range md {
		if isReservedHeader(k) {
			// Clients don't tolerate reading restricted headers after some non restricted ones were sent.
			continue
		}
		for _, v := range vv {
			headerFields = append(headerFields, hpack.HeaderField{Name: strings.ToLower(k), Value: encodeMetadataHeader(k, v)})
		}
	}
	return headerFields
}

func (t *HTTP2ServerMux) checkForHeaderListSize(it interface{}) bool {
	if t.maxSendHeaderListSize == nil {
		return true
	}
	hdrFrame := it.(*headerFrame)
	var sz int64
	for _, f := range hdrFrame.hf {
		if sz += int64(f.Size()); sz > int64(*t.maxSendHeaderListSize) {
			log.Printf("header list size to send violates the maximum size (%d bytes) set by client", *t.maxSendHeaderListSize)

			return false
		}
	}
	return true
}

func (t *HTTP2ServerMux) streamContextErr(s *Stream) error {
	select {
	case <-t.done:
		return ErrConnClosing
	default:
	}
	return ContextErr(s.ctx.Err())
}

// WriteHeader sends the header metadata md back to the client.
func (t *HTTP2ServerMux) WriteHeader(s *Stream) error {
	if s.updateHeaderSent() {
		return ErrIllegalHeaderWrite
	}

	if s.getState() == streamDone {
		return t.streamContextErr(s)
	}

	s.hdrMu.Lock()
	if err := t.writeHeaderLocked(s); err != nil {
		s.hdrMu.Unlock()
		return err
	}
	s.hdrMu.Unlock()
	return nil
}

func (t *HTTP2ServerMux) setResetPingStrikes() {
	atomic.StoreUint32(&t.resetPingStrikes, 1)
}

func (t *HTTP2ServerMux) writeHeaderLocked(s *Stream) error {
	// TODO(mmukhi): Benchmark if the performance gets better if count the metadata and other header fields
	// first and create a slice of that exact size.
	headerFields := make([]hpack.HeaderField, 0, 2) // at least :status, content-type will be there if none else.
	headerFields = append(headerFields, hpack.HeaderField{Name: ":status", Value: s.Response.Status})
	headerFields = appendHeaderFieldsFromMD(headerFields, s.Response.Header)
	success, err := t.controlBuf.executeAndPut(t.checkForHeaderListSize, &headerFrame{
		streamID:  s.Id,
		hf:        headerFields,
		endStream: false,
		onWrite:   t.setResetPingStrikes,
	})
	if !success {
		if err != nil {
			return err
		}
		t.closeStream(s, true, frame2.ErrCodeInternal, false)
		return ErrHeaderListSizeLimitViolation
	}
	return nil
}

func (t *HTTP2ServerMux) writeStatus(s *Stream) error {
	if s.getWriteClosed() != 0 {
		return errStreamDone
	}

	s.setWriteClosed(1)

	var trailingHeader *headerFrame

	s.hdrMu.Lock()
	headerFields := make([]hpack.HeaderField, 0, 2) // grpc-status and grpc-message will be there if none else.

	// Attach the trailer metadata.
	headerFields = appendHeaderFieldsFromMD(headerFields, s.Response.Trailer)

	trailingHeader = &headerFrame{
		streamID:  s.Id,
		hf:        headerFields,
		endStream: true,
		onWrite:   t.setResetPingStrikes,
	}
	s.hdrMu.Unlock()

	success, err := t.controlBuf.execute(t.checkForHeaderListSize, trailingHeader)
	if !success {
		if err != nil {
			return err
		}
		t.closeStream(s, true, frame2.ErrCodeInternal, false)
		return ErrHeaderListSizeLimitViolation
	}

	// Old: Send a RST_STREAM after the trailers if the client has not already half-closed.
	// rst := s.getState() == streamActive

	trailingHeader.cleanup = &cleanupStream{
		streamID: s.Id,
		rst:      false,
		onWrite: func() {
			t.deleteStream(s)
		},
	}
	t.controlBuf.put(trailingHeader)

	return nil
}

// Write converts the data into HTTP2 data frame and sends it out. Non-nil error
// is returns if it fails (e.g., framing error, transport error).
func (t *HTTP2ServerMux) Write(s *Stream, hdr []byte, data []byte, last bool) error {
	if !s.isHeaderSent() { // Headers haven't been written yet.
		if err := t.WriteHeader(s); err != nil {
			return err
		}
	} else {
		// Writing headers checks for this condition.
		if s.getState() == streamDone {
			return t.streamContextErr(s)
		}
	}
	if s.getWriteClosed() != 0 {
		return errStreamDone
	}
	if last {
		// If it's the last message, update stream state.
		s.setWriteClosed(1)
	} else if s.getState() != streamActive {
		return errStreamDone
	}
	df := &dataFrame{
		streamID:    s.Id,
		endStream:   last,
		d:           data,
		onEachWrite: t.setResetPingStrikes,
		onDone:      s.writeDoneChan,
	}
	if err := s.wq.get(int32(len(hdr) + len(data))); err != nil {
		return t.streamContextErr(s)
	}
	err := t.controlBuf.put(df)
	if err != nil {
		return err
	}
	<-s.writeDoneChan
	return nil
}

// keepalive running in a separate goroutine does the following:
// 1. Gracefully closes an idle connection after a duration of keepalive.MaxConnectionIdle.
// 2. Gracefully closes any connection after a duration of keepalive.MaxConnectionAge.
// 3. Forcibly closes a connection after an additive period of keepalive.MaxConnectionAgeGrace over keepalive.MaxConnectionAge.
// 4. Makes sure a connection is alive by sending pings with a frequency of keepalive.Time and closes a non-responsive connection
// after an additional duration of keepalive.Timeout.
func (t *HTTP2ServerMux) keepalive() {
	p := &ping{}
	// True iff a ping has been sent, and no data has been received since then.
	outstandingPing := false
	// Amount of time remaining before which we should receive an ACK for the
	// last sent ping.
	kpTimeoutLeft := time.Duration(0)
	// Records the last value of t.lastRead before we go block on the timer.
	// This is required to check for read activity since then.
	prevNano := time.Now().UnixNano()
	// Initialize the different timers to their default values.
	idleTimer := time.NewTimer(t.kp.MaxConnectionIdle)
	ageTimer := time.NewTimer(t.kp.MaxConnectionAge)
	kpTimer := time.NewTimer(t.kp.Time)
	defer func() {
		// We need to drain the underlying channel in these timers after a call
		// to Stop(), only if we are interested in resetting them. Clearly we
		// are not interested in resetting them here.
		idleTimer.Stop()
		ageTimer.Stop()
		kpTimer.Stop()
	}()

	for {
		select {
		case <-idleTimer.C:
			t.mu.Lock()
			idle := t.idle
			if idle.IsZero() { // The connection is non-idle.
				t.mu.Unlock()
				idleTimer.Reset(t.kp.MaxConnectionIdle)
				continue
			}
			val := t.kp.MaxConnectionIdle - time.Since(idle)
			t.mu.Unlock()
			if val <= 0 {
				// The connection has been idle for a duration of keepalive.MaxConnectionIdle or more.
				// Gracefully close the connection.
				t.Drain()
				return
			}
			idleTimer.Reset(val)
		case <-ageTimer.C:
			t.Drain()
			ageTimer.Reset(t.kp.MaxConnectionAgeGrace)
			select {
			case <-ageTimer.C:
				// Close the connection after grace period.
				t.Close()
			case <-t.done:
			}
			return
		case <-kpTimer.C:
			lastRead := atomic.LoadInt64(&t.lastRead)
			if lastRead > prevNano {
				// There has been read activity since the last time we were
				// here. Setup the timer to fire at kp.Time seconds from
				// lastRead time and continue.
				outstandingPing = false
				kpTimer.Reset(time.Duration(lastRead) + t.kp.Time - time.Duration(time.Now().UnixNano()))
				prevNano = lastRead
				continue
			}
			if outstandingPing && kpTimeoutLeft <= 0 {
				t.Close()
				return
			}
			if !outstandingPing {
				//if channelz.IsOn() {
				//	atomic.AddInt64(&t.czData.kpCount, 1)
				//}
				t.controlBuf.put(p)
				kpTimeoutLeft = t.kp.Timeout
				outstandingPing = true
			}
			// The amount of time to sleep here is the minimum of kp.Time and
			// timeoutLeft. This will ensure that we wait only for kp.Time
			// before sending out the next ping (for cases where the ping is
			// acked).
			sleepDuration := minTime(t.kp.Time, kpTimeoutLeft)
			kpTimeoutLeft -= sleepDuration
			kpTimer.Reset(sleepDuration)
		case <-t.done:
			return
		}
	}
}

// Close starts shutting down the HTTP2ServerMux transport.
// TODO(zhaoq): Now the destruction is not blocked on any pending streams. This
// could cause some resource issue. Revisit this later.
func (t *HTTP2ServerMux) Close() {
	t.mu.Lock()
	if t.state == closing {
		t.mu.Unlock()
		return
	}
	t.state = closing
	streams := t.activeStreams
	t.activeStreams = nil
	t.mu.Unlock()
	t.controlBuf.finish()
	close(t.done)
	t.conn.Close()
	// Cancel all active streams.
	for _, s := range streams {
		s.cancel()
	}
}

// deleteStream deletes the stream s from transport's active streams.
func (t *HTTP2ServerMux) deleteStream(s *Stream) {
	if s.getWriteClosed() != 0 && s.getReadClosed() != 0 {
		t.mu.Lock()
		if _, ok := t.activeStreams[s.Id]; ok {
			delete(t.activeStreams, s.Id)
			if len(t.activeStreams) == 0 {
				t.idle = time.Now()
			}
		}
		t.mu.Unlock()
	}
}

// closeStream clears the footprint of a stream when the stream is not needed any more.
// eosReceived is required before deleting the stream from 'active'
func (t *HTTP2ServerMux) closeStream(s *Stream, rst bool, rstCode frame2.ErrCode, eosReceived bool) {
	// In case stream sending and receiving are invoked in separate
	// goroutines (e.g., bi-directional streaming), cancel needs to be
	// called to interrupt the potential blocking on other goroutines.
	s.cancel()

	s.swapState(streamDone)

	t.deleteStream(s)

	t.controlBuf.put(&cleanupStream{
		streamID: s.Id,
		rst:      rst,
		rstCode:  rstCode,
		onWrite:  func() {},
	})
}

func (t *HTTP2ServerMux) RemoteAddr() net.Addr {
	return t.remoteAddr
}

func (t *HTTP2ServerMux) Drain() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.drainChan != nil {
		return
	}
	t.drainChan = make(chan struct{})
	t.controlBuf.put(&goAway{code: frame2.ErrCodeNo, debugData: []byte{}, headsUp: true})
}

var goAwayPing = &ping{data: [8]byte{1, 6, 1, 8, 0, 3, 3, 9}}

// Handles outgoing GoAway and returns true if loopy needs to put itself
// in draining mode.
func (t *HTTP2ServerMux) outgoingGoAwayHandler(g *goAway) (bool, error) {
	t.maxStreamMu.Lock()
	t.mu.Lock()
	if t.state == closing { // TODO(mmukhi): This seems unnecessary.
		t.mu.Unlock()
		t.maxStreamMu.Unlock()
		// The transport is closing.
		return false, ErrConnClosing
	}
	if !g.headsUp {
		// Stop accepting more streams now.
		t.state = draining
		sid := t.maxStreamID
		if len(t.activeStreams) == 0 {
			g.closeConn = true
		}
		t.mu.Unlock()
		t.maxStreamMu.Unlock()
		if err := t.framer.fr.WriteGoAway(sid, g.code, g.debugData); err != nil {
			return false, err
		}
		if g.closeConn {
			// Abruptly close the connection following the GoAway (via
			// loopywriter).  But flush out what's inside the buffer first.
			t.framer.writer.Flush()
			return false, fmt.Errorf("transport: Connection closing")
		}
		return true, nil
	}
	t.mu.Unlock()
	t.maxStreamMu.Unlock()
	// For a graceful close, send out a GoAway with stream ID of MaxUInt32,
	// Follow that with a ping and wait for the ack to come back or a timer
	// to expire. During this time accept new streams since they might have
	// originated before the GoAway reaches the client.
	// After getting the ack or timer expiration send out another GoAway this
	// time with an ID of the max stream server intends to process.
	if err := t.framer.fr.WriteGoAway(math.MaxUint32, frame2.ErrCodeNo, []byte{}); err != nil {
		return false, err
	}
	if err := t.framer.fr.WritePing(false, goAwayPing.data); err != nil {
		return false, err
	}
	go func() {
		timer := time.NewTimer(time.Minute)
		defer timer.Stop()
		select {
		case <-t.drainChan:
		case <-timer.C:
		case <-t.done:
			return
		}
		t.controlBuf.put(&goAway{code: g.code, debugData: g.debugData})
	}()
	return false, nil
}

func (t *HTTP2ServerMux) IncrMsgSent() {
	atomic.AddInt64(&t.czData.msgSent, 1)
	atomic.StoreInt64(&t.czData.lastMsgSentTime, time.Now().UnixNano())
}

func (t *HTTP2ServerMux) IncrMsgRecv() {
	atomic.AddInt64(&t.czData.msgRecv, 1)
	atomic.StoreInt64(&t.czData.lastMsgRecvTime, time.Now().UnixNano())
}

func (t *HTTP2ServerMux) getOutFlowWindow() int64 {
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

func (t *HTTP2ServerMux) Conn() net.Conn {
	return t.conn
}
