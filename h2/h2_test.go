package h2

import (
	"context"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"testing"

	"github.com/costinm/hbone/nio"
)

// Most of the testing is done in the hbone package.
type ClientServerTransport struct {
	Listener        net.Listener
	ClientTransport *H2ClientTransport
	ServerTransport *H2Transport

	AcceptedStreams chan *H2Stream
}

func (p *ClientServerTransport) InitPair() error {
	p.AcceptedStreams = make(chan *H2Stream)

	// accepted server transports
	svChan := make(chan *H2Transport)

	// Setup a H2 server on a listener
	l, err := nio.ListenAndServe(":0", func(conn net.Conn) {
		// Server event handlers
		se := &Events{}
		se.OnEvent(Event_Settings, EventHandlerFunc(func(evt EventType, t *H2Transport, s *H2Stream, f *nio.Buffer) {
			log.Println("SCONNECT START ", "maxStreams", t.maxStreams,
				"frameSize", t.FrameSize, "maxSendHeaderListSize", t.maxSendHeaderListSize)
		}))
		t, err := NewServerConnection(conn, &ServerConfig{}, se)
		if err != nil {
			log.Println(err)
			return
		}
		t.Handle = func(stream *H2Stream) {
			p.AcceptedStreams <- stream
		}
		go t.HandleStreams()

		svChan <- t
	})
	if err != nil {
		return err
	}
	p.Listener = l

	// Create a H2 Client connection to the server
	//ctx, cf := context.WithDeadline(context.Background(), time.Now().Add(10*time.Second))

	// Create the WebTransport for the client.
	ct, err := NewConnection(context.Background(), H2Config{})

	ct.Events.OnEvent(Event_Settings, EventHandlerFunc(func(evt EventType, t *H2Transport, s *H2Stream, f *nio.Buffer) {
		log.Println("CONNECT START ", "maxStreams", t.maxStreams,
			"frameSize", t.FrameSize, "maxSendHeaderListSize", t.maxSendHeaderListSize)
	}))
	ct.Handle = func(stream *H2Stream) {
		log.Println("Accepted stream ", stream)
		p.AcceptedStreams <- stream
	}

	ctcp, err := net.Dial("tcp", l.Addr().String())
	ct.StartConn(ctcp)

	// Get the accepted transport corresponding the client transport.
	st := <-svChan

	p.ClientTransport = ct
	p.ServerTransport = st
	return err
}

func BenchmarkH2(b *testing.B) {
	pair := &ClientServerTransport{}
	pair.InitPair()

	defer pair.Listener.Close()

	b.Run("NoData", func(b *testing.B) {
		for n := 0; n < b.N; n++ {
			clientStream := NewStreamReq(&http.Request{
				Method: "CONNECT",
				Header: map[string][]string{
					":protocol": {"webtransport"},
					"origin":    {"example.com"}},
				Host: "example.com", // currently required
			})
			_, err := pair.ClientTransport.DialStream(clientStream)
			if err != nil {
				b.Fatal(err)
			}

			serverStream := <-pair.AcceptedStreams

			serverStream.WriteHeader(200)

			clientStream.WaitHeaders()

			serverStream.CloseWrite()

		}
	})

	b.Run("DataBlocking", func(b *testing.B) {
		clientStream := NewStreamReq(&http.Request{
			Method: "CONNECT",
			Header: map[string][]string{
				":protocol": {"webtransport"},
				"origin":    {"example.com"}},
			Host: "example.com", // currently required
		})
		_, err := pair.ClientTransport.DialStream(clientStream)
		if err != nil {
			b.Fatal(err)
		}
		serverStream := <-pair.AcceptedStreams
		serverStream.WriteHeader(200)

		clientStream.WaitHeaders()
		buf := make([]byte, 2048)

		for n := 0; n < b.N; n++ {
			serverStream.Write(buf)
			clientStream.Read(buf)
		}
	})

	b.Run("DataSend", func(b *testing.B) {
		clientStream := NewStreamReq(&http.Request{
			Method: "CONNECT",
			Header: map[string][]string{
				":protocol": {"webtransport"},
				"origin":    {"example.com"}},
			Host: "example.com", // currently required
		})
		_, err := pair.ClientTransport.DialStream(clientStream)
		if err != nil {
			b.Fatal(err)
		}
		serverStream := <-pair.AcceptedStreams
		serverStream.WriteHeader(200)

		clientStream.WaitHeaders()
		buf := make([]byte, 2048)

		for n := 0; n < b.N; n++ {
			bufw := nio.GetDataBufferChunk(2048)
			serverStream.SendRaw(bufw, 0, 2048, false)
			clientStream.Read(buf)
		}
	})
}

func TestH2(t *testing.T) {
	pair := &ClientServerTransport{}
	pair.InitPair()

	defer pair.Listener.Close()

	t.Run("ClientToServer", func(t *testing.T) {
		s1cs := NewStreamReq(&http.Request{
			Method: "CONNECT",
			Header: map[string][]string{
				":protocol": {"webtransport"},
				"origin":    {"example.com"}},
			Host: "example.com", // currently required
		})
		_, err := pair.ClientTransport.DialStream(s1cs)
		if err != nil {
			t.Fatal(err)
		}

		s1sc := <-pair.AcceptedStreams
		checkSttreams(t, s1sc, s1cs, 2)
	})

	t.Run("ServerToClient", func(t *testing.T) {
		s2sc := NewStreamReq(&http.Request{
			URL: &url.URL{Path: "/", Host: "example.com"},
		})
		_, err := pair.ServerTransport.DialStream(s2sc)
		if err != nil {
			t.Fatal(err)
		}

		s2cs := <-pair.AcceptedStreams

		checkSttreams(t, s2cs, s2sc, 0)
	})

	t.Run("DataSend", func(t *testing.T) {
		clientStream := NewStreamReq(&http.Request{
			Method: "CONNECT",
			Header: map[string][]string{
				":protocol": {"webtransport"},
				"origin":    {"example.com"}},
			Host: "example.com", // currently required
		})
		_, err := pair.ClientTransport.DialStream(clientStream)
		if err != nil {
			t.Fatal(err)
		}
		serverStream := <-pair.AcceptedStreams
		serverStream.WriteHeader(200)

		clientStream.WaitHeaders()
		//uf := make([]byte, 2048)
		wsize := 1024

		for n := 0; n < 101; n++ {
			bufw := nio.GetDataBufferChunk(int64(wsize + 9))
			for i := 0; i < 256; i++ {
				bufw[i] = byte(i)
			}
			serverStream.SendRaw(bufw, 9, 9+wsize, n == 99)

			err = consume(clientStream, wsize)
			if err != nil && err != io.EOF {
				t.Fatal(err)
			} else if err == io.EOF {
				if n != 99 {
					// We may get the EOF in the next read
					t.Fatal("Early EOF")
				}
				break
			} else {
				if n == 99 {
					// We may get the EOF in the next read
					_, err := clientStream.RecvRaw()
					if err == io.EOF {
						break
					}
				}

			}
		}
	})
}

func consume(clientStream *H2Stream, wsize int) error {
	rcv := 0
	for rcv < wsize {
		bb, err := clientStream.RecvRaw()
		if err != nil {
			return err
		}
		rcv += bb.Len()
		nio.PutDataBufferChunk(bb.Bytes())

	}
	return nil
}

func checkSttreams(t *testing.T, serverStream *H2Stream, clientStream *H2Stream, expectedReqH int) {
	serverStream.WriteHeader(200)

	clientStream.WaitHeaders()

	// At this point session is established.

	if clientStream.Id != serverStream.Id {
		t.Fatal("Invalid id", clientStream.Id, serverStream.Id)
	}
	if clientStream.Response.Status != "200" {
		t.Fatal("Invalid status", clientStream.Response)
	}
	if len(serverStream.Request.Header) != expectedReqH {
		t.Error("Invalid header length", serverStream.Request.Header)
	}
	if serverStream.Request.Proto != "HTTP/2.0" || serverStream.Request.ProtoMajor != 2 {
		t.Error("Invalid proto", serverStream.Request.Proto)
	}
	if serverStream.Request.Host != "example.com" {
		t.Error("Invalid host", serverStream.Request.Host)
	}

	buf := make([]byte, 2048)

	serverStream.CloseWrite()
	_, err := clientStream.Read(buf)
	if err != io.EOF {
		t.Error("Invalid error", err)
	}
	// TODO: verify stats, field status, events
}
