package h2

import "github.com/costinm/hbone/nio"

type EventType int

const (
	// Similar to GetConn in ClientTrace, but can provide data

	Event_Unknown EventType = iota

	// TODO: cluster event
	//Event_GetConn
	//Event_GotConn

	// TODO: XDS/on-demand/DNS abstraction for resolving clusters to ep IP
	//Event_DNS_Start
	//Event_DNS_Done

	// Client TCP connection starting, called before dialing the endpoint TCP conn
	Event_Connect_Start

	// TODO: handshake errors, all other conn or accept errors.
	//Event_TLSHandshakeStart
	//Event_TLSHandshakeDone

	// Server/client: Connect portion done, including handshake for settings.
	// TLS alpn and conn info available.
	Event_Connect_Done

	// Settings received from the other side.
	Event_Settings

	// Server: sent go away, start draining. Client: received go away, draining.
	Event_GoAway

	// Connection closed.
	Event_ConnClose

	// H2Stream-level
	Event_Response

	// Generated before sending headers for requests only. May add headers.
	EventStreamRequestStart

	EventStreamStart

	Event_WroteHeaders

	// For net.http, this is called on the first byte of response header.
	// For hbone and h2, on the receipt of the full header frame.
	EventGotFirstResponseByte

	// In http interceptor: WroteRequest(err)
	EventSendDone
	EventReceiveDone

	EventStreamClosed

	// Data events
	Event_FrameReceived
	Event_FrameSent

	EventLAST
)

type eventChain struct {
	chain []EventHandler
}

func (f eventChain) HandleEvent(evt EventType, t *H2Transport, s *H2Stream, d *nio.Buffer) {
	for _, eh := range f.chain {
		eh.HandleEvent(evt, t, s, d)
	}
}

func (e *Events) GetHandler(t EventType) EventHandler {
	if e.eventHandlers == nil {
		return nil
	}
	eh := e.eventHandlers[t]
	return eh
}

func (s *H2Stream) StreamDataEvent(t EventType) {
	eh := s.GetHandler(t)
	if eh == nil {
		return
	}

	eh.HandleEvent(t, nil, s, nil)
}

func (s *H2Stream) StreamEvent(t EventType) {
	eh := s.GetHandler(t)
	if eh == nil {
		return
	}
	eh.HandleEvent(t, nil, s, nil)
}

func (s *Events) OnEvent(t EventType, eh EventHandler) {
	if t > EventLAST {
		return
	}
	if s.eventHandlers == nil {
		s.eventHandlers = make([]EventHandler, EventLAST)
	}
	if eh == nil {
		return
	}
	if s.eventHandlers[t] == nil {
		s.eventHandlers[t] = eh
		return
	}
	oeh := s.eventHandlers[t]
	if ec, ok := oeh.(eventChain); ok {
		ec.chain = append(ec.chain, eh)
		return
	}

	s.eventHandlers[t] = eventChain{
		chain: []EventHandler{oeh, eh},
	}
}

func (e *Events) Add(events Events) {
	for i, eh := range events.eventHandlers {
		e.OnEvent(EventType(i), eh)
	}
}

type Events struct {
	eventHandlers []EventHandler
}

// Event includes information about H2.
//
// In the core library, httptrace package provides a subset.
type Event struct {
	H2Mux    *H2Transport
	H2Stream *H2Stream
	Frame    *nio.Buffer
}

type EventHandlerFunc func(evt EventType, t *H2Transport, s *H2Stream, f *nio.Buffer)

func (f EventHandlerFunc) HandleEvent(evt EventType, t *H2Transport, s *H2Stream, d *nio.Buffer) {
	f(evt, t, s, d)
}

type EventHandler interface {
	HandleEvent(evtype EventType, t *H2Transport, s *H2Stream, f *nio.Buffer)
}

func (s *H2Transport) MuxEvent(t EventType) {
	eh := s.GetHandler(t)
	if eh == nil {
		return
	}
	eh.HandleEvent(t, s, nil, nil)
}

func (s *H2Transport) streamEvent(t EventType, h2s *H2Stream) {
	eh := s.GetHandler(t)
	if eh == nil {
		return
	}
	eh.HandleEvent(t, s, h2s, nil)
}
