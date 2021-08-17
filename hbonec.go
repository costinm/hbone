package hbone

import (
	"context"
	"crypto/tls"
	"io"
	"log"
	"net"
	"net/http"
	"strings"

	"golang.org/x/net/http2"
)

type HBoneClient struct {
	ServiceAddr string

	Endpoints []*Endpoint
	hb        *HBone
}

func (c HBoneClient) NewEndpoint(url string) *Endpoint {
	ep := c.hb.NewEndpoint(url)
	c.Endpoints = append(c.Endpoints, ep)
	return ep
}

// Endpoint is a client for a specific destination.
type Endpoint struct {
	hb *HBone

	// Service addr - using the service name.
	ServiceAddr string

	// URL used to reach the H2 endpoint providing the proxy.
	URL string
	// use mTLS over H2 with this config. If nil, use TCP over H2.
	MTLSConfig *tls.Config

	// SNI name to use - defaults to service name
	SNI     string

	// SNIGate is the endpoint address of a SNI gate. It can be a normal Istio SNI, a SNI to HBone or other protocols,
	// or a H2R gate.
	// If empty, the endpoint will use the URL and HBone protocol directly.
	// If set, the endpoint will use the nomal in-cluster Istio protocol.
	SNIGate string

	// TODO: multiple per endpoint
	tlsCon *tls.Conn
	rt     *http2.ClientConn // http.RoundTripper
}
func (hb *HBone) NewClient(service string) *HBoneClient {
	return &HBoneClient{hb: hb, ServiceAddr: service}
}

// NewEndpoint creates a client for connecting to a specific service:port
//
// The service is mapped to an endpoint URL, protocol, etc. using a config callback,
// to isolate XDS or discovery dependency.
//
func (hb *HBone) NewEndpoint(urlOrHost string) *Endpoint {
	hc := &Endpoint{hb: hb}

	if !strings.HasPrefix(urlOrHost, "https://") {

		// TODO: for host and port - assume mTLS, using system certs for the 'external' tunnel
		// TODO: resolver call, to map to endpoint (including SNI routers or gateway)
		h, p, err := net.SplitHostPort(urlOrHost)
		if err == nil {
			urlOrHost = "https://" + h + "/_hbone/" + p
		}
		hc.URL = urlOrHost
	} else {
		hc.URL = urlOrHost
	}

	return hc
}

// Proxy will proxy in/out (plain text) to a remote service, using mTLS tunnel over H2 POST.
// used for testing.
func (hb *HBone) Proxy(svc string, hbURL string, stdin io.ReadCloser, stdout io.WriteCloser, innerTLS *tls.Config) error {
	c := hb.NewEndpoint(hbURL)
	c.MTLSConfig = innerTLS
	return c.Proxy(context.Background(), stdin, stdout)
}


func (hc *Endpoint) dialTLS(ctx context.Context, addr string) (*tls.Conn, error) {
	d := net.Dialer{} // TODO: customizations

	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}

	// Using the low-level interface, to keep control over TLS.
	conf := hc.hb.Auth.TLSConfig.Clone()

	if hc.SNI != "" {
		conf.ServerName = hc.SNI
	} else {
		host, _, _ := net.SplitHostPort(addr)
		conf.ServerName = host
	}

	// TODO: how to keep it alive and detect when it gets closed ?
	// - add Close method to client for explicit close.

	tlsCon := tls.Client(conn, conf)

	err = HandshakeTimeout(tlsCon, hc.hb.HandsahakeTimeout, conn)
	if err != nil {
		return nil, err
	}

	return tlsCon, nil
}

func (hc *Endpoint) Proxy(ctx context.Context, stdin io.Reader, stdout io.WriteCloser) error {
	if hc.SNIGate != "" {
		return hc.sniProxy(ctx, stdin, stdout)
	}
	// It is usually possible to pass stdin directly to NewRequest.
	// Using a pipe allows getting stats.
	i, o := io.Pipe()
	defer stdout.Close()

	r, err := http.NewRequest("POST", hc.URL, i)
	if err != nil {
		return err
	}

	var rt = hc.rt

	if hc.rt == nil {
		/* Alternative, using http.Client.
		  	ug = &http.Client{
				Transport: &http2.Transport{
					// So http2.Transport doesn't complain the URL scheme isn't 'https'
					AllowHTTP: true,
					// Pretend we are dialing a TLS endpoint.
					// Note, we ignore the passed tls.Config
					DialTLS: func(network, addr string, cfg *tls.Config) (net.Conn, error) {
						return net.Dial(network, addr)
					},
				},
			}
		*/
		host, port, _ := net.SplitHostPort(r.URL.Host)

		// Expect system certificates.
		if port == "443" || port == "" {
			d := tls.Dialer{
				Config:    nil,
				NetDialer: &net.Dialer{},
			}
			nConn, err := d.DialContext(ctx, "tcp", r.URL.Host)
			if err != nil {
				return err
			}
			tlsCon := nConn.(*tls.Conn)

			tlsCon.VerifyHostname(host)

			if err != nil {
				return err
			}
			hc.tlsCon = tlsCon
		} else {
			tlsCon, err := hc.dialTLS(ctx, r.URL.Host)
			if err != nil {
				return err
			}
			// TODO: how to keep it alive and detect when it gets closed ?
			// - add Close method to client for explicit close.
			defer tlsCon.Close()

			hc.tlsCon = tlsCon
			// TODO: check the SANs have been verified by TLSConfig call.
		}

		rt, err = hc.hb.h2t.NewClientConn(hc.tlsCon)
		if err != nil {
			return err
		}

		hc.rt = rt
	}

	//rt = hb.HTTPClientSystem.Transport
	res, err := rt.RoundTrip(r)
	if err != nil {
		return err
	}

	log.Println("client-rt", res.Status, res.Header)
	ch := make(chan int)
	var s1, s2 Stream

	if hc.MTLSConfig == nil {
		s1 = Stream{
			ID:  "client-o",
			Dst: o,
			Src: stdin,
		}
		go s1.CopyBuffered(ch, true)

		s2 = Stream{
			ID:  "client-i",
			Dst: stdout,
			Src: res.Body,
		}
		s2.CopyBuffered(nil, true)
	} else {
		// Do the mTLS handshake for the tunneled connection
		tlsTun := tls.Client(&HTTPConn{acceptedConn: hc.tlsCon, r: res.Body, w: o}, hc.MTLSConfig)
		err = HandshakeTimeout(tlsTun, hc.hb.HandsahakeTimeout, nil)
		if err != nil {
			return err
		}
		log.Println("client-rt tun handshake", tlsTun.ConnectionState())
		s1 = Stream{
			Dst: tlsTun,
			Src: stdin,
		}
		go s1.CopyBuffered(ch, true)

		s2 = Stream{
			Dst: stdout,
			Src: tlsTun,
		}
		s2.CopyBuffered(nil, true)
	}

	<-ch

	return s2.Err
}
