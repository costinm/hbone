package h2

import (
	"context"
	"log"
	"net"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/costinm/hbone/nio"
)

// Most of the testing is done in the hbone package.

func TestH2(t *testing.T) {

	l, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	svChan := make(chan *H2Transport)

	// Server event handlers
	se := &Events{}

	se.OnEvent(Event_Settings, EventHandlerFunc(func(evt EventType, t *H2Transport, s *H2Stream, f *nio.Buffer) {
		log.Println("SCONNECT START ", "maxStreams", t.maxStreams,
			"frameSize", t.FrameSize, "maxSendHeaderListSize", t.maxSendHeaderListSize)
	}))

	go func() {
		for {
			lc, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				t, err := NewServerTransport(lc, &ServerConfig{}, se)
				if err != nil {
					log.Println(err)
					return
				}
				svChan <- t
			}()
		}
	}()

	ctx, cf := context.WithDeadline(context.Background(), time.Now().Add(10*time.Second))
	defer cf()

	ct, err := NewHTTP2Client(ctx, H2Config{}, func(reason GoAwayReason) {
	}, func() {
		log.Println("Muxc: Close ")
	})

	ct.Events.OnEvent(Event_Settings, EventHandlerFunc(func(evt EventType, t *H2Transport, s *H2Stream, f *nio.Buffer) {
		log.Println("CONNECT START ", "maxStreams", t.maxStreams,
			"frameSize", t.FrameSize, "maxSendHeaderListSize", t.maxSendHeaderListSize)
	}))

	ctcp, err := net.Dial("tcp", l.Addr().String())
	ct.StartConn(ctcp)

	st := <-svChan

	serverStreamsChan := make(chan *H2Stream)

	// TODO: replace with event, same for client
	st.Handle = func(stream *H2Stream) {
		log.Println("Accepted stream ", stream)
		serverStreamsChan <- stream
	}
	st.TraceCtx = func(ctx context.Context, s string) context.Context {
		//log.Println("Trace", s)
		return ctx
	}
	go st.HandleStreams()

	//log.Println(ct)

	// TODO: 'authority' in transport, it is per transport.

	// Eqivalent to ct.Dial(req) res
	s1cs := NewStreamReq(&http.Request{
		Host: "example.com", // currently required
		URL:  &url.URL{Path: "/"},
	})
	_, err = ct.DialStream(s1cs)
	if err != nil {
		t.Fatal(err)
	}

	log.Println(s1cs)
	s1sc := <-serverStreamsChan
	log.Println(s1sc)

}
