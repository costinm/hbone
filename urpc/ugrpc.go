package uxds

import (
	"context"
	"io"
	"net/http"

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
func NewGRPCStream(ctx context.Context, c http.RoundTripper, host, path string) *nio.Stream {
	// TODO: non-blocking implementation, to avoid an extra gorutine for Write and RoundTrip.
	in, out := io.Pipe()

	req, _ := http.NewRequestWithContext(ctx, "POST", "https://"+host+path, in)
	req.Header.Add("content-type", "application/grpc")
	req.Header.Add("te", "trailers") // TODO: can it be removed ?

	//req.Header.Add("grpc-timeout", "10S")

	r := &nio.Stream{
		Out:     out,
		Request: req,
	}

	r.Cluster = c

	return r
}

func GRPCHandler(f func(ctx context.Context, args proto.Message) (interface{}, error),
	preq, pres proto.Message) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		s := nio.NewStreamRequest(request, writer, nil)

		f, err := s.Recv(true)
		if err != nil {
			writer.WriteHeader(500)
			return
		}
		pres1 := pres.ProtoReflect().New()
		err = proto.Unmarshal(f.Bytes(), pres1.(proto.Message))

	})
}

func SendGPRC(s *nio.Stream, args interface{}, reply interface{}) error {

	s.RoundTripStart()

	bb := s.GetWriteFrame()
	bout, _ := proto.MarshalOptions{}.MarshalAppend(bb.Bytes(), args.(proto.Message))
	bb.UpdateAppend(bout)
	err := s.Send(bb)
	if err != nil {
		return err
	}
	s.CloseWrite()

	f, err := s.Recv(true)
	if err != nil {
		return err
	}
	err = proto.Unmarshal(f.Bytes(), reply.(proto.Message))

	return err
}
