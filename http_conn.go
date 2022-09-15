package hbone

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"time"

	"github.com/costinm/hbone/nio"
)

// HTTPConn wraps a http server request/response in a net.Conn, used as a parameter
// to tls.Client and tls.Server, if the inner H2 stream uses (m)TLS.
// It is also the result of Dial()
type HTTPConn struct {
	R io.Reader // res.Body for client,  req.Body for server
	W io.Writer // io.Pipe for client, ResponseWriter for server

	// Conn is the underlying connection, for getting local/remote address.
	Conn net.Conn

	nio.Stats

	// Req holds the request object.
	Req *http.Request

	// Res holds the response, for client mode
	Res *http.Response
	// ResW holds the response writer, in server mode.
	ResW http.ResponseWriter

	// Settings
	Cluster *Cluster

	// Used for framing responses
	// LastFrame holds the last received frame. It will be reused by Recv if not nil.
	// The caller can take ownership of the frame by setting this to nil.
	LastFrame *nio.Buffer

	// buffer used for reads. Wrapped to Res.Body (client) and Req.Body (server)
	readBuf *nio.Buffer

	// Saves the error from the RoundTrip, if any.
	// Also set if the response code is not the expected 200 or if any
	// protocol parsing error is found.
	RoundTripError error

	// Status, valid after the last message is received in client mode.
	// Should be sent before sending the last message in server mode.
	Status int

	StatusMessage string

	// Will receive a 'nil' or error on connect.
	// Will receive a nil or error on receive error (clean close or RST)
	ErrChan chan error

	// internal channel for roundtrip, for Recv thread to sync with response getting
	// received.
	rtCh chan error
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
	Conn *HTTPConn

	Error error

	Buffer *nio.Buffer
}

func NewHTTPConn(ctx context.Context, method, url string) *HTTPConn {
	// TODO: non-blocking implementation, to avoid an extra gorutine for Write and RoundTrip.
	in, out := io.Pipe()

	req, _ := http.NewRequestWithContext(ctx, method, url, in)

	//req.Header.Add("grpc-timeout", "10S")

	r := &HTTPConn{
		W:   out,
		Req: req,
	}

	return r
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
func NewGRPCStream(ctx context.Context, c *Cluster, path string) *HTTPConn {
	// TODO: non-blocking implementation, to avoid an extra gorutine for Write and RoundTrip.
	in, out := io.Pipe()

	req, _ := http.NewRequestWithContext(ctx, "POST", "https://"+c.Addr+path, in)
	req.Header.Add("content-type", "application/grpc")
	req.Header.Add("te", "trailers") // TODO: can it be removed ?

	//req.Header.Add("grpc-timeout", "10S")

	r := &HTTPConn{
		W:   out,
		Req: req,
	}
	r.Cluster = c

	return r
}

func (hc *HTTPConn) Start() {
	if hc.rtCh != nil || hc.Res != nil {
		return // already started
	}
	hc.rtCh = make(chan error)
	// Needs to be in a go routine - Write is blocking until bytes are
	// sent, which happens during roundtrip.
	go func() {
		hres, err := hc.Cluster.RoundTrip(hc.Req)
		if err != nil {
			hc.RoundTripError = err
		} else {
			hc.Res = hres

			if hres.StatusCode >= 300 || hres.StatusCode < 200 {
				err = errors.New(fmt.Sprintf("status code %v",
					hres.StatusCode))
			}
			hc.readBuf = nio.NewBufferReader(hc.Res.Body)
		}

		if err != nil {
			if c, ok := hc.W.(io.Closer); ok {
				c.Close()
			}
		}

		if hc.ErrChan != nil {
			hc.ErrChan <- err
		}

		hc.rtCh <- err
	}()
}

// NetConn returns the underlying network connection.
// This will be a tls.Conn in most cases.
func (hc *HTTPConn) NetConn() net.Conn {
	return hc.Conn
}

func (hc *HTTPConn) Read(b []byte) (n int, err error) {
	n, err = hc.R.Read(b)
	hc.RcvdBytes += n
	hc.LastRead = time.Now()
	return n, err
}

// Write wraps the writer, which can be a http.ResponseWriter.
// Will make sure Flush() is called - normal http is buffering.
func (hc *HTTPConn) Write(b []byte) (n int, err error) {
	n, err = hc.W.Write(b)
	if f, ok := hc.W.(http.Flusher); ok {
		f.Flush()
	}
	hc.LastWrite = time.Now()
	hc.SentBytes += n
	return
}

func (hc *HTTPConn) CloseWrite() error {
	// TODO: close write
	if cw, ok := hc.W.(nio.CloseWriter); ok {
		return cw.CloseWrite()
	}
	if cw, ok := hc.W.(io.Closer); ok {
		return cw.Close()
	}
	log.Println("Unexpected writer not implement CloseWriter")
	return nil
}

func (hc *HTTPConn) LocalAddr() net.Addr {
	return hc.Conn.LocalAddr()
}

func (hc *HTTPConn) RemoteAddr() net.Addr {
	return hc.Conn.RemoteAddr()
}

func (hc *HTTPConn) SetDeadline(t time.Time) error {
	err := hc.SetReadDeadline(t)
	if err != nil {
		return err
	}
	return hc.SetWriteDeadline(t)
}

func (hc *HTTPConn) SetReadDeadline(t time.Time) error {
	// TODO: body read timeout
	return nil // hc.Conn.SetReadDeadline(t)
}

func (hc *HTTPConn) SetWriteDeadline(t time.Time) error {
	return nil // hc.Conn.SetWriteDeadline(t)
}

func (hc *HTTPConn) Close() error {
	// TODO: send trailers if server !!!
	//hc.W.Close()
	if cw, ok := hc.W.(io.Closer); ok {
		cw.Close()
	}
	if cw, ok := hc.R.(io.Closer); ok {
		return cw.Close()
	}

	return nil
}

func (hc *HTTPConn) Recv(last bool) (*nio.Buffer, error) {
	if hc.Res != nil {
		if hc.Res.StatusCode >= 300 || hc.Res.StatusCode < 200 {
			return nil, errors.New(fmt.Sprintf("status code %v",
				hc.Res.StatusCode))
		}
	}

	if hc.RcvdPackets == 0 {
		err := <-hc.rtCh
		if err != nil {
			return nil, err
		}
	}

	b := hc.readBuf

	if hc.RcvdPackets > 0 {
		b.Discard(int(5 + len(hc.LastFrame.Bytes())))
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
	head, err := b.Peek(5)
	if err == nil {
		if head[0] != 0 {
			return nil, fmt.Errorf("Invalid frame %d", head[0])
		}
		mlen = binary.BigEndian.Uint32(head[1:])

		_, err = b.Peek(int(5 + mlen))
	}
	// At this point, Res should be set.

	if err == io.EOF {
		// TODO: extract, see http2-client in grpc
		// grpc-status
		// grpc-message
		// grpc-status-details-bin - base64 proto
		if hc.Res != nil {
			log.Println("Trailer", hc.Res.Trailer)
			hc.Res.Body.Close()
		}
	} else if err != nil {
		return nil, err
	}

	if hc.Res.Trailer.Get("") != "" {

	}

	hc.LastFrame = b.Frame(5, int(5+mlen))
	return hc.LastFrame, err
}

// Return a buffer with reserved front space to be used for appending.
// If using functions like proto.Marshal, b.UpdateForAppend should be called
// with the new []byte. App should not touch the prefix.
func (hc *HTTPConn) GetWriteFrame() *nio.Buffer {
	//if m.OutFrame != nil {
	//	return m.OutFrame
	//}
	b := nio.GetBuffer()
	b.WriteByte(0)
	b.WriteUnint32(0)
	return b
}

// Framed sending/receiving.
func (hc *HTTPConn) Send(b *nio.Buffer) error {
	if hc.rtCh == nil {
		hc.Start()
	}

	hc.SentPackets++
	frameLen := b.Size() - 5
	binary.BigEndian.PutUint32(b.Bytes()[1:], uint32(frameLen))

	_, err := hc.W.Write(b.Bytes())
	if f, ok := hc.W.(http.Flusher); ok {
		f.Flush()
	}

	b.Recycle()

	//if ch != nil {
	//	err = <-ch
	//}

	return err
}
