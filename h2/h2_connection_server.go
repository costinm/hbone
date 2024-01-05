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
	"sync/atomic"
	"time"

	"github.com/costinm/hbone/h2/frame"
	"github.com/costinm/hbone/h2/hpack"
	"github.com/costinm/ugate/nio"
)

var (
	// ErrIllegalHeaderWrite indicates that setting header is illegal because of
	// the stream's streamDone.
	ErrIllegalHeaderWrite = errors.New("transport: SendHeader called multiple times")
	// ErrHeaderListSizeLimitViolation indicates that the header list size is larger
	// than the limit set by peer.
	ErrHeaderListSizeLimitViolation = errors.New("transport: trying to send header list size larger than the limit set by peer")
)

// serverConnectionCounter counts the number of connections a server has seen
// (equal to the number of http2Servers created). Must be accessed atomically.
var serverConnectionCounter uint64

// NewServerConnection creates a http2 transport with conn and configuration
// options from config.
//
// It returns a non-nil transport and a nil error on success. On failure, it
// returns a nil transport and a non-nil error. For a special case where the
// underlying conn gets closed before the client preface could be readBlocking, it
// returns a nil transport and a nil error.
func NewServerConnection(conn net.Conn, config *ServerConfig, ev *Events) (_ *H2Transport, err error) {
	// Check the validity of client preface.
	preface := make([]byte, len(clientPreface))
	if _, err := io.ReadFull(conn, preface); err != nil {
		// In deployments where a gRPC server runs behind a cloud reloadChannel balancer
		// which performs regular TCP level health checks, the connection is
		// closed immediately by the latter.  Returning io.EOF here allows the
		// grpc server implementation to recognize this scenario and suppress
		// logging to reduce spam.
		if err == io.EOF {
			return nil, io.EOF
		}
		return nil, connectionErrorf(false, err, "transport: H2Transport.HandleStreams failed to receive the preface from client: %v", err)
	}
	if !bytes.Equal(preface, clientPreface) {
		return nil, connectionErrorf(false, nil, "transport: H2Transport.HandleStreams received bogus greeting from client: %q", preface)
	}

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
	isettings := []frame.Setting{{
		ID:  frame.SettingMaxFrameSize,
		Val: uint32(fs),
	}}
	// TODO(zhaoq): Have a better way to signal "no limit" because 0 is
	// permitted in the HTTP2 spec.
	maxStreams := config.MaxStreams
	if maxStreams == 0 {
		maxStreams = math.MaxUint32
	}
	isettings = append(isettings, frame.Setting{
		ID:  frame.SettingMaxConcurrentStreams,
		Val: maxStreams,
	})

	dynamicWindow := true
	iwz := int32(initialWindowSize)
	if config.InitialWindowSize >= defaultWindowSize {
		iwz = config.InitialWindowSize
		dynamicWindow = false
	}
	icwz := 2 * int32(initialWindowSize)
	if config.InitialConnWindowSize >= defaultWindowSize {
		icwz = config.InitialConnWindowSize
		dynamicWindow = false
	}
	if iwz != defaultWindowSize {
		isettings = append(isettings, frame.Setting{
			ID:  frame.SettingInitialWindowSize,
			Val: uint32(iwz)})
	}
	if config.MaxHeaderListSize != nil {
		isettings = append(isettings, frame.Setting{
			ID:  frame.SettingMaxHeaderListSize,
			Val: *config.MaxHeaderListSize,
		})
	}
	if config.HeaderTableSize != nil {
		isettings = append(isettings, frame.Setting{
			ID:  frame.SettingHeaderTableSize,
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
	ctx, cancel := context.WithCancel(context.Background())
	defer func() {
		if err != nil {
			cancel()
		}
	}()

	done := make(chan struct{})
	t := &H2Transport{
		ctx:               ctx,
		conn:              conn,
		framer:            framer,
		readerDone:        make(chan struct{}),
		writerDone:        make(chan struct{}),
		fc:                &trInFlow{limit: uint32(icwz)},
		done:              done,
		initialWindowSize: defaultWindowSize,
		activeStreams:     make(map[uint32]*H2Stream),
		bufferPool:        newBufferPool(),
		idle:              time.Now(),
		start:             time.Now(),
		closing:           false,
		FrameSize:         fs,
		maxStreams:        maxStreams,
		kp:                kp,
		kep:               kep,
		goAway:            make(chan struct{}),
		nextID:            2,
		StartTime:         time.Now(),
	}
	t.Events.Add(*ev)

	t.controlBuf = newControlBuffer(t, t.done)
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
			t.Close(err)
		}
	}()

	rfr, err := t.framer.fr.ReadFrame()
	if err == io.EOF || err == io.ErrUnexpectedEOF {
		return nil, err
	}
	if err != nil {
		return nil, connectionErrorf(false, err, "transport: H2Transport.HandleStreams failed to readBlocking initial settings frame: %v", err)
	}
	atomic.StoreInt64(&t.LastRead, time.Now().UnixNano())
	sf, ok := rfr.(*frame.SettingsFrame)
	if !ok {
		return nil, connectionErrorf(false, nil, "transport: H2Transport.HandleStreams saw invalid preface type %T from client", rfr)
	}
	t.handleSettings(sf)

	t.loopy = newLoopyWriter(t, serverSide, t.framer, t.controlBuf, t.bdpEst, fs)
	go func() {
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

// operateHeader takes action on the decoded headers of a received request.
func (t *H2Transport) operateHeaders(rfr *frame.MetaHeadersFrame) error {
	streamID := rfr.Header().StreamID
	if streamID%2 != 1 && t.loopy.side == serverSide {
		// odd stream headers on server side - reverse channel.
		t.operateResponseHeaders(rfr)
		return nil
	}
	return t.operateAcceptHeaders(rfr)
}

func (t *H2Transport) operateAcceptHeaders(rfr *frame.MetaHeadersFrame) error {
	streamID := rfr.Header().StreamID

	// Acquire max stream WorkloadID lock for entire duration
	t.maxStreamMu.Lock()
	defer t.maxStreamMu.Unlock()

	// frame.Truncated is set to true when framer detects that the current header
	// list size hits MaxHeaderListSize limit.
	if rfr.Truncated {
		t.controlBuf.put(&cleanupStream{
			streamID: streamID,
			rst:      true,
			rstCode:  frame.ErrCodeFrameSize,
			onWrite:  func() {},
		})
		return nil
	}

	if streamID <= t.maxStreamID {
		return fmt.Errorf("transport: H2Transport.HandleStreams received an illegal stream id: %v", streamID)
	}

	t.maxStreamID = streamID

	hreq, _ := http.NewRequestWithContext(t.ctx, "POST", "https:///", nil)
	s := &H2Stream{
		StreamState:   nio.StreamState{MuxID: streamID},
		transport:     t,
		writeDoneChan: make(chan *dataFrame),
		inFlow:        &inFlow{limit: uint32(t.initialWindowSize)},
		Request:       hreq,
		Response: &http.Response{
			Header:  http.Header{},
			Trailer: http.Header{},
			Request: hreq,
		},
	}
	hreq.Proto = "HTTP/2.0"
	hreq.ProtoMajor = 2
	hreq.ProtoMinor = 0
	var (
		headerError bool

		timeoutSet bool
		timeout    time.Duration
	)

	for _, hf := range rfr.Fields {
		switch hf.Name {
		case "content-type":
			s.Request.Header.Add(hf.Name, hf.Value)
			if strings.HasPrefix(hf.Value, "application/grpc") {
				s.grpc = true
			}
		case ":method":
			s.Request.Method = hf.Value
		case ":path":
			qp := strings.SplitN(hf.Value, "?", 2)
			s.Request.URL.Path = qp[0]
			if len(qp) > 1 {
				s.Request.URL.RawQuery = qp[1]
			}
		case ":authority":
			s.Request.Host = hf.Value
			s.Request.URL.Host = hf.Value
		case ":scheme":
			s.Request.URL.Scheme = hf.Value
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
			rstCode:  frame.ErrCodeProtocol,
			onWrite:  func() {},
		})
		return nil
	}
	if timeoutSet {
		s.ctx, s.cancel = context.WithTimeout(t.ctx, timeout)
	} else {
		s.ctx, s.cancel = context.WithCancel(t.ctx)
	}
	s.ctxDone = s.ctx.Done()
	s.outFlow = newWriteQuota(defaultWriteQuota, s.ctxDone)
	s.inFrameList = nio.NewRecvBuffer(s.ctxDone, t.bufferPool.put, nil)

	if rfr.StreamEnded() {
		s.setReadClosed(1, io.EOF)
	}
	t.mu.Lock()
	if t.closing {
		t.mu.Unlock()
		s.cancel()
		return nil
	}
	if uint32(len(t.activeStreams)) >= t.maxStreams && t.IsServer() {
		t.mu.Unlock()
		t.controlBuf.put(&cleanupStream{
			streamID: streamID,
			rst:      true,
			rstCode:  frame.ErrCodeRefusedStream,
			onWrite:  func() {},
		})
		s.cancel()
		return nil
	}
	t.activeStreams[streamID] = s
	if len(t.activeStreams) == 1 {
		t.idle = time.Time{}
	}
	t.mu.Unlock()

	t.streamEvent(EventStreamStart, s)

	if t.TraceCtx != nil {
		s.ctx = t.TraceCtx(s.ctx, s.Request.URL.Path)
	} else {
		s.ctx, s.cancel = context.WithCancel(context.Background())
	}
	// Register the stream with loopy.
	t.controlBuf.put(&registerStream{
		streamID: s.MuxID,
		wq:       s.outFlow,
	})
	if t.Handle != nil {
		t.Handle(s)
	} else {
		s.CloseError(1)
	}
	return nil
}

// HandleStreams receives incoming streams using the given handler. This is
// typically run in a separate goroutine.
// traceCtx attaches trace to ctx and returns the new context.
func (t *H2Transport) HandleStreams() {
	if t.TraceCtx == nil {
		t.TraceCtx = func(ctx context.Context, s string) context.Context {
			return ctx
		}
	}
	defer close(t.readerDone)
	for {
		t.controlBuf.throttle()
		rfr, err := t.framer.fr.ReadFrame()
		atomic.StoreInt64(&t.LastRead, time.Now().UnixNano())
		if err != nil {
			if se, ok := err.(frame.StreamError); ok {
				log.Printf("transport: H2Transport.HandleStreams encountered http2.StreamError: %v", se)

				t.mu.Lock()
				s := t.activeStreams[se.StreamID]
				t.mu.Unlock()
				if s != nil {
					t.closeStream(s, nil, true, se.Code)
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
			if err == io.EOF {
				t.Close(nil)
				return
			}
			if err == io.ErrUnexpectedEOF {
				t.Close(err)
				return
			}
			log.Printf("transport: H2Transport.HandleStreams failed to readBlocking frame: %v", err)

			t.Close(err)
			return
		}
		switch tfr := rfr.(type) {
		case *frame.MetaHeadersFrame:
			atomic.AddInt64(&t.streamsStarted, 1)
			atomic.StoreInt64(&t.LastStreamCreatedTime, time.Now().UnixNano())
			if err := t.operateHeaders(tfr); err != nil {
				t.Close(err)
				break
			}
		case *frame.DataFrame:
			atomic.AddInt64(&t.msgRecv, 1)
			atomic.StoreInt64(&t.lastMsgRecvTime, time.Now().UnixNano())
			t.handleData(tfr)
		case *frame.RSTStreamFrame:
			t.handleRSTStream(tfr)
		case *frame.SettingsFrame:
			t.handleSettings(tfr)
		case *frame.PingFrame:
			t.handlePing(tfr)
		case *frame.WindowUpdateFrame:
			t.handleWindowUpdate(tfr)
		case *frame.GoAwayFrame:
			// TODO: Handle GoAway from the client appropriately.
		default:
			log.Printf("transport: H2Transport.HandleStreams found unhandled frame type %v.", tfr)
		}
	}
}

func (t *H2Transport) handleSettings(f *frame.SettingsFrame) {
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
			if t.FrameSize > 0 && t.FrameSize > rfs {
				t.FrameSize = rfs
			}
		case frame.SettingMaxHeaderListSize:
			updateFuncs = append(updateFuncs, func() {
				t.maxSendHeaderListSize = new(uint32)
				*t.maxSendHeaderListSize = s.Val
			})
		default:
			// Handled in loopyWriter - SettingsInitialWindowSize, SettingsHeaderTableSize
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

	t.MuxEvent(Event_Settings)
}

const (
	maxPingStrikes     = 2
	defaultPingTimeout = 2 * time.Hour
)

func (t *H2Transport) handlePing(f *frame.PingFrame) {
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

		t.controlBuf.put(&goAway{code: frame.ErrCodeEnhanceYourCalm, debugData: []byte("too_many_pings"), closeConn: true})
	}
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

func (t *H2Transport) checkForHeaderListSize(it interface{}) bool {
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

func (t *H2Transport) streamContextErr(s *H2Stream) error {
	select {
	case <-t.done:
		return ErrConnClosing
	default:
	}
	return ContextErr(s.ctx.Err())
}

// WriteHeader sends the header metadata md back to the client.
func (t *H2Transport) WriteHeader(s *H2Stream) error {
	if s.updateHeaderSent() {
		return ErrIllegalHeaderWrite
	}

	if atomic.LoadUint32((*uint32)(&s.streamDone)) == 1 {
		return t.streamContextErr(s)
	}

	s.hdrMu.Lock()
	if err := t.writeResponseHeaders(s); err != nil {
		s.hdrMu.Unlock()
		return err
	}
	s.hdrMu.Unlock()
	return nil
}

func (t *H2Transport) setResetPingStrikes() {
	atomic.StoreUint32(&t.resetPingStrikes, 1)
}

func (t *H2Transport) writeResponseHeaders(s *H2Stream) error {
	// TODO(mmukhi): Benchmark if the performance gets better if count the metadata and other header fields
	// first and create a slice of that exact size.
	headerFields := make([]hpack.HeaderField, 0, 2) // at least :status, content-type will be there if none else.
	status := s.Response.Status
	if status == "" {
		status = "200"
	}
	headerFields = append(headerFields, hpack.HeaderField{Name: ":status", Value: status})
	headerFields = appendHeaderFieldsFromMD(headerFields, s.Response.Header)
	success, err := t.controlBuf.executeAndPut(t.checkForHeaderListSize, &headerFrame{
		streamID:  s.MuxID,
		hf:        headerFields,
		endStream: false,
		onWrite:   t.setResetPingStrikes,
	})
	if !success {
		if err != nil {
			return err
		}
		t.closeStream(s, nil, true, frame.ErrCodeInternal)
		return ErrHeaderListSizeLimitViolation
	}
	return nil
}

func (t *H2Transport) writeTrailer(s *H2Stream) error {
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
		streamID:  s.MuxID,
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
		t.closeStream(s, nil, true, frame.ErrCodeInternal)
		return ErrHeaderListSizeLimitViolation
	}

	// Old: Send a RST_STREAM after the trailers if the client has not already half-closed.
	// rst := s.getState() == streamActive

	trailingHeader.cleanup = &cleanupStream{
		streamID: s.MuxID,
		rst:      false,
	}
	t.controlBuf.put(trailingHeader)

	return nil
}

// Send will add the buffer to the send queue, waiting for quota if needed.
// The buffer ownership is passed to the transport - after write the buffer will be recycled
// and reused for input or output. Write may fail later - the last call to Send or CloseWrite
// will return an error if any write failed.
func (t *H2Transport) Send(s *H2Stream, b []byte, start, end int, last bool) error {
	if !s.isHeaderSent() {
		// Headers haven't been written yet. This should only be the case
		// for response headers - requests send header first.
		if err := t.WriteHeader(s); err != nil {
			return err
		}
	}
	if s.getWriteClosed() != 0 {
		return errStreamDone
	}
	if last {
		s.setWriteClosed(1)
	}
	s.SentPackets++

	data := b
	df := &dataFrame{
		streamID:    s.MuxID,
		endStream:   last,
		end:         end,
		start:       start,
		d:           b,
		onEachWrite: t.setResetPingStrikes,
	}
	df.onDoneFunc = func(i []byte) {
		nio.PutDataBufferChunk(b)
	}

	if data != nil { // If it's not an empty data frame, check quota.
		dataLen := len(data)
		if end != 0 {
			dataLen = end - start
		}
		if err := s.outFlow.waitOrConsumeQuota(int32(dataLen)); err != nil {
			return t.streamContextErr(s)
		}
	}

	err := t.controlBuf.put(df)
	if err != nil {
		return err
	}

	return s.Error
}

// Write converts the data into HTTP2 data frame and sends it out. Non-nil error
// is returns if it fails (e.g., framing error, transport error).
func (t *H2Transport) Write(s *H2Stream, hdr []byte, data []byte, last bool) error {
	if !s.isHeaderSent() {
		// Headers haven't been written yet. This should only be the case
		// for response headers - requests send header first.
		if err := t.WriteHeader(s); err != nil {
			return err
		}
	}
	if s.getWriteClosed() != 0 {
		return errStreamDone
	}

	if last {
		s.setWriteClosed(1)
	}

	df := &dataFrame{
		streamID:    s.MuxID,
		endStream:   last,
		d:           data,
		onEachWrite: t.setResetPingStrikes,
		onDone:      s.writeDoneChan,
	}

	if hdr != nil || data != nil { // If it's not an empty data frame, check quota.
		if err := s.outFlow.waitOrConsumeQuota(int32(len(hdr) + len(data))); err != nil {
			return t.streamContextErr(s)
		}
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
func (t *H2Transport) keepalive() {
	p := &ping{}
	// True iff a ping has been sent, and no data has been received since then.
	outstandingPing := false
	// Amount of time remaining before which we should receive an ACK for the
	// last sent ping.
	kpTimeoutLeft := time.Duration(0)
	// Records the last value of t.lastRead before we go block on the timer.
	// This is required to check for readBlocking activity since then.
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
				t.Close(nil)
			case <-t.done:
			}
			return
		case <-kpTimer.C:
			lastRead := atomic.LoadInt64(&t.LastRead)
			if lastRead > prevNano {
				// There has been readBlocking activity since the last time we were
				// here. Setup the timer to fire at kp.Time seconds from
				// lastRead time and continue.
				outstandingPing = false
				kpTimer.Reset(time.Duration(lastRead) + t.kp.Time - time.Duration(time.Now().UnixNano()))
				prevNano = lastRead
				continue
			}
			if outstandingPing && kpTimeoutLeft <= 0 {
				t.Close(errors.New("Keepalive timeout"))
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

// Close starts shutting down the H2Transport transport.
// After it is called, the transport should not be accessed any more, in future may be
// recycled.
func (t *H2Transport) Close(err error) {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return
	}
	t.closed = true

	if t.Error == nil {
		t.Error = err
	}

	// Call t.onClose before setting the streamDone to closing to prevent the client
	// from attempting to create new streams ASAP.
	t.MuxEvent(Event_ConnClose)
	streams := t.activeStreams
	t.activeStreams = nil

	if t.kpDormant {
		// If the keepalive goroutine is blocked on this condition variable, we
		// should unblock it so that the goroutine eventually exits.
		t.kpDormancyCond.Signal()
	}
	t.mu.Unlock()
	t.controlBuf.finish()

	if t.cancel != nil {
		t.cancel()
	}

	close(t.done)

	t.conn.Close()

	// Cancel all active streams.
	for _, s := range streams {
		t.closeStream(s, ErrConnClosing, false, 0)
	}
	t.MuxEvent(Event_ConnClose)
}

// deleteStream deletes the stream s from transport's active streams.
// Called once, as result of closeStream(), from the writer thread.
func (t *H2Transport) deleteStream(s *H2Stream) {
	closeMux := false
	if s.getWriteClosed() != 0 && s.getReadClosed() != 0 && t.activeStreams != nil {
		t.mu.Lock()
		if _, ok := t.activeStreams[s.MuxID]; ok {
			delete(t.activeStreams, s.MuxID)
			if len(t.activeStreams) == 0 {
				t.idle = time.Now()
				closeMux = true
			}
		}
		t.mu.Unlock()

		if closeMux && t.closing {
			t.Close(nil)
		}
	}
}

func (t *H2Transport) Drain() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.drainChan != nil {
		return
	}
	t.drainChan = make(chan struct{})
	t.controlBuf.put(&goAway{code: frame.ErrCodeNo, debugData: []byte{}, headsUp: true})
}

var goAwayPing = &ping{data: [8]byte{1, 6, 1, 8, 0, 3, 3, 9}}

// Handles outgoing GoAway and returns true if loopy needs to Put itself
// in draining mode.
func (t *H2Transport) outgoingGoAwayHandler(g *goAway) (bool, error) {
	t.maxStreamMu.Lock()
	t.mu.Lock()
	if t.closing { // TODO(mmukhi): This seems unnecessary.
		t.mu.Unlock()
		t.maxStreamMu.Unlock()
		// The transport is closing.
		return false, ErrConnClosing
	}
	if !g.headsUp {
		// Stop accepting more streams now.
		t.closing = true
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
	// For a graceful close, send out a GoAway with stream WorkloadID of MaxUInt32,
	// Follow that with a ping and wait for the ack to come back or a timer
	// to expire. During this time accept new streams since they might have
	// originated before the GoAway reaches the client.
	// After getting the ack or timer expiration send out another GoAway this
	// time with an WorkloadID of the max stream server intends to process.
	if err := t.framer.fr.WriteGoAway(math.MaxUint32, frame.ErrCodeNo, []byte{}); err != nil {
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
