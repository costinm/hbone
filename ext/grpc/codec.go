package grpc

import (
	"net/http"

	"google.golang.org/protobuf/proto"
)

// github.com/golang/protobuf/proto" is now implemented as wrapper on protobuf
//

// MsgStream represents a stream of marshalled objects. It allows receiving and
// sending objects.
//
// Normal Marshal interface returns or takes a []byte - which is not friendly to
// GC and streaming interfaces.
type MsgStream interface {
	// CloseSend sends a FIN and closes the stream gracefully.
	// TODO: add a status / trialer headers
	CloseSend() error

	// Recv unmarshal the next object from a frame.
	Recv(interface{}) error

	// Send marshals and sends an object
	Send(dr interface{}) error
}

type ProtoStream struct {
	// The Resolver can be customized, to avoid using protoregistry.GlobalTypes for Any
	UnMarshaller *proto.UnmarshalOptions
	Marshaller   *proto.MarshalOptions

	// Expose the http objects
	Request        *http.Request
	Response       *http.Response
	ResponseWriter http.ResponseWriter

	outbuf []byte
	inbuf  []byte
}

func New() *ProtoStream {
	return &ProtoStream{
		Marshaller: &proto.MarshalOptions{
			UseCachedSize: true,
		},
		UnMarshaller: &proto.UnmarshalOptions{},
	}
}

func (ps *ProtoStream) Recv(out interface{}) error {

	marshaller := proto.UnmarshalOptions{}
	marshaller.Unmarshal(ps.inbuf, out.(proto.Message))
	return nil
}

func (ps *ProtoStream) Send(out interface{}, last bool) error {
	marshaller := proto.MarshalOptions{}

	bb, err := marshaller.MarshalAppend(ps.outbuf[5:], out.(proto.Message))
	ps.outbuf = bb

	return err
}
