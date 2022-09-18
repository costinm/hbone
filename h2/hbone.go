package h2

import (
	"context"
	"encoding/binary"
	"fmt"
	"log"
	"net"
	"net/http"
	"sync"
	"sync/atomic"

	http2 "github.com/costinm/hbone/h2/frame"
	"github.com/costinm/hbone/nio"
)

// newHTTP2Client constructs a connected ClientTransport to addr based on HTTP2
// and starts to receive messages on it. Non-nil error returns if construction
// fails.
func NewHTTP2Client(connectCtx, ctx context.Context, conn net.Conn, opts ConnectOptions, onPrefaceReceipt func(), onGoAway func(GoAwayReason), onClose func()) (_ *HTTP2ClientMux, err error) {
	scheme := "http"
	ctx, cancel := context.WithCancel(ctx)
	defer func() {
		if err != nil {
			cancel()
		}
	}()

	// Any further errors will close the underlying connection
	defer func(conn net.Conn) {
		if err != nil {
			conn.Close()
		}
	}(conn)
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

	dynamicWindow := true
	icwz := int32(initialWindowSize)
	if opts.InitialConnWindowSize >= defaultWindowSize {
		icwz = opts.InitialConnWindowSize
		dynamicWindow = false
	}
	writeBufSize := opts.WriteBufferSize
	readBufSize := opts.ReadBufferSize
	maxHeaderListSize := defaultClientMaxHeaderListSize
	if opts.MaxHeaderListSize != nil {
		maxHeaderListSize = *opts.MaxHeaderListSize
	}
	fs := uint32(http2MaxFrameLen)
	if opts.MaxFrameSize != 0 {
		fs = opts.MaxFrameSize
	}
	t := &HTTP2ClientMux{
		ctx:           ctx,
		ctxDone:       ctx.Done(), // Cache Done chan.
		cancel:        cancel,
		userAgent:     opts.UserAgent,
		conn:          conn,
		readerDone:    make(chan struct{}),
		writerDone:    make(chan struct{}),
		goAway:        make(chan struct{}),
		framer:        newFramer(conn, writeBufSize, readBufSize, maxHeaderListSize, fs),
		fc:            &trInFlow{limit: uint32(icwz)},
		scheme:        scheme,
		activeStreams: make(map[uint32]*Stream),
		isSecure:      isSecure,
		//perRPCCreds:           perRPCCreds,
		kp:                    kp,
		initialWindowSize:     initialWindowSize,
		onPrefaceReceipt:      onPrefaceReceipt,
		nextID:                1,
		maxConcurrentStreams:  defaultMaxStreamsClient,
		streamQuota:           defaultMaxStreamsClient,
		streamsQuotaAvailable: make(chan struct{}, 1),
		czData:                new(channelzData),
		onGoAway:              onGoAway,
		onClose:               onClose,
		keepaliveEnabled:      keepaliveEnabled,
		bufferPool:            newBufferPool(),
	}

	t.controlBuf = newControlBuffer(t.ctxDone)
	if opts.InitialWindowSize >= defaultWindowSize {
		t.initialWindowSize = opts.InitialWindowSize
		dynamicWindow = false
	}
	if dynamicWindow {
		t.bdpEst = &bdpEstimator{
			bdp:               initialWindowSize,
			updateFlowControl: t.updateFlowControl,
		}
	}
	//t.channelzID, err = channelz.RegisterNormalSocket(t, opts.ChannelzParentID, fmt.Sprintf("%s -> %s", t.localAddr, t.remoteAddr))
	//if err != nil {
	//	return nil, err
	//}
	if t.keepaliveEnabled {
		t.kpDormancyCond = sync.NewCond(&t.mu)
		go t.keepalive()
	}
	// Start the reader goroutine for incoming message. Each transport has
	// a dedicated goroutine which reads HTTP2 frame from network. Then it
	// dispatches the frame to the corresponding stream entity.
	go t.reader()

	// Send connection preface to server.
	n, err := t.conn.Write(clientPreface)
	if err != nil {
		err = connectionErrorf(true, err, "transport: failed to write client preface: %v", err)
		t.Close(err)
		return nil, err
	}
	if n != len(clientPreface) {
		err = connectionErrorf(true, nil, "transport: preface mismatch, wrote %d bytes; want %d", n, len(clientPreface))
		t.Close(err)
		return nil, err
	}
	var ss []http2.Setting

	if t.initialWindowSize != defaultWindowSize {
		ss = append(ss, http2.Setting{
			ID:  http2.SettingInitialWindowSize,
			Val: uint32(t.initialWindowSize),
		})
	}
	ss = append(ss, http2.Setting{
		ID:  http2.SettingMaxFrameSize,
		Val: fs,
	})
	t.FrameSize = fs
	if opts.MaxHeaderListSize != nil {
		ss = append(ss, http2.Setting{
			ID:  http2.SettingMaxHeaderListSize,
			Val: *opts.MaxHeaderListSize,
		})
	}
	err = t.framer.fr.WriteSettings(ss...)
	if err != nil {
		err = connectionErrorf(true, err, "transport: failed to write initial settings frame: %v", err)
		t.Close(err)
		return nil, err
	}
	// Adjust the connection flow control window if needed.
	if delta := uint32(icwz - defaultWindowSize); delta > 0 {
		if err := t.framer.fr.WriteWindowUpdate(0, delta); err != nil {
			err = connectionErrorf(true, err, "transport: failed to write window update: %v", err)
			t.Close(err)
			return nil, err
		}
	}

	t.connectionID = atomic.AddUint64(&clientConnectionCounter, 1)

	if err := t.framer.writer.Flush(); err != nil {
		return nil, err
	}
	go func() {
		t.loopy = newLoopyWriter(clientSide, t.framer, t.controlBuf, t.bdpEst, fs)
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
	return t, nil
}

func (t *HTTP2ClientMux) RoundTrip(request *http.Request) (*http.Response, error) {
	res, err := t.Dial(request)
	if err != nil {
		return nil, err
	}
	s := res.Body.(*Stream)
	s.waitOnHeader()

	return res, err
}

func (t *HTTP2ClientMux) Dial(request *http.Request) (*http.Response, error) {
	s, err := t.NewStream(request.Context(), &CallHdr{
		Req: request,
		DoneFunc: func() {
			log.Println("Dial ", request.Host, "done")
		},
	})
	if err != nil {
		return nil, err
	}
	// TODO: special header or method to indicate the Body of Request supports optimized
	// copy ( no io.Pipe !!)
	if request.Body != nil {
		go func() {
			s1 := &nio.ReaderCopier{
				ID:  fmt.Sprintf("req-body-%d", s.Id),
				Out: s,
				In:  request.Body,
			}
			s1.Copy(nil, false)
			if s1.Err != nil {
				s1.Close()
			} else {
				s.CloseWrite()
			}
		}()
	}

	return s.Response, nil
}

const (
	Event_Response = 0
	Event_Data
	Event_Sent
	Event_FIN
	Event_RST
)

type Event struct {
	Type int
	Conn *Stream

	Error error

	Buffer *nio.Buffer
}

// NewGRPCStream creates a HTTPConn with a request set using gRPC framing and headers,
// capable of sending a stream of GRPC messages.
//
// https://github.com/grpc/grpc/blob/master/doc/PROTOCOL-HTTP2.md
//
// No protobuf processed or used - this is a low-level network interface. Caller or generated code can handle
// marshalling.
// It only provides gRPC encapsulation, i.e. 1 byte TAG, 4 byte len, msg[len]
// It can be used for both unary and streaming requests - unary gRPC just sends or receives a single request.
//
// Caller should set Req.Headers, including:
// - Set content-type to a specific subtype ( default is set to application/grpc )
// - add extra headers
//
// For response, the caller must handle headers like:
//   - trailer grpc-status, grpc-message, grpc-status-details-bin, grpc-encoding
//   - http status codes other than 200 (which is expected for gRPC)
func NewGRPCStream(ctx context.Context, host, path string) *Stream {

	req, _ := http.NewRequestWithContext(ctx, "POST", "https://"+host+path, nil)
	req.Header.Add("content-type", "application/grpc")
	req.Header.Add("te", "trailers") // TODO: can it be removed ?

	//req.Header.Add("grpc-timeout", "10S")
	return NewStreamReq(req)
}

func NewStreamReq(req *http.Request) *Stream {
	s := &Stream{
		Request: req,
		ctx:     req.Context(),
		//ct:            t,
		//doneFunc:      callHdr.DoneFunc,
		Response: &http.Response{
			Header:  http.Header{},
			Trailer: http.Header{},
			Request: req,
		},
	}
	s.Response.Body = s

	return s
}

// dial associates a Stream with a mux and sends the request.
func (t *HTTP2ClientMux) dial(s *Stream) (*http.Response, error) {
	s.ct = t
	s.wq = newWriteQuota(defaultWriteQuota, s.done)
	s.requestRead = func(n int) {
		t.adjustWindow(s, uint32(n))
	}
	// The client side stream context should have exactly the same life cycle with the user provided context.
	// That means, s.ctx should be read-only. And s.ctx is done iff ctx is done.
	// So we use the original context here instead of creating a copy.
	s.done = make(chan struct{})
	s.buf = newRecvBuffer()
	s.writeDoneChan = make(chan *dataFrame)
	s.headerChan = make(chan struct{})

	s.trReader = &transportReader{
		reader: &recvBufferReader{
			ctx:     s.ctx,
			ctxDone: s.ctx.Done(),
			recv:    s.buf,
			closeStream: func(err error) {
				t.CloseStream(s, err)
			},
			freeBuffer: t.bufferPool.put,
		},
		windowHandler: t,
		s:             s,
	}

	headerFields, err := t.createHeaderFields(s.ctx, s.Request)
	if err != nil {
		return nil, err
	}
	t.sendHeaders(s, headerFields)
	// TODO: special header or method to indicate the Body of Request supports optimized
	// copy ( no io.Pipe !!)
	if s.Request.Body != nil {
		go func() {
			s1 := &nio.ReaderCopier{
				ID:  fmt.Sprintf("req-body-%d", s.Id),
				Out: s,
				In:  s.Request.Body,
			}
			s1.Copy(nil, false)
			if s1.Err != nil {
				s1.Close()
			} else {
				s.CloseWrite()
			}
		}()
	}

	return s.Response, nil
}

// Return a buffer with reserved front space to be used for appending.
// If using functions like proto.Marshal, b.UpdateForAppend should be called
// with the new []byte. App should not touch the prefix.
func (hc *Stream) GetWriteFrame() *nio.Buffer {
	//if m.OutFrame != nil {
	//	return m.OutFrame
	//}
	b := nio.GetBuffer()
	b.WriteByte(0)
	b.WriteUnint32(0)
	return b
}

// Framed sending/receiving.
func (hc *Stream) Send(b *nio.Buffer) error {
	if !hc.isHeaderSent() {
		hc.WriteHeader(0)
	}

	hc.SentPackets++
	frameLen := b.Size() - 5
	binary.BigEndian.PutUint32(b.Bytes()[1:], uint32(frameLen))

	_, err := hc.Write(b.Bytes()) // this will flush too
	//if f, ok := hc.W.(http.Flusher); ok {
	//	hc.Flush()
	//}

	b.Recycle()

	//if ch != nil {
	//	err = <-ch
	//}

	return err
}
