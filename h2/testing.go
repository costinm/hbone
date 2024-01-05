package h2

import (
	"context"
	"log"
	"net"

	"github.com/costinm/ugate/nio"
)

// General helpers for testing

// ClientServerTransport creates a piped H2 client and server.
type ClientServerTransport struct {
	Listener net.Listener

	ServerTransport *H2Transport

	// All received H2 streams will be added here.
	AcceptedStreams chan *H2Stream

	ClientTransport *H2ClientTransport
}

func NewClientServerTransport() *ClientServerTransport {
	l, _ := net.Listen("tcp", ":0")
	return &ClientServerTransport{Listener: l}
}

func (p *ClientServerTransport) TcpPair() (net.Conn, net.Conn, error) {

	cs, err := net.Dial("tcp", p.Listener.Addr().String())
	if err != nil {
		return nil, nil, err
	}
	ss, err := p.Listener.Accept()
	return cs, ss, err
}

func (p *ClientServerTransport) InitPair() error {
	p.AcceptedStreams = make(chan *H2Stream, 1024)

	// accepted server transports
	svChan := make(chan *H2Transport, 1024)

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
