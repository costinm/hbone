package urpc

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"net/http"

	"github.com/costinm/hbone"
	"github.com/costinm/hbone/h2"
	"github.com/costinm/hbone/nio"
	"google.golang.org/protobuf/proto"
)

// Light gRPC encoding/decoding using hbone h2 stack.

// grpc.RoundTripStart creates a client, which wraps one or more h2 connections
// to that host. It returns a ClientConn.
//
// The grpc generated stubs use grpc.ClientConnInterface
//
//// ClientConnInterface defines the functions clients need to perform unary and
//// streaming RPCs.  It is implemented by *ClientConn, and is only intended to
//// be referenced by generated code.
//type ClientConnInterface interface {
//	// Invoke performs a unary RPC and returns after the response is received
//	// into reply.
//	Invoke(ctx context.Context, method string, args interface{}, reply interface{}, opts ...CallOption) error
//	// NewStream begins a streaming RPC.
//	NewStream(ctx context.Context, desc *StreamDesc, method string, opts ...CallOption) (ClientStream, error)
//}
// Options are passed in CallOption, and metadata in context.

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

// UGRPC represents a single gRPC transaction - unary or stream request.
type UGRPC struct {
	Stream *h2.H2Stream

	// Streaming support
	// Used for framing responses
	// lastFrame holds the last received frame. It will be reused by Recv if not nil.
	// The caller can take ownership of the frame by setting this to nil.
	lastFrame *nio.Buffer
	rbuffer   *nio.Buffer
}

func New(ctx context.Context, c *hbone.Cluster, host, path string) (*UGRPC, error) {
	req, _ := http.NewRequestWithContext(ctx, "POST", "https://"+host+path, nil)
	req.Header.Add("content-type", "application/grpc")
	req.Header.Add("te", "trailers") // TODO: can it be removed ?

	h2c := h2.NewStreamReq(req)

	// Find or dial an h2 transport
	tr, err := c.FindTransport(ctx)
	h2c.SetTransport(tr, true)

	return &UGRPC{Stream: h2c}, err
}

func NewFromStream(s *h2.H2Stream) *UGRPC {
	return &UGRPC{Stream: s}
}

// Invoke implements a minimal gRPC protocol.
func (u *UGRPC) Invoke(args interface{}, reply interface{}) error {
	s := u.Stream

	_, err := s.Transport().DialStream(s)
	//s.RoundTripStart()

	pm := args.(proto.Message)
	m := proto.MarshalOptions{UseCachedSize: true}
	sz := m.Size(pm)
	bb := s.GetWriteFrame(sz)

	// Slice = pointer to start position on underlying array, len, capacity - space from end of data to real array end
	// bb.bytes: realArray[start:end]
	// bout: either realArray[start:end+sz] or newArray[], similar to append()
	//
	// Problem: bb may have a prefix, which is lost if we use start:end. We need 0:end
	bout, _ := proto.MarshalOptions{}.MarshalAppend(bb.BytesAppend(), pm)

	bb.UpdateAppend(bout)

	err = s.Send(bb)
	if err != nil {
		return err
	}
	s.CloseWrite()

	s.WaitHeaders()

	f, err := u.Recv()
	if err != nil {
		return err
	}
	err = proto.Unmarshal(f.Bytes(), reply.(proto.Message))

	return err
}

func (u *UGRPC) SendMsg(args interface{}) error {
	return SendMsg(u.Stream, args)
}

func (u *UGRPC) RecvMsg(args interface{}) error {
	f, err := u.Recv()
	if err != nil {
		return err
	}
	err = proto.Unmarshal(f.Bytes(), args.(proto.Message))
	return err
}

func (u *UGRPC) Recv() (*nio.Buffer, error) {
	hc := u.Stream

	if u.rbuffer == nil {
		u.rbuffer = nio.GetBuffer(0, 0)
	}
	b := u.rbuffer

	if b.Len() > 0 {
		b.Discard(int(5 + len(u.lastFrame.Bytes())))
	} else {
		//log.Println("Header: ", hc.Res.Header)
		// Initial headers don't include grpc-status - just 200
		// grpc-encoding - compression

	}

	// TODO: only if an incomplete frame.
	if !b.IsEmpty() {
		b.Compact()
	}

	hc.RcvdPackets++

	var mlen uint32
	head, err := b.Fill(hc, 5)
	if err == nil {
		if head[0] != 0 {
			return nil, fmt.Errorf("Invalid frame %d", head[0])
		}
		mlen = binary.BigEndian.Uint32(head[1:])

		_, err = b.Fill(hc, int(5+mlen))
	}
	// At this point, Res should be set.

	if err == io.EOF {
		// TODO: extract, see http2-client in grpc
		// grpc-status
		// grpc-message
		// grpc-status-details-bin - base64 proto
		if hc.Response != nil {
			log.Println("Trailer", hc.Response.Trailer)
			hc.Response.Body.Close()
		}
	} else if err != nil {
		return nil, err
	}

	if hc.Response.Trailer.Get("") != "" {

	}

	u.lastFrame = b.Frame(5, int(5+mlen))
	return u.lastFrame, err
}

func (u *UGRPC) Recv4() (*nio.Buffer, error) {
	hc := u.Stream

	if u.rbuffer == nil {
		u.rbuffer = nio.GetBuffer(0, 0)
	}
	b := u.rbuffer

	if b.Len() > 0 {
		b.Discard(int(4 + len(u.lastFrame.Bytes())))
	}

	if !b.IsEmpty() {
		b.Compact()
	}

	var mlen uint32

	head, err := b.Fill(hc, 4)
	if err == nil {
		mlen = binary.BigEndian.Uint32(head[0:])

		_, err = b.Fill(hc, int(4+mlen))
	}

	if err != nil {
		return nil, err
	}

	hc.RcvdPackets++
	u.lastFrame = b.Frame(4, int(4+mlen))
	return u.lastFrame, err
}

func SendMsg(s *h2.H2Stream, args interface{}) error {
	pm := args.(proto.Message)
	m := proto.MarshalOptions{UseCachedSize: true}
	sz := m.Size(pm)
	bb := s.GetWriteFrame(sz)
	bout, _ := m.MarshalAppend(bb.BytesAppend(), pm)
	bb.UpdateAppend(bout)
	err := s.Send(bb)
	if err != nil {
		return err
	}

	return err
}
