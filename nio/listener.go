package nio

import (
	"fmt"
	"net"
)

type StreamListener struct {
	closed   chan struct{}
	incoming chan net.Conn
	Address  net.Addr
}

func newListener() *StreamListener {
	return &StreamListener{
		incoming: make(chan net.Conn),
		closed:   make(chan struct{}),
	}
}

func (l *StreamListener) Close() error {
	l.closed <- struct{}{}
	return nil
}

func (l *StreamListener) Addr() net.Addr {
	return l.Address
}

func (l *StreamListener) Accept() (net.Conn, error) {
	for {
		select {
		case c, ok := <-l.incoming:
			if !ok {
				return nil, fmt.Errorf("listener is closed")
			}
			return c, nil
		case <-l.closed:
			return nil, fmt.Errorf("listener is closed")
		}
	}
}
