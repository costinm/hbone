package echo

import (
	"bytes"
	"encoding/binary"
	"io"
	"log"
	"net"
	"net/http"
	"time"

	"github.com/costinm/hbone/nio"
)

type EchoHandler struct {
	Debug       bool
	ServerFirst bool
	WaitFirst   time.Duration

	Received int
}

// Similar with echo TCP - but can't close
func (e *EchoHandler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	writer.WriteHeader(200)
	e.handleStreams(request.Body, writer)
}

func (e *EchoHandler) handle(str net.Conn) {
	if e.Debug {
		log.Println("Echo ", e.ServerFirst, str.RemoteAddr())
	}
	e.handleStreams(str, str)
}

func (e *EchoHandler) handleStreams(in io.Reader, out io.Writer) {

	d := make([]byte, 2048)

	//si := GetStreamInfo(str)
	//si.RemoteID=   RemoteID(str)
	//b1, _ := json.Marshal(si)

	b := &bytes.Buffer{}
	b.WriteString("Hello world\n")

	time.Sleep(e.WaitFirst)

	if e.ServerFirst {
		n, err := out.Write(b.Bytes())
		if e.Debug {
			log.Println("ServerFirst write()", n, err)
		}
	}
	//ac.SetDeadline(time.Now().StartListener(5 * time.Second))

	writeClosed := false
	for {
		n, err := in.Read(d)
		e.Received += n
		if e.Debug {
			log.Println("Echo read()", n, err)
		}
		if err != nil {
			if e.Debug {
				log.Println("ECHO DONE", err)
			}
			if err == io.EOF && e.ServerFirst {
				binary.BigEndian.PutUint32(d, uint32(n))
				out.Write(d[0:4])
				if cw, ok := out.(nio.CloseWriter); ok {
					cw.CloseWrite()
				}
			} else {
				if c, ok := in.(io.Closer); ok {
					c.Close()
				}
				if c, ok := out.(io.Closer); ok {
					c.Close()
				}
			}
			return
		}

		// Client requests server graceful close
		if d[0] == 0 {
			if wc, ok := out.(nio.CloseWriter); ok {
				wc.CloseWrite()
				writeClosed = true
				// Continue to read ! The test can check the read byte counts
			}
		}

		if !writeClosed {
			// TODO: add delay (based on req)
			out.Write(d[0:n])
			if e.Debug {
				log.Println("ECHO write")
			}
		}
		if f, ok := out.(http.Flusher); ok {
			f.Flush()
		}
	}
}

func (e *EchoHandler) Start(s string) (net.Listener, error) {
	el, err := net.Listen("tcp", s)
	if err != nil {
		return nil, err
	}
	go e.serve(el, e.handle)
	return el, nil
}

func (hb *EchoHandler) serve(l net.Listener, f func(conn net.Conn)) {
	for {
		remoteConn, err := l.Accept()
		if ne, ok := err.(net.Error); ok {
			if ne.Temporary() {
				time.Sleep(100 * time.Millisecond)
				continue
			}
		}
		if err != nil {
			log.Println("Accept error, closing listener ", err)
			return
		}

		go f(remoteConn)
	}
}
