package h2

import (
	"context"
	"fmt"
	"net/http"

	http2 "github.com/costinm/hbone/h2/frame"
	"github.com/costinm/hbone/nio"
)

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
	s.SetTransport(t, true)
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
// Will send headers, but not wait for headers.
func (t *H2Transport) DialStream(s *H2Stream) (*http.Response, error) {
	s.ct = t
	s.done = make(chan struct{})
	s.writeDoneChan = make(chan *dataFrame)
	s.headerChan = make(chan struct{})

	s.outFlow = newWriteQuota(defaultWriteQuota, s.done)

	// The client side stream context should have exactly the same life cycle with the user provided context.
	// That means, s.ctx should be readBlocking-only. And s.ctx is done iff ctx is done.
	// So we use the original context here instead of creating a copy.
	s.inFrameList = nio.NewRecvBuffer(
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
