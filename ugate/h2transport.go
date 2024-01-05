package ugate

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"runtime/debug"
	"time"

	"github.com/costinm/hbone/h2"
	"github.com/costinm/ugate"
	"github.com/costinm/ugate/nio"
	"golang.org/x/exp/slog"
)

// Implement ugate interfaces using the custom h2 implementation

type H2Transport struct {
	hb *ugate.UGate
	// Event handlers will be copied to all created Mux and streams
	// It is possible to add more to each individual mux/stream
	h2.Events

	// Accepted reverse connections
	H2RConn map[*h2.H2Transport]*ugate.EndpointCon

	H2RCallback func(string, *h2.H2Transport)
}

// StartH2ServerConn will initiate an H2 server on the netConn.
func (t *H2Transport) BlockingServerConn(conn net.Conn, mux *http.ServeMux) {
	st, err := h2.NewServerConnection(conn, &h2.ServerConfig{
		//MaxFrameSize:          1 << 22,
		//InitialConnWindowSize: 1 << 26,
		//InitialWindowSize:     1 << 25,
	}, &t.Events)
	if err != nil {
		log.Println("H2 server err", err)
		conn.Close()
		return
	}

	st.StartTime = time.Now()

	st.Handle = func(stream *h2.H2Stream) {
		go t.handleH2Stream(t.hb, st, stream, mux)
	}

	st.TraceCtx = func(ctx context.Context, s string) context.Context {
		//log.Println("Trace", s)
		return ctx
	}

	st.MuxEvent(h2.Event_Connect_Done)

	// blocks - read frames
	st.HandleStreams()
}

// - H2R Server accepts mTLS connection from client, using h2r ALPN
// - Client opens a H2 _server_ handler on the stream, H2R server acts as
// a H2 client.
// - EndpointCon is registered in k8s, using IP of the server holding the connection
// - SNI requests on the H2R server are routed to existing connection
// - if a connection is not found on local server, forward based on endpoint.
// DialH2R connects to an H2R tunnel endpoint, and accepts connections from the tunnel
// Not blocking.
func (t *H2Transport) DialH2R(ctx context.Context, hc *ugate.EndpointCon, addr string) error {
	tc, err := hc.Dial(ctx, addr)
	if err != nil {
		return err
	}
	c := hc.Cluster
	go func() {
		backoff := 50 * time.Millisecond
		for {
			t0 := time.Now()
			h2c, err := h2.NewConnection(ctx,
				h2.H2Config{
					InitialConnWindowSize: c.InitialConnWindowSize,
					InitialWindowSize:     c.InitialWindowSize, // 1 << 25,
					MaxFrameSize:          c.MaxFrameSize,      // 1 << 24,
				})
			if err != nil {
				return
			}
			h2c.Events.OnEvent(h2.Event_ConnClose, h2.EventHandlerFunc(func(evt h2.EventType, t *h2.H2Transport, s *h2.H2Stream, f *nio.Buffer) {
				log.Println("Muxc: Close ", addr)
			}))

			h2c.StartConn(tc)

			//hc.c.ugate.h2Server.ServeConn(tc, &http2.ServeConnOpts{
			//	//Context:    ctx,
			//	Handler:    &HBoneAcceptedConn{conn: hc.TLSConn, UGate: hc.c.UGate},
			//	BaseConfig: &http.Server{},
			//})
			if ugate.Debug {
				log.Println("H2RClient closed, redial", time.Since(t0))
			}
			for {
				if ctx.Err() != nil {
					log.Println("H2RClient canceled")
					return
				}
				tc, err = hc.Dial(ctx, addr)
				if err != nil {
					time.Sleep(backoff)
					backoff = backoff * 2
				}
				backoff = 50 * time.Millisecond
				break
			}
			if ugate.Debug {
				log.Println("H2RClient reconnected", time.Since(t0))
			}
		}
	}()
	return nil
}

// handleH2Stream is called when a H2 stream header has been received.
func (t *H2Transport) handleH2Stream(hb *ugate.UGate, st *h2.H2Transport, stream *h2.H2Stream, mux *http.ServeMux) {
	// TODO: stats
	r := stream.Request

	tunMode := r.Header.Get("x-tun")
	if r.Method == "POST" && tunMode != "" {
		_, p, err := net.SplitHostPort(tunMode)

		stream.Response.Status = "200"
		stream.Response.Header.Add("x-status", "200")
		st.WriteHeader(stream)

		// Create a stream, used for Proxy with caching.
		conf := hb.Auth.GenerateTLSConfigServer(true)
		tls := tls.Server(stream, conf)

		err = nio.HandshakeTimeout(tls, hb.HandsahakeTimeout, nil)
		if err != nil {
			stream.Close()
			log.Println("HBD-MTLS: error inner mTLS ", err)
			return
		}
		log.Println("HBD-MTLS:", tls.ConnectionState())

		// TODO: All Istio checks go here. The TLS handshake doesn't check
		// root cert or anything - this is proof of concept only, to eval
		// perf.

		// TODO: allow user to customize app port, protocol.
		// TODO: if protocol is not matching wire protocol, convert.

		hb.HandleTCPProxy(tls, tls, "localhost:"+p)

		return
	}

	// For connect, the requestURI is the same as host - probably for backward compat
	hbSvc := r.Header.Get("x-service")
	if r.Method == "CONNECT" || r.Method == "POST" && hbSvc != "" {
		// TODO: verify host is endpoint IP ?
		// TODO: support gateway mode

		host := stream.Request.Host

		_, p, _ := net.SplitHostPort(host)
		// TODO: verify host is endpoint IP ?
		// TODO: support gateway mode

		hostPort := "localhost:" + p

		nc, err := net.Dial("tcp", hostPort)
		if err != nil {
			log.Println("Error dialing ", hostPort, err)
			return
		}

		stream.Response.Status = "200"
		stream.Response.Header.Add("x-status", "200")
		st.WriteHeader(stream)

		proxyErr := ugate.Proxy(nc, stream, stream, hostPort)
		if proxyErr != nil {
			slog.Info("HBoneProxyErr", "host", host, "err", proxyErr)
		}

		return
	}

	var hc http.Handler
	hc = &HBoneAcceptedConn{hb: hb, mux: mux, stream: stream}
	// Request Body is a read closer - appropriate for the H2Stream in server mode.
	// The Write method and associated apply to the response writer.
	// H2Stream is both the Request and Response body.
	stream.Request.Body = stream

	hc.ServeHTTP(stream, stream.Request)

	// TODO: make sure all is closed and done
	stream.CloseWrite()
	stream.Close()
}

// findMux - find an EndpointCon that is able to accept new connections.
// Will also dial a connection as needed, and verify the mux can accept a new connection.
// LB should happen here.
// WIP: just one, no retry, only for testing.
// TODO: implement LB properly
func (t *H2Transport) FindMux(ctx context.Context, c *ugate.MeshCluster) (*ugate.EndpointCon, error) {
	if len(c.EndpointCon) == 0 {
		var endp *ugate.Endpoint
		if len(c.Endpoints) > 0 {
			endp = c.Endpoints[0]
		} else {
			endp = &ugate.Endpoint{}
			c.Endpoints = append(c.Endpoints, endp)
		}

		ep := &ugate.EndpointCon{
			Cluster:  c,
			Endpoint: endp,
		}
		c.EndpointCon = append(c.EndpointCon, ep)
	}
	ep := c.EndpointCon[0]
	if cc, ok := ep.RoundTripper.(*h2.H2ClientTransport); ok {
		if !cc.CanTakeNewRequest() {
			ep.RoundTripper = nil
		}
		//if cc.State().StreamsActive > 128 {
		//	// TODO: create new endpoint
		//}
	}

	if ep.RoundTripper == nil {
		// TODO: on failure, try another endpoint
		err := t.dialH2ClientConn(ctx, ep)
		if err != nil {
			return nil, err
		}
	}
	return ep, nil
}

// RoundTripStart a single connection to the host, wrap it with a h2 RoundTripper.
// This is bypassing the http2 client - allowing custom LB and mesh options.
func (t *H2Transport) dialH2ClientConn(ctx context.Context, ep *ugate.EndpointCon) error {
	// TODO: untrusted proxy support

	// TODO: if ep has a URL or addr, use that instead of c.Addr
	addr := ep.Endpoint.HBoneAddress

	// UGate fixed port
	// TODO: check label
	// TODO: select based on CIDR range as well ( pods in a node pool or per node ).
	if addr == "" && ep.Endpoint.Address != "" {
		addr = ep.Endpoint.Address
		h, _, _ := net.SplitHostPort(addr)
		addr = net.JoinHostPort(h, "15008")
	}

	// fallback to cluster address, typical for external
	if addr == "" {
		addr = ep.Cluster.Addr
	}

	c := ep.Cluster
	if c.InitialWindowSize == 0 {
		c.InitialWindowSize = 4194304 // 4M - max should bellow 1 << 24,
	}
	if c.InitialConnWindowSize == 0 {
		c.InitialConnWindowSize = 4 * c.InitialWindowSize
	}
	if c.MaxFrameSize == 0 {
		c.MaxFrameSize = 262144 // 2^18, 256k
	}
	okch := make(chan int, 1)
	hc, err := h2.NewConnection(ctx,
		h2.H2Config{
			//InitialConnWindowSize: c.InitialConnWindowSize,
			//InitialWindowSize:     c.InitialWindowSize, // 1 << 25,
			MaxFrameSize: c.MaxFrameSize, // 1 << 24,
		})
	if err != nil {
		return err
	}

	hc.Events.OnEvent(h2.Event_Settings, h2.EventHandlerFunc(func(evt h2.EventType, t *h2.H2Transport, s *h2.H2Stream, f *nio.Buffer) {
		okch <- 1
		log.Println("Muxc: Preface received ", s)
	}))
	hc.Events.OnEvent(h2.Event_GoAway, h2.EventHandlerFunc(func(evt h2.EventType, t *h2.H2Transport, s *h2.H2Stream, f *nio.Buffer) {
		ep.RoundTripper = nil
	}))
	hc.Events.OnEvent(h2.Event_ConnClose, h2.EventHandlerFunc(func(evt h2.EventType, t *h2.H2Transport, s *h2.H2Stream, f *nio.Buffer) {
		okch <- 0
		log.Println("Muxc: Close ", addr)
		ep.RoundTripper = nil
	}))

	hc.Events.Add(t.Events)
	//hc.Events.Add(ep.Cluster.Events)

	// TODO: on-demand discovery using XDS, report discovery start.
	hc.MuxEvent(h2.Event_Connect_Start)

	hc.StartTime = time.Now()

	_, err = ep.Dial(ctx, addr)
	if err != nil {
		return err
	}

	hc.MuxEvent(h2.Event_Connect_Done)

	alpn := ep.TLSConn.(*tls.Conn).ConnectionState().NegotiatedProtocol
	if alpn != "h2" {
		log.Println("Invalid alpn")
	}

	err = hc.StartConn(ep.TLSConn)
	if err != nil {
		return err
	}

	<-okch

	ep.RoundTripper = hc
	return nil
}

// HBoneAcceptedConn keeps track of one accepted H2 connection.
type HBoneAcceptedConn struct {
	hb     *ugate.UGate
	mux    *http.ServeMux
	stream *h2.H2Stream
}

// ServeHTTP implements the basic TCP-over-H2 and H2 proxy protocol.
// Requests that are not UGate will be handled by the mux in UGate, and
// if they don't match a handler may be forwarded by the reverse HTTP
// proxy.
func (hac *HBoneAcceptedConn) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	t0 := time.Now()
	var proxyErr error
	host := r.Host

	defer func() {
		log.Println(r.Method, r.URL, r.Proto, host, r.Header, r.RemoteAddr,
			time.Since(t0), proxyErr)

		if r := recover(); r != nil {
			fmt.Println("Recovered in hbone", r)

			debug.PrintStack()

			// find out exactly what the error was and set err
			var err error

			switch x := r.(type) {
			case string:
				err = errors.New(x)
			case error:
				err = x
			default:
				err = errors.New("Unknown panic")
			}
			if err != nil {
				fmt.Println("ERRROR: ", err)
			}
		}
	}()

	// Envoy can't set the path when upgrading TCP using POST - all info is in :authority header, just like
	// in CONNECT.

	//
	// Original :authority from client - for services would be svcname.ns.svc:port
	//
	//xfh := r.Header.Get("X-Forwarded-Host")

	// Currently this is only used for 'terminal' connection
	// TODO: support proxy/gateway mode, use host to forward to the proper pod

	rh, pat := hac.mux.Handler(r)
	if pat != "" {
		rh.ServeHTTP(w, r)
		return
	}

	// Make sure xfcc header is removed
	r.Header.Del("x-forwarded-client-cert")
	hac.hb.ReverseProxy.ServeHTTP(w, r)
}
