package hbone

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"strings"
	"time"

	"golang.org/x/net/http2"
)

// Cluster represents a set of endpoints, with a common configuration.
//
//
// Based on K8S Cluster config and semantics, also similar with Envoy Cluster
// or Istio service.
type Cluster struct {
	// ServiceAddr is the TCP address of the cluster - VIP:port or DNS:port
	// This can also be an 'original dst' address for single-endpoint clusters.
	//
	// Created from endpoint. For example Swagger generated configs uses BasePath, Host, Scheme.
	Addr string

	// If not empty, the path to use in requests to the cluster.
	Path string

	// Active connections to endpoints
	Endpoints []*EndpointCon

	// Parent.
	hb *HBone

	// If empty, the cluster is using system certs.
	// Otherwise it's the configured root certs list, in PEM format.
	// May include multiple roots.
	CACert []byte

	// SNI to use when making the request. Defaults to hostname in Addr
	SNI string

	// TODO: UserAgent, DefaultHeaders

	// Optional TokenProvider - not needed if client wraps google oauth
	// or mTLS is used.
	TokenProvider func(context.Context, string) (string, error)

	// Cluster ID - the cluster name in kube config, hub, gke - cluster name in XDS
	// Defaults to Base addr - but it is possible to have multiple clusters for
	// same address ( ex. different users / token providers).
	// Naming:
	// GKE: gke_PROJECT_LOCATION_NAME
	Id string

	// For GKE K8S clusters - extracted from Id.
	// This is the default location for the endpoints.
	Location string

	// From CDS

	// timeout for new network connections to endpoints in cluster
	ConnectTimeout           time.Duration
	TCPKeepAlive             time.Duration
	MaxRequestsPerConnection int

	// Client configured with the root CA of the K8S cluster, used
	// for HTTP/1.1 requests. If set, the cluster is not HBone/H2 but a fallback
	// or non-mesh destination.
	// TODO: customize Dialer to still use mesh LB
	// TODO: attempt ws for tunneling H2
	Client *http.Client

	// TLS config used when dialing, shared
	TLSClientConfig *tls.Config

	// Shared by all endpoints for this cluster
	H2T *http2.Transport

	// If set, will be used to select the next endpoint. Based on lb_policy
	// May dial a new connection.
	//LB func(*Cluster) *EndpointCon
}

// Endpoint represents a connection/association with a node.
type Endpoint struct {
	Labels map[string]string

	LBWeight int
	Priority int

	Address string
}

func (c *Cluster) NewEndpoint(url string) *EndpointCon {
	ep := c.hb.NewEndpointCon(url)
	ep.c = c
	c.Endpoints = append(c.Endpoints, ep)
	return ep
}

// EndpointCon is a client for a specific destination.
type EndpointCon struct {
	hb *HBone

	// URL used to reach the H2 endpoint providing the Hbone service.
	URL string

	// MTLSConfig is a custom config to use for the inner connection - will
	// enable mTLS over H2. If nil, it's regular TCP over H2.
	MTLSConfig *tls.Config

	ExternalMTLSConfig *tls.Config

	// SNI name to use - defaults to service name
	SNI string

	// SNIGate is the endpoint address of a SNI gate. It can be a normal Istio SNI, a SNI to HBone or other protocols,
	// or a H2R gate.
	// If empty, the endpoint will use the URL and HBone protocol directly.
	// If set, the endpoint will use the normal in-cluster Istio protocol.
	SNIGate string

	// H2Gate is the endpoint of a HTTP/2 gateway. Will be used to dial.
	// It is expected to have a spiffee identity, and request client certs -
	// similar with an egress gateway.
	H2Gate string

	tlsCon net.Conn
	rt     http.RoundTripper // *http2.ClientConn //
	c      *Cluster
}

func (hb *HBone) AddCluster(service string, c *Cluster) *Cluster {
	hb.m.Lock()
	if service == "" {
		service = c.Addr
	}
	hb.Clusters[service] = c
	c.hb = hb
	if c.ConnectTimeout == 0 {
		c.ConnectTimeout = hb.ConnectTimeout
	}
	hb.m.Unlock()
	return c
}

// NewEndpointCon creates a client for connecting to a specific service:port
//
// The service is mapped to an endpoint URL, protocol, etc. using a config callback,
// to isolate XDS or discovery dependency.
//
func (hb *HBone) NewEndpointCon(urlOrHost string) *EndpointCon {
	hc := &EndpointCon{hb: hb}

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

// Proxy will Proxy in/out (plain text) to a remote service, using mTLS tunnel over H2 POST.
// used for testing.
func (hb *HBone) Proxy(svc string, hbURL string, stdin io.ReadCloser, stdout io.WriteCloser, innerTLS *tls.Config) error {
	c := hb.NewEndpointCon(hbURL)
	c.MTLSConfig = innerTLS
	return c.Proxy(context.Background(), stdin, stdout)
}

func (hc *EndpointCon) dialTLS(ctx context.Context, addr string) (*tls.Conn, error) {
	d := net.Dialer{} // TODO: customizations

	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}

	// Using the low-level interface, to keep control over TLS.
	conf := hc.hb.Auth.GenerateTLSConfigClient(addr) //MeshTLSConfig.Clone()

	if hc.SNI != "" {
		conf.ServerName = hc.SNI
	} else {
		host, _, _ := net.SplitHostPort(addr)
		conf.ServerName = host
	}

	// TODO: how to keep it alive and detect when it gets closed ?
	// - add Close method to client for explicit close.

	tlsCon := tls.Client(conn, conf)

	err = nio.HandshakeTimeout(tlsCon, hc.hb.HandsahakeTimeout, conn)
	if err != nil {
		return nil, err
	}

	return tlsCon, nil
}

func (c *Cluster) DoRequest(req *http.Request) ([]byte, error) {
	var resp *http.Response
	var err error

	resp, err = c.RoundTrip(req) // Client.Do(req)
	if Debug {
		log.Println(req, resp, err)
	}

	if err != nil {
		return nil, err
	}

	defer resp.Body.Close()
	data, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		log.Println(err)
		return nil, err
	}

	if resp.StatusCode >= 300 || resp.StatusCode < 200 {
		return nil, errors.New(fmt.Sprintf("kconfig: unable to get %v, status code %v",
			req.URL, resp.StatusCode))
	}

	return data, nil
}

// RoundTrip implements a barebone connection and roundtrip using a single endpoint.
func (c *Cluster) RoundTrip(req *http.Request) (*http.Response, error) {
	var resp *http.Response
	var err error

	if c.TokenProvider != nil {
		t, err := c.TokenProvider(req.Context(), "https://"+c.Addr)
		if err != nil {
			return nil, err
		}
		req.Header.Add("authorization", "Bearer "+t)
	}

	// Find a channel - LB would go here if multiple addresses and sockets
	//

	rg, err := c.DialRoundTripper(req.Context())
	if err != nil {
		return nil, err
	}
	resp, err = rg.RoundTrip(req)
	if Debug {
		log.Println(req, resp, err)
	}

	return resp, err
}

// Dial a single connection to the host, wrap it with a h2 RoundTripper.
// This is bypassing the http2 client - allowing custom LB and mesh options.
func (c *Cluster) DialRoundTripper(ctx context.Context) (*http2.ClientConn, error) {
	d := &net.Dialer{
		Timeout:   c.ConnectTimeout,
		KeepAlive: c.TCPKeepAlive,
	}

	// TODO: skip TLS if trusted network
	if c.TLSClientConfig == nil {
		sni := c.SNI
		if sni == "" {
			sni, _, _ = net.SplitHostPort(c.Addr)
		}

		if c.CACert != nil && len(c.CACert) > 0 {
			roots := x509.NewCertPool()
			if !roots.AppendCertsFromPEM(c.CACert) {
				log.Println("Failed to decode PEM")
			}
			c.TLSClientConfig = &tls.Config{
				RootCAs:    roots,
				ServerName: sni, // required
				NextProtos: []string{"istio", "h2"},
			}
		} else {
			c.TLSClientConfig = &tls.Config{
				ServerName: sni, // required
				NextProtos: []string{"istio", "h2"},
			}
		}
	}

	// First do TCP
	tcpC, err := d.DialContext(ctx, "tcp", c.Addr)
	if err != nil {
		return nil, err
	}

	// Upgrade to TLS - NPN included
	cc := tls.Client(tcpC, c.TLSClientConfig)
	err = cc.HandshakeContext(ctx)
	if err != nil {
		return nil, err
	}

	if c.H2T == nil {
		c.H2T = &http2.Transport{
			ReadIdleTimeout:            10000 * time.Second,
			StrictMaxConcurrentStreams: false,
			AllowHTTP:                  true,
		}
	}

	// Multiplex the connection
	rt, err := c.H2T.NewClientConn(cc)
	return rt, err
}

// Send the content of in and out to the remote endpoint as a Hbone stream.
func (hc *EndpointCon) Proxy(ctx context.Context, stdin io.Reader, stdout io.WriteCloser) error {
	if hc.SNIGate != "" {
		return SNIProxy(ctx, hc, stdin, stdout)
	}

	t0 := time.Now()
	// It is usually possible to pass stdin directly to NewRequest.
	// Using a pipe allows getting stats.
	i, o := io.Pipe()
	defer stdout.Close()

	r, err := http.NewRequest("POST", hc.URL, i)
	if err != nil {
		return err
	}

	var rt = hc.rt

	if hc.hb.TokenCallback != nil {
		h := r.URL.Host
		if strings.Contains(h, ":") && h[0] != '[' {
			hn, _, _ := net.SplitHostPort(h)
			h = hn
		}
		t, err := hc.hb.TokenCallback(ctx, "https://"+h)
		if err != nil {
			log.Println("Failed to get token, attempt unauthenticated", err)
		} else {
			r.Header.Set("Authorization", "Bearer "+t)
		}
	}

	if hc.rt == nil {
		/* Alternative, using http.Client.
		  	ug = &http.Client{
				Transport: &http2.Transport{
					// So http2.Transport doesn't complain the URL scheme isn't 'https'
					AllowHTTP: true,
					// Pretend we are dialing a TLS endpoint.
					// Note, we ignore the passed tls.Config
					DialTLS: func(network, addr string, cfg *tls.Config) (net.ReaderCopier, error) {
						return net.Dial(network, addr)
					},
				},
			}
		*/
		h := r.URL.Host
		if hc.H2Gate != "" {
			h = hc.H2Gate
		}
		if Debug {
			rd, _ := httputil.DumpRequest(r, false)
			log.Println("HB req: ", h, string(rd))
		}
		host, port, _ := net.SplitHostPort(h)

		// Expect system certificates.
		if r.URL.Scheme == "http" {
			d := &net.Dialer{}

			dialHost := r.URL.Host
			if port == "" {
				dialHost = net.JoinHostPort(dialHost, "80")
			}
			nConn, err := d.DialContext(ctx, "tcp", dialHost)
			if err != nil {
				return err
			}

			hc.tlsCon = nConn

		} else if port == "443" || port == "" {
			d := tls.Dialer{
				Config: &tls.Config{
					NextProtos: []string{"h2"},
				},
				NetDialer: &net.Dialer{},
			}
			h := r.URL.Host
			if port == "" {
				h = net.JoinHostPort(h, "443")
			}
			if hc.H2Gate != "" {
				h = hc.H2Gate
			}
			nConn, err := d.DialContext(ctx, "tcp", h)
			if err != nil {
				return err
			}
			tlsCon := nConn.(*tls.Conn)

			tlsCon.VerifyHostname(host)

			if err != nil {
				return err
			}
			if tlsCon.ConnectionState().NegotiatedProtocol != "h2" {
				log.Println("Failed to negotiate h2", tlsCon.ConnectionState().NegotiatedProtocol)
				return errors.New("invalid ALPN protocol")
			}
			hc.tlsCon = tlsCon
		} else {

			tlsCon, err := hc.dialTLS(ctx, h)
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

		if hc.hb.Transport != nil {
			rt = hc.hb.Transport(rt)
		}
		hc.rt = rt
	}

	// This might be useful to make sure auth works - but it doesn't seem to help with the deadlock/canceling send.
	//r.Header.Add("Expect", "100-continue")
	res, err := rt.RoundTrip(r)
	if err != nil {
		return err
	}

	t1 := time.Now()
	ch := make(chan int)
	var s1, s2 *nio.ReaderCopier

	if hc.MTLSConfig == nil {
		s1 = &nio.ReaderCopier{
			ID:  "client-o",
			Out: o,
			In:  stdin,
		}
		go s1.Copy(ch, true)

		s2 = &nio.ReaderCopier{
			ID:  "client-i",
			Out: stdout,
			In:  res.Body,
		}
		s2.Copy(nil, true)
	} else {
		// Do the mTLS handshake for the tunneled connection
		tlsTun := tls.Client(&nio.HTTPConn{Conn: hc.tlsCon, R: res.Body, W: o}, hc.MTLSConfig)
		err = nio.HandshakeTimeout(tlsTun, hc.hb.HandsahakeTimeout, nil)
		if err != nil {
			return err
		}
		log.Println("client-rt tun handshake", tlsTun.ConnectionState())
		s1 = &nio.ReaderCopier{
			Out: tlsTun,
			In:  stdin,
		}
		go s1.Copy(ch, true)

		s2 = &nio.ReaderCopier{
			Out: stdout,
			In:  tlsTun,
		}
		s2.Copy(nil, true)
	}

	<-ch

	log.Println("hbc-done", "status", res.Status, "conTime", t1.Sub(t0), "dur", time.Since(t1), "err", s2.Err, s1.Err, s2.InError, s1.InError)
	return s2.Err
}
