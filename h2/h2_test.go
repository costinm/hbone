package h2

import (
	"bytes"
	"context"
	"io"
	"log"
	"net/http"
	"net/url"
	"testing"

	"github.com/costinm/ugate/nio"
)

// Most of the testing is done in the hbone package.

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
			err := pair.ClientTransport.DialStream(clientStream)
			if err != nil {
				b.Fatal(err)
			}

			serverStream := <-pair.AcceptedStreams

			serverStream.WriteHeader(200)

			clientStream.WaitHeaders()

			serverStream.CloseWrite()

		}
	})

	b.Run("NoDataClose", func(b *testing.B) {
		for n := 0; n < b.N; n++ {
			clientStream := NewStreamReq(&http.Request{
				Method: "CONNECT",
				Header: map[string][]string{
					":protocol": {"webtransport"},
					"origin":    {"example.com"}},
				Host: "example.com", // currently required
			})
			err := pair.ClientTransport.DialStream(clientStream)
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
		err := pair.ClientTransport.DialStream(clientStream)
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
		err := pair.ClientTransport.DialStream(clientStream)
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
	pair := NewClientServerTransport()
	defer pair.Listener.Close()

	t.Run("Example-RoundTripper", func(t *testing.T) {
		cs, ss, err := pair.TcpPair()
		defer cs.Close()
		defer ss.Close()

		if err != nil {
			t.Fatal(err)
		}

		go func() {
			// Got a client and server stream.
			se := &Events{}
			// Blocks until it gets the handshake done
			s, err := NewServerConnection(ss, &ServerConfig{}, se)
			if err != nil {
				t.Fatal(err)
			}
			s.Handle = func(stream *H2Stream) {
				log.Println("Stream", "stream", stream.Request)
				stream.SendRaw([]byte("123456789hello"), 9, 14, true)
				//stream.Close()
			}
			// blocking
			s.HandleStreams()
		}()

		// Client side
		c, err := NewConnection(context.Background(), H2Config{})
		c.StartConn(cs)

		// c is a client connection, s is a server connection.
		req, _ := http.NewRequest("POST", "/test", bytes.NewReader([]byte("hello")))
		// Blocking
		res, err := c.RoundTrip(req)
		log.Println(res, err)
		hres := res.Body.(*H2Stream)
		resb := make([]byte, 1024)
		nr, err := hres.Read(resb)
		log.Println("Res: ", resb[0:nr])
	})

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
		err := pair.ClientTransport.DialStream(s1cs)
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
		err := pair.ServerTransport.DialStream(s2sc)
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
		err := pair.ClientTransport.DialStream(clientStream)
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

	if clientStream.MuxID != serverStream.MuxID {
		t.Fatal("Invalid id", clientStream.MuxID, serverStream.MuxID)
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
