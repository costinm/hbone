package nio

import (
	"context"
	"crypto/tls"
	"io"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// TODO: benchmark different sizes.
var Debug = false

// ReaderCopier copies from In to Out
type ReaderCopier struct {
	// Number of bytes copied.
	Written int64

	// First error - may be on reading from In (InError=true) or writing to Out.
	Err error

	InError bool

	In  io.Reader
	Out io.Writer

	// An ID of the copier, for debug purpose.
	ID string
}

// Verify if in and out can be spliced. Used by proxy code to determine best
// method to copy.
//
// Tcp connections implement ReadFrom, not WriteTo
// ReadFrom is only spliced in few cases
func CanSplice(in io.Reader, out io.Writer) bool {
	if _, ok := in.(*net.TCPConn); ok {
		if _, ok := out.(*net.TCPConn); ok {
			return true
		}
	}
	return false
}

// Copy will copy src to dst, using a pooled intermediary buffer.
//
// Blocking, returns when src returned an error or EOF/graceful close.
//
// May also return with error if src or dst return errors.
//
// Copy may be called in a go routine, for one of the streams in the
// connection - the stats and error are returned on a channel.
func (s *ReaderCopier) Copy(ch chan int, close bool) {
	buf1 := bufferPoolCopy.Get().([]byte)
	defer bufferPoolCopy.Put(buf1)
	bufCap := cap(buf1)
	buf := buf1[0:bufCap:bufCap]

	//st := ReaderCopier{}

	// For netstack: src is a gonet.ReaderCopier, doesn't implement WriterTo. Dst is a net.TcpConn - and implements ReadFrom.
	// Copy is the actual implementation of Copy and CopyBuffer.
	// if buf is nil, one is allocated.
	// Duplicated from io

	// This will prevent stats from working.
	// If the reader has a WriteTo method, use it to do the copy.
	// Avoids an allocation and a copy.
	//if wt, ok := src.(io.WriterTo); ok {
	//	return wt.WriteTo(dst)
	//}
	// Similarly, if the writer has a ReadFrom method, use it to do the copy.
	//if rt, ok := dst.(io.ReaderFrom); ok {
	//	return rt.ReadFrom(src)
	//}
	if ch != nil {
		defer func() {
			ch <- int(0)
		}()
	}
	if s.ID == "" {
		s.ID = strconv.Itoa(int(atomic.AddUint32(&StreamId, 1)))
	}
	if Debug {
		log.Println(s.ID, "startCopy()")
	}
	for {
		if srcc, ok := s.In.(net.Conn); ok {
			srcc.SetReadDeadline(time.Now().Add(15 * time.Minute))
		}
		nr, er := s.In.Read(buf)
		if Debug {
			log.Println(s.ID, "read()", nr, er)
		}
		if nr > 0 { // before dealing with the read error
			nw, ew := s.Out.Write(buf[0:nr])
			if Debug {
				log.Println(s.ID, "write()", nw, ew)
			}
			if nw > 0 {
				s.Written += int64(nw)
			}
			if f, ok := s.Out.(http.Flusher); ok {
				f.Flush()
			}
			if nr != nw { // Should not happen
				ew = io.ErrShortWrite
				if Debug {
					log.Println(s.ID, "write error - short write", s.Err)
				}
			}
			if ew != nil {
				s.Err = ew
				return
			}
		}
		if er != nil {
			if strings.Contains(er.Error(), "NetworkIdleTimeout") {
				er = io.EOF
			}
			if er == io.EOF {
				if Debug {
					log.Println(s.ID, "done()")
				}
			} else {
				s.Err = er
				s.InError = true
				if Debug {
					log.Println(s.ID, "readError()", s.Err)
				}
			}
			if close {
				// read is already closed - we need to close out
				closeWriter(s.Out)
			}
			return
		}
	}
}

func closeWriter(dst io.Writer) error {
	if cw, ok := dst.(CloseWriter); ok {
		return cw.CloseWrite()
	}
	if c, ok := dst.(io.Closer); ok {
		return c.Close()
	}
	if rw, ok := dst.(http.ResponseWriter); ok {
		// Server side HTTP stream. For client side, FIN can be sent by closing the pipe (or
		// request body). For server, the FIN will be sent when the handler returns - but
		// this only happen after request is completed and body has been read. If server wants
		// to send FIN first - while still reading the body - we are in trouble.

		// That means HTTP2 TCP servers provide no way to send a FIN from server, without
		// having the request fully read.

		// This works for H2 with the current library - but very tricky, if not set as trailer.
		rw.Header().Set("X-Close", "0")
		rw.(http.Flusher).Flush()
		return nil
	}
	log.Println("Server out not Closer nor CloseWriter nor ResponseWriter", dst)
	return nil
}

func Proxy(ctx context.Context, cin io.Reader, cout io.WriteCloser, sin io.Reader, sout io.WriteCloser) error {
	ch := make(chan int)
	s1 := &ReaderCopier{
		ID:  "client-o",
		Out: sout,
		In:  cin,
	}
	go s1.Copy(ch, true)

	s2 := &ReaderCopier{
		ID:  "client-i",
		Out: cout,
		In:  sin,
	}
	s2.Copy(nil, true)
	<-ch
	if s1.Err != nil {
		return s1.Err
	}
	return s2.Err
}

// HTTPConn wraps a http server request/response in a net.Conn, used mainly as a parameter
// to tls.Client and tls.Server, if the inner H2 stream uses (m)TLS.
type HTTPConn struct {
	R io.Reader
	W io.Writer

	// Conn is the underlying connection, for getting local/remote address.
	Conn net.Conn
}

func (hc *HTTPConn) Read(b []byte) (n int, err error) {
	return hc.R.Read(b)
}

// Write wraps the writer, which can be a http.ResponseWriter.
// Will make sure Flush() is called - normal http is buffering.
func (hc *HTTPConn) Write(b []byte) (n int, err error) {
	n, err = hc.W.Write(b)
	if f, ok := hc.W.(http.Flusher); ok {
		f.Flush()
	}
	return
}

func (hc *HTTPConn) Close() error {
	// TODO: close write
	if cw, ok := hc.W.(CloseWriter); ok {
		return cw.CloseWrite()
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
	return hc.Conn.SetReadDeadline(t)
}

func (hc *HTTPConn) SetWriteDeadline(t time.Time) error {
	return hc.Conn.SetWriteDeadline(t)
}

type tlsHandshakeTimeoutError struct{}

func (tlsHandshakeTimeoutError) Timeout() bool   { return true }
func (tlsHandshakeTimeoutError) Temporary() bool { return true }
func (tlsHandshakeTimeoutError) Error() string   { return "net/http: TLS handshake timeout" }

// HandshakeTimeout wraps tlsConn.Handshake with a timeout, to prevent hanging connection.
func HandshakeTimeout(tlsConn *tls.Conn, d time.Duration, plainConn net.Conn) error {
	errc := make(chan error, 2)
	var timer *time.Timer // for canceling TLS handshake
	if d == 0 {
		d = 3 * time.Second
	}
	timer = time.AfterFunc(d, func() {
		errc <- tlsHandshakeTimeoutError{}
	})
	go func() {
		err := tlsConn.Handshake()
		if timer != nil {
			timer.Stop()
		}
		errc <- err
	}()
	if err := <-errc; err != nil {
		if plainConn != nil {
			plainConn.Close()
		} else {
			tlsConn.Close()
		}
		return err
	}
	return nil
}

func ServeListener(l net.Listener, f func(conn net.Conn)) error {
	for {
		remoteConn, err := l.Accept()
		if err != nil {
			if ne, ok := err.(interface {
				Temporary() bool
			}); ok && ne.Temporary() {
				time.Sleep(100 * time.Millisecond)
				continue
			}
			// TODO: callback to notify. This may happen if interface restarts, etc.
			log.Println("Accepted done ", l)
			return err
		}

		// TODO: set read/write deadlines

		go f(remoteConn)
	}
}
