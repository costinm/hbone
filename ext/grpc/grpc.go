package grpc

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"

	"github.com/costinm/hbone"
	"github.com/costinm/hbone/nio"
)

// GRPC framing and low-level protocol support. No protobuf processed or used -
// this is a low-level network interface. Caller or generated code can handle
// marshalling.

func newGRPCRequest(ctx context.Context, c *hbone.Cluster, path string, postdata []byte) *http.Request {
	head := []byte{0, 0, 0, 0, 0}
	binary.BigEndian.PutUint32(head[1:], uint32(len(postdata)))
	multi := io.MultiReader(bytes.NewReader(head), bytes.NewReader(postdata))

	req, _ := http.NewRequestWithContext(ctx, "POST", "https://"+c.Addr+path,
		multi)
	req.Header.Add("content-type", "application/grpc")
	req.Header.Add("te", "trailers") // TODO: can it be removed ?

	//req.Header.Add("grpc-timeout", "10S")
	return req
}

// NewGRPCStream creates a http.Request capable of sending a stream of GRPC messages.
// The returned writer will send messages - one Write per message.
// Not best interface - message buffers should be pooled, buffers with
// space in front to avoid 2 write - but this can be done later.
func NewGRPCStream(ctx context.Context, c *hbone.Cluster, path string) *GRPCFrameStream {
	// TODO: non-blocking implementation, to avoid an extra gorutine for Write and RoundTrip.
	in, out := io.Pipe()

	req, _ := http.NewRequestWithContext(ctx, "POST", "https://"+c.Addr+path, in)
	req.Header.Add("content-type", "application/grpc")
	req.Header.Add("te", "trailers") // TODO: can it be removed ?

	//req.Header.Add("grpc-timeout", "10S")
	r := NewGRPCFrameStream(out, req)
	r.c = c
	return r
}

// DoGRPC implements a unary RPC over HBone, using the raw protocol.
// https://github.com/grpc/grpc/blob/master/doc/PROTOCOL-HTTP2.md
func DoGRPC(ctx context.Context, c *hbone.Cluster, path string, postdata []byte) ([]byte, error) {
	req := newGRPCRequest(ctx, c, path, postdata)

	//
	resb, err := c.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	return grpcRes(c, resb)
}

type GrpcError struct {
	IOErr   error
	Headers http.Header
}

func grpcRes(c *hbone.Cluster, resp *http.Response) ([]byte, error) {
	defer resp.Body.Close()
	resb, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		log.Println(err)
		return nil, err
	}

	if resp.StatusCode >= 300 || resp.StatusCode < 200 {
		return nil, errors.New(fmt.Sprintf("status code %v",
			resp.StatusCode))
	}

	log.Println(resp.Trailer, resp.Header)

	// TODO: check status, must be 200
	if resb == nil || len(resb) < 5 {
		return nil, errors.New("gRPC response too small")
	}
	if resb[0] != 0 {
		return nil, fmt.Errorf("gRPC unexpected first byte %d", resb[0])
	}
	// TODO: check the returned length.

	// TODO: check the headers

	return resb[5:], nil
}

// GRPCFrameStream is a stream using gRPC encapsulation, i.e. 1 byte TAG, 4 byte len, msg[len]
// It can be used for both unary and streaming requests - unary gRPC just sends or receives a single request.
//
type GRPCFrameStream struct {
	InCount  int
	OutCount int

	LastFrame *Frame

	// Req holds the request object.
	Req *http.Request

	// Res holds the response, for client mode
	Res *http.Response
	// ResW holds the response writer, in server mode.
	ResW http.ResponseWriter

	// The out pipe of the Req, in client mode
	out io.Writer

	head []byte

	// Settings
	c *hbone.Cluster

	// buffer used for reads. Wrapped to Res.Body (client) and Req.Body (server)
	readBuf *nio.StreamBuffer

	// Saves the error from the RoundTrip, if any.
	// Also set if the response code is not the expected 200 or if any
	// protocol parsing error is found.
	RoundTripError error

	// Status, valid after the last message is received in client mode.
	// Should be sent before sending the last message in server mode.
	Status int

	StatusMessage string
}

func NewGRPCFrameStream(out io.Writer, req *http.Request) *GRPCFrameStream {
	return &GRPCFrameStream{
		out: out,
		Req: req,
	}
}

func (m *GRPCFrameStream) RoundTrip(ctx context.Context) error {
	hres, err := m.c.RoundTrip(m.Req)
	if err != nil {
		m.RoundTripError = err
		return err
	}
	m.Res = hres

	if hres.StatusCode >= 300 || hres.StatusCode < 200 {
		return errors.New(fmt.Sprintf("status code %v",
			hres.StatusCode))
	}
	m.readBuf = nio.NewBufferReader(hres.Body)

	return nil
}

func (m *GRPCFrameStream) Write(postdata []byte) (n int, err error) {
	head := []byte{0, 0, 0, 0, 0}
	binary.BigEndian.PutUint32(head[1:], uint32(len(postdata)))
	m.out.Write(head)
	n, err = m.out.Write(postdata)
	if f, ok := m.out.(http.Flusher); ok {
		f.Flush()
	}
	return n, err
}

func (m *GRPCFrameStream) GetWriteFrame() *nio.Buffer {
	b := nio.GetBuffer()
	b.WriteByte(0)
	b.WriteUnint32(0)
	return b
}

func (m *GRPCFrameStream) Send(b *nio.Buffer) error {
	if m.OutCount == 0 {
		// Needs to be in a go routine - Write is blocking until bytes are
		// sent, which happens during roundtrip.
		go func() {
			m.RoundTrip(m.Req.Context())
		}()
	}
	m.OutCount++
	frameLen := b.Size() - 5
	b.SetUnint32(1, uint32(frameLen))

	_, err := m.out.Write(b.Bytes())
	if f, ok := m.out.(http.Flusher); ok {
		f.Flush()
	}
	b.Recycle()
	return err
}

type Frame struct {
	Tag  int
	Data []byte
	s    *GRPCFrameStream
}

func (m *GRPCFrameStream) Recv() (*Frame, error) {
	b := m.readBuf

	if m.InCount > 0 {
		b.Discard(int(5 + len(m.LastFrame.Data)))
	} else {
		log.Println("Header: ", m.Res.Header)
		// Initial headers don't include grpc-status - just 200
		// grpc-encoding - compression

	}
	if !b.IsEmpty() {
		b.Compact()
	}

	m.InCount++

	head, err := b.Peek(5)
	if err != nil {
		return nil, err
	}

	if head[0] != 0 {
		return nil, fmt.Errorf("Invalid frame %d", head[0])
	}
	mlen := binary.BigEndian.Uint32(head[1:])

	buf, err := b.Peek(int(5 + mlen))
	if err != nil && err != io.EOF {
		return nil, err
	}

	if err == io.EOF {
		log.Println("Trailer", m.Res.Trailer)
		// TODO: extract, see http2-client in grpc
		// grpc-status
		// grpc-message
		// grpc-status-details-bin - base64 proto
	}

	m.LastFrame = &Frame{Data: buf[5 : mlen+5], s: m}
	return m.LastFrame, err
}
