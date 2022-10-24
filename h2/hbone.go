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

func (t *HTTP2ClientMux) StartConn(conn net.Conn) (err error) {
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
	if t.opts.MaxHeaderListSize != nil {
		ss = append(ss, http2.Setting{
			ID:  http2.SettingMaxHeaderListSize,
			Val: *t.opts.MaxHeaderListSize,
		})
	} else {
		ss = append(ss, http2.Setting{
			ID:  http2.SettingMaxHeaderListSize,
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
	// limit is initialized to initial con window size
	if delta := uint32(t.fc.limit - defaultWindowSize); delta > 0 {
		if err := t.framer.fr.WriteWindowUpdate(0, delta); err != nil {
			err = connectionErrorf(true, err, "transport: failed to write window update: %v", err)
			t.Close(err)
			return err
		}
	}

	t.connectionID = atomic.AddUint64(&clientConnectionCounter, 1)

	if err := t.framer.writer.Flush(); err != nil {
		t.Close(err)
		return err
	}
	go func() {
		t.loopy = newLoopyWriter(&t.H2Transport, clientSide, t.framer, t.controlBuf, t.bdpEst, fs)
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

func (t *H2Transport) RoundTrip(request *http.Request) (*http.Response, error) {
	res, err := t.Dial(request)
	if err != nil {
		return nil, err
	}

	s := res.Body.(*H2Stream)
	s.WaitHeaders()

	return res, err
}

// Dial sends a request (headers). Does not block waiting for response,
// but waits for available stream.
func (t *H2Transport) Dial(request *http.Request) (*http.Response, error) {
	s := NewStreamReq(request)
	_, err := t.DialStream(s)

	if err != nil {
		return nil, err
	}

	return s.Response, nil
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
func NewGRPCStream(ctx context.Context, host, path string) *H2Stream {

	req, _ := http.NewRequestWithContext(ctx, "POST", "https://"+host+path, nil)
	req.Header.Add("content-type", "application/grpc")
	req.Header.Add("te", "trailers") // TODO: can it be removed ?

	//req.Header.Add("grpc-timeout", "10S")
	return NewStreamReq(req)
}

func NewStreamReq(req *http.Request) *H2Stream {
	s := &H2Stream{
		Request: req,
		ctx:     req.Context(),
		Response: &http.Response{
			Header:  http.Header{},
			Trailer: http.Header{},
			Request: req,
		},
	}
	s.Response.Body = s

	return s
}

// dial associates a H2Stream with a mux and sends the request.
func (t *H2Transport) DialStream(s *H2Stream) (*http.Response, error) {
	s.ct = t
	s.done = make(chan struct{})
	s.writeDoneChan = make(chan *dataFrame)
	s.headerChan = make(chan struct{})

	s.wq = newWriteQuota(defaultWriteQuota, s.done)

	// The client side stream context should have exactly the same life cycle with the user provided context.
	// That means, s.ctx should be readBlocking-only. And s.ctx is done iff ctx is done.
	// So we use the original context here instead of creating a copy.
	s.trReader = nio.NewRecvBuffer(
		s.ctx.Done(), t.bufferPool.put, func(err error) {
			t.closeStream(s, err, true, http2.ErrCodeCancel)
		})

	t.streamEvent(EventStreamRequestStart, s)
	headerFields, err := t.createHeaderFields(s.ctx, s.Request)
	if err != nil {
		return nil, err
	}
	err = t.writeRequestHeaders(s, headerFields)
	if err != nil {
		return nil, err
	}
	s.updateHeaderSent()

	t.streamEvent(EventStreamStart, s)
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
func (hc *H2Stream) GetWriteFrame() *nio.Buffer {
	//if m.OutFrame != nil {
	//	return m.OutFrame
	//}
	b := nio.GetBuffer()
	b.WriteByte(0)
	b.WriteUnint32(0)
	return b
}

func (hc *H2Stream) Send(b []byte, start, end int, last bool) error {
	hc.SentPackets++
	return hc.Transport().Send(hc, b, start, end, last)
}

func (hc *H2Stream) Transport() *H2Transport {
	if hc.ct != nil {
		return hc.ct
	}
	return hc.st
}

// Framed sending/receiving.
func (hc *H2Stream) SendDataFrame(b *nio.Buffer) error {

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
