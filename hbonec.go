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
	"strconv"
	"sync/atomic"
	"time"

	"github.com/costinm/hbone/ext/transport"
	"github.com/costinm/hbone/nio"
	"github.com/costinm/hbone/nio/syscall"

	//"golang.org/x/net/http2"
	"github.com/costinm/hbone/ext/http2"
)

// Structs are yaml-friendly:
// lowercase used by yaml
//

// Cluster represents a set of endpoints, with a common configuration.
// Can be a K8S Service with VIP and DNS name, an external service, etc.
//
// Similar with Envoy Cluster or K8S service, can also represent single
// endpoint with multiple paths/IPs.
type Cluster struct {
	// Cluster ID - the cluster name in kube config, hub, gke - cluster name in XDS
	// Defaults to Base addr - but it is possible to have multiple clusters for
	// same address ( ex. different users / token providers).
	// Naming:
	// GKE: gke_PROJECT_LOCATION_NAME
	Id string

	// ServiceAddr is the TCP address of the cluster - VIP:port or DNS:port
	// For K8S service it should be in the form serviceName.namespace.svc:svcPort
	//
	// This can also be an 'original dst' address for single-endpoint clusters.
	Addr string

	// VIP for the cluster. In case of workload, the IP of the workload.
	// May be empty - Addr will be used.
	VIP string

	// SNI to use when making the request. Defaults to hostname in Addr
	SNI string

	// Set if the destination cluster is a HTTP service with a URL path.
	// This is only used when the cluster is used for HTTP, as a prefix.
	Path string

	// Active connections to endpoints, each is a multiplexed H2 connection.
	EndpointCon []*EndpointCon

	// Endpoint addresses associated with the cluster.
	// If empty, the Cluster Addr will be used directly.
	Endpoints []*Endpoint

	// Parent.
	hb *HBone

	// If empty, the cluster is using system certs or SPIFFE
	// Otherwise, it's the configured root certs list, in PEM format.
	// May include multiple concatenated roots.
	CACert string

	// cached value
	roots *x509.CertPool

	// TODO: UserAgent, DefaultHeaders

	// Optional TokenProvider - not needed if client wraps google oauth
	// or mTLS is used.
	TokenProvider func(context.Context, string) (string, error)

	Token string

	// For GKE K8S clusters - extracted from Id.
	// This is the default location for the endpoints.
	Location string

	// From CDS

	// timeout for new network connections to endpoints in cluster
	ConnectTimeout           time.Duration
	TCPKeepAlive             time.Duration
	TCPUserTimeout           time.Duration
	MaxRequestsPerConnection int

	// Client configured with the root CA of the K8S cluster, used
	// for HTTP/1.1 requests. If set, the cluster is not HBone/H2 but a fallback
	// or non-mesh destination.
	// TODO: customize Dialer to still use mesh LB
	// TODO: attempt ws for tunneling H2
	Client *http.Client

	// TLS config used when dialing using workload identity, shared
	TLSClientConfig *tls.Config

	// Shared by all endpoints for this cluster
	H2T *http2.Transport

	// If set, will be used to select the next endpoint. Based on lb_policy
	// May dial a new connection.
	//LB func(*Cluster) *EndpointCon
}

// Cluster and EndpointCon implements the Mux interface.
type Mux interface {
	Dial(ctx context.Context) (net.Conn, error)
}

// Endpoint represents a connection/association with a cluster.
type Endpoint struct {
	Labels map[string]string

	LBWeight int
	Priority int

	// Address is the PodIP:port. Can be a hostname:port for external endpoints.
	// Will be used when dialing direct.
	// If Dialing via a proxy (east-west, PEP, SNI, etc) - the proxy address will
	// be dialed, but the endpoint Address will be included as a header.
	Address string

	// HBoneAddress is hostOrIP:port for hbone. If not set, default port 15008 will be used.
	// The server is expected to support HTTP/2 and mTLS for CONNECT, or HTTP/2 and JWT for POST.
	// It is expected to have a spiffee identity, and request client certs in CONNECT case.
	HBoneAddress string

	// SNIGate is the endpoint address of a SNI gate. It can be a normal Istio SNI, a SNI to HBone or other protocols,
	// or a H2R gate.
	// If empty, the endpoint will use the URL and HBone protocol directly.
	// If set, the endpoint will use the normal in-cluster Istio protocol.
	SNIGate string

	// SNI name to use - defaults to service name
	SNI string

	Secure bool
}

// EndpointCon is a multiplexed H2 client for a specific destination instance.
// Should not be used directly.
type EndpointCon struct {
	c        *Cluster
	Endpoint *Endpoint

	rt     http.RoundTripper // *http2.ClientConn or custom
	tlsCon net.Conn
	// The stream connection - may be a real TCP or not
	streamCon net.Conn
}

func (c *Cluster) UpdateEndpoints(ep []*Endpoint) {
	c.hb.m.Lock()
	// TODO: preserve unmodified endpoints connections, by IP, refresh pending
	c.Endpoints = ep
	c.hb.m.Unlock()
}

// AddService will add a cluster to be used for Dial and RoundTrip.
// The 'Addr' field can be a host:port or IP:port.
// If id is set, it can be host:port or hostname - will be added as a destination.
// The service can be IP:port or URLs
func (hb *HBone) AddService(c *Cluster, service ...*Endpoint) *Cluster {
	hb.m.Lock()
	hb.Clusters[c.Addr] = c
	if c.Id != "" {
		hb.Clusters[c.Id] = c
	}
	c.hb = hb
	if c.ConnectTimeout == 0 {
		c.ConnectTimeout = hb.ConnectTimeout
	}
	hb.m.Unlock()
	for _, s := range service {
		c.Endpoints = append(c.Endpoints, s)
	}
	return c
}

func (hb *HBone) Dial(n, a string) (net.Conn, error) {
	return hb.DialContext(context.Background(), n, a)
}

func (hb *HBone) GetCluster(addr string) *Cluster {
	hb.m.RLock()
	c := hb.Clusters[addr]
	hb.m.RUnlock()
	return c
}

func (hb *HBone) Cluster(ctx context.Context, addr string) (*Cluster, error) {
	// TODO: extract cluster from addr, allow URL with params to indicate how to connect.
	//host := ""
	//if strings.Contains(dest, "//") {
	//	u, _ := url.Parse(dest)
	//
	//	host, _, _ = net.SplitHostPort(u.Host)
	//} else {
	//	host, _, _ = net.SplitHostPort(dest)
	//}
	//if strings.HasSuffix(host, ".svc") {
	//	hc.H2Gate = hg + ":15008" // hbone/mtls
	//	hc.ExternalMTLSConfig = auth.GenerateTLSConfigServer()
	//}
	//// Initialization done - starting the proxy either on a listener or stdin.

	// 1. Find the cluster for the address. If not found, create one with the defaults or use on-demand
	// if XDS server is configured
	hb.m.RLock()
	c, ok := hb.Clusters[addr]
	hb.m.RUnlock()
	// TODO: use discovery to find info about service addr, populate from XDS on-demand or DNS
	if !ok {
		// TODO: on-demand, DNS lookups, etc
		c = &Cluster{Addr: addr, hb: hb}
		hb.AddService(c)
	}
	return c, nil
}

// dialTLS creates an outer layer TLS connection with the H2 CONNECT (or POST) address.
// Will initiate a TCP connection first - possibly using the SNI gate, and do the handshake.
func (hc *EndpointCon) dialTLS(ctx context.Context, addr string) (net.Conn, error) {
	c := hc.c
	d := &net.Dialer{
		Timeout:   c.ConnectTimeout,
		KeepAlive: c.TCPKeepAlive,
	}

	if hc.Endpoint.SNIGate != "" {
		addr = hc.Endpoint.SNIGate
		// TODO: mangle the address of the hbone port and/or endpoint
	}

	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}

	// If net connection is cut, by default the socket may linger for up to 20 min without detecting this.
	// Extracted from gRPC - needs to apply at TCP socket level
	if c.TCPUserTimeout != 0 {
		syscall.SetTCPUserTimeout(conn, c.TCPUserTimeout)
	}

	hc.streamCon = conn

	// TODO: skip TLS if trusted network
	if hc.c.hb.SecureConn(hc.Endpoint) {
		hc.tlsCon = conn
		return conn, nil
	}

	if c.TLSClientConfig == nil {
		sni := c.SNI
		if hc.Endpoint.SNIGate != "" {
			// Mangle the address - using legacy Istio format
			h, p, _ := net.SplitHostPort(c.Addr)
			sni = fmt.Sprintf("outbound_.%s._.%s", p, h)
		}
		if sni == "" {
			sni, _, _ = net.SplitHostPort(c.Addr)
		}
		c.TLSClientConfig = c.hb.Auth.GenerateTLSConfigClientRoots(sni, c.trustRoots())
	}
	conf := c.TLSClientConfig

	//conn1 := &debugCon{conn}

	tlsCon := tls.Client(conn, conf)

	err = nio.HandshakeTimeout(tlsCon, hc.c.hb.HandsahakeTimeout, conn)
	if err != nil {
		return nil, err
	}

	// tlsCon.VerifyHostname(c.SNI) is handled in the verifier

	hc.tlsCon = tlsCon
	return tlsCon, nil
}

type debugTLSCon struct {
	*tls.Conn
}

func (dc *debugTLSCon) Read(b []byte) (int, error) {
	n, err := dc.Conn.Read(b)
	log.Println("YYY TCPRead() ", n, err)
	return n, err
}

type debugCon struct {
	net.Conn
}

func (dc *debugCon) Read(b []byte) (int, error) {
	n, err := dc.Conn.Read(b)
	log.Println("YYY TLSRead() ", n, err)
	return n, err
}

type debugConW struct {
	net.Conn
}

func (dc *debugConW) Write(b []byte) (int, error) {
	n, err := dc.Conn.Write(b)
	log.Println("ZZZ TCPWrite() ", n, err)
	if err != nil {
		log.Println("ZZZ err TCPWrite() ", n, err)
	}
	return n, err
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

// Return the custom cert pool, if cluster config specifies a list of trusted
// roots, or nil if default trust is used.
func (c *Cluster) trustRoots() *x509.CertPool {
	// Custom server verification.
	if c.roots != nil {
		return c.roots
	}
	var roots *x509.CertPool
	if c.CACert != "" {
		roots = x509.NewCertPool()
		if !roots.AppendCertsFromPEM([]byte(c.CACert)) {
			log.Println("Failed to decode PEM")
		}
		c.roots = roots
	}
	return roots
}

// DialContext dials a destination address (host:port) using TCP-over-H2 protocol.
// This can be used in applications as a TCP Dial replacement.
//
// The result is an instance of nio.HTTPConn (diret) or a tls.Conn (for untrusted proxy mode).
// In the later case, NetConn() returns the nio.HTTPConn to the proxy.
//
// The HTTPConn represents the HBone connection to the peer or to a proxy.
func (hb *HBone) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	log.Println("Dial ", addr)
	c := hb.GetCluster(addr)
	if c == nil {
		// TODO: custom dialer
		// TODO: if egress gateway is set, use it ( redirect all unknown to egress )
		// TODO: CIDR range of Endpoints, Nodes, VIPs to use hbone
		// TODO: if port, use SNI or match clusters
		return net.Dial(network, addr)
	}

	c, err := hb.Cluster(ctx, addr)
	if err != nil {
		return nil, err
	}

	return c.Dial(ctx, nil)
}

// DialRequest connects to the host defined in req.Host or req.URL.Host, creates
// a H2 mux and opens a stream, returing a net.Conn object
//
// Dialing sends and headers in req. If req.Body is set, it will be forwarded to
// remote, as expected. If it is not set, net.Conn Write will be sent to remote,
// as in a typical dialed connection.
//
// The resulting connection implements HBoneConn interface, which provides access
// to response headers. Response headers are not available immediately, unlike
// RoundTrip() this does not wait for response headers - just connect and starts
// the stream. Response headers can be waited, or will be available after first
// Read() in the conn.
func (hb *HBone) DialRequest(ctx context.Context, req *http.Request) (net.Conn, error) {
	hostPort := req.Host
	if hostPort == "" {
		hostPort = req.URL.Host
	}
	c, err := hb.Cluster(ctx, hostPort)
	if err != nil {
		return nil, err
	}

	return c.Dial(ctx, req)
}

// Similar with HBone dial, if you already have the Cluster (by hostname).
func (c *Cluster) Dial(ctx context.Context, r *http.Request) (net.Conn, error) {
	epc, nc, err := c.dial(ctx, r)
	if err != nil {
		return nc, err
	}

	// We now have a stream using (m)TLS + (JWT) to a proxy server.
	// This is generally sufficient if the proxy is trusted, or for direct pod-to-pod communication.
	// If we the proxy (HBoneAddress) is not trusted, we'll add e2e mTLS

	// The 'standard' http_proxy is treated as untrusted. dial creates an mTLS connection to the proxy only.
	// We need to do another round of mTLS to connect to the end host.
	// This is compatible with normal CONNECT and http_proxy - goes to the original destination port, not to the hbone
	// port on the target.
	if epc.Endpoint.Labels["http_proxy"] != "" {
		// Do the mTLS handshake for the tunneled connection
		// SNI is based on the service name - or the SNI override.
		sni := c.SNI
		if sni == "" {
			sni, _, _ = net.SplitHostPort(c.Addr)
		}
		tlsClientConfig := c.hb.Auth.GenerateTLSConfigClientRoots(sni, c.trustRoots())

		tlsTun := tls.Client(nc, //&nio.HTTPConn{Conn: epc.tlsCon, R: nc, W: nc, Cluster: c},
			tlsClientConfig)
		err = nio.HandshakeTimeout(tlsTun, c.hb.HandsahakeTimeout, nil)
		if err != nil {
			return nil, err
		}
		log.Println("client-rt tun handshake", tlsTun.ConnectionState())
		return tlsTun, err
	}

	return nc, err
}

// TODO(costin): use the hostname, get IP override from x-original-dst header or cookie.
func (c *Cluster) dial(ctx context.Context, req *http.Request) (*EndpointCon, net.Conn, error) {
	epc, err := c.findMux(ctx)
	if err != nil {
		return nil, nil, err
	}

	// Hacky dial-using-proxy. Should be handled by rt(). The Dial method takes the result and does
	// an extra mTLS handshake.
	//
	// For http requests calling Roundtrip, the same should happen.
	if epc.Endpoint.Labels["http_proxy"] != "" {
		// TODO: only POST mode supported right now, address not from label.

		// Tunnel mode, untrusted proxy authentication.
		i, o := io.Pipe()
		req, _ := http.NewRequestWithContext(ctx, "POST", "https://"+epc.Endpoint.HBoneAddress, i)
		if c.TokenProvider != nil {
			t, err := c.TokenProvider(req.Context(), "https://"+epc.Endpoint.HBoneAddress)
			if err != nil {
				return nil, nil, err
			}
			req.Header.Add("authorization", "Bearer "+t)
		}
		if c.Token != "" {
			req.Header.Add("authorization", c.Token)
		}

		// See:
		//https://www.envoyproxy.io/docs/envoy/latest/configuration/http/http_conn_man/headers#config-http-conn-man-headers-downstream-service-cluster

		// user-agent - based on service-cluster
		// server - defaults to 'envoy'
		// x-client-trace-id
		// x-envoy-downstream-client-cluster
		// x-envoy-downstream-client-node
		// x-envoy-external-address
		// x-envoy-force-trace
		// x-envoy-internal
		// x-envoy-original-dst-host - override original destination, if use_http_header option set
		// x-forwarded-client-cert
		// x-forwarded-for
		// x-forwarded-host - original host in authority, if host is rewritten/replaced
		// x-forwarded-proto
		// x-request-id
		// x-ot-span-context
		// x-b3-* - for zipkin
		//
		// Envoy configs can set headers based on meta:
		// DOWNSTREAM_REMOTE_ADDRESS - that's client address, possibly from x-f-f
		// DOWNSTREAM_REMOTE_ADDRESS_WITHOUT_PORT
		// DOWNSTREAM_REMOTE_PORT
		// DOWNSTREAM_DIRECT_REMOTE_ADDRESS = IP address, direct, also _PORT and WITHOUT_PORT
		// DOWNSTREAM_LOCAL_ADDRESS = local IP for direct, or original dest for REDIRECT. Original dst/port
		// DOWNSTREAM_LOCAL_URI_SAN - SAN in the local certificate
		// DOWNSTREAM_PEER_URI_SAN
		// same for SUBJECT, ISSUER, TLS_SESSION_ID, TLS_CIPHER, TLS_VERSION, PEER_FINGERPRINT_256
		// DOWNSTREAM_PEER_CERT
		// ... V_START, V_END
		// REQUESTED_SERVER_NAME - SNI
		// UPSTREAM_METADATA("namespace", "key")
		// DYNAMIC_METADATA
		//
		// UPSTREAM_REMOTE_ADDRESS - can't be added to requests (not known yet)
		// PER_REQUEST_STATE ???
		// REQ(header-name)
		// START_TIME
		// RESPONSE_FLAGS
		// RESPONSE_CODE_DETAILS
		// VIRTUAL_CLUSTER_NAME

		req.Header.Add("X-Service", c.Addr)
		req.Header.Add("X-tun", epc.Endpoint.Address)

		// https://www.w3.org/TR/trace-context/
		// https://w3c.github.io/baggage/
		// version:0
		// traceID: 16B hex
		// parentID: 8B hex - span id, id of the request from caller
		// flags: 01 = sampled
		//req.Header.Add("traceparent", "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-00")
		//tracestate: congo=ucfJifl5GOE,rojo=00f067aa0ba902b7

		res, err := epc.rt.RoundTrip(req)
		if err != nil {
			epc.rt = nil
			return nil, nil, err
		}

		return epc, &HTTPConn{R: res.Body, W: o, Conn: epc.tlsCon, Req: req, Res: res,
			Cluster: c}, err
	}

	if useGrpcH2 {
		if req == nil {
			req, _ = http.NewRequestWithContext(ctx, "CONNECT", "https://"+epc.Endpoint.Address, nil)
		}

		req.Header.Add("X-Service", c.Addr)

		res, tlsc, err := c.rt(epc, req)
		if err != nil {
			return nil, nil, err
		}

		nc := res.Body.(net.Conn)
		// TODO: return nc directly instead of HTTPConn
		return epc, &HTTPConn{
			R:       res.Body,
			W:       nc,
			Conn:    tlsc.tlsCon,
			Req:     req,
			Res:     res,
			Cluster: c}, err
	}
	// It is usually possible to pass stdin directly to NewRequest.
	// Using a pipe allows getting stats.
	var out io.Writer
	if req == nil {
		i, o := io.Pipe()
		out = o
		req, _ = http.NewRequestWithContext(ctx, "CONNECT", "https://"+epc.Endpoint.Address, i)
	} else {
		if req.Body == nil {
			i, o := io.Pipe()
			out = o
			req.Body = i
		}
	}

	req.Header.Add("X-Service", c.Addr)

	res, tlsc, err := c.rt(epc, req)
	if err != nil {
		return nil, nil, err
	}

	return epc, &HTTPConn{R: res.Body, W: out, Conn: tlsc.tlsCon,
		Req: req, Res: res, Cluster: c}, err
}

func (c *Cluster) RoundTrip(req *http.Request) (*http.Response, error) {
	epc, err := c.findMux(req.Context())
	if err != nil {
		return nil, err
	}
	r, _, err := c.rt(epc, req)
	// TODO: grpc rt doesn't wait for headers !
	return r, err
}

func (c *Cluster) rt(epc *EndpointCon, req *http.Request) (*http.Response, *EndpointCon, error) {
	var resp *http.Response
	var rterr, err error

	if c.TokenProvider != nil {
		t, err := c.TokenProvider(req.Context(), "https://"+c.Addr)
		if err != nil {
			return nil, nil, err
		}
		req.Header.Add("authorization", "Bearer "+t)
	}

	for i := 0; i < 3; i++ {

		// Find a channel - LB would go here if multiple addresses and sockets
		if epc == nil || epc.rt == nil {
			epc, err = c.findMux(req.Context())
			if err != nil {
				return nil, nil, err
			}
		}

		if useGrpcH2 {

		} else {

		}

		// IMPORTANT: some servers will not return the headers until the first byte of the response is sent, which
		// may not happen until request bytes have been sent.
		// For CONNECT, we will require that the server is flushing the headers as soon as the request is received,
		// to emulate the connection semantics - at least initially.
		// For POST and other methods - we can't assume this. That means read() on the conn will need to be blocked
		// and wait for the Header frame to be received, and any metadata too.
		resp, rterr = epc.rt.RoundTrip(req)
		if Debug {
			log.Println(req, resp, rterr)
		}

		if rterr != nil {
			// retry on different mux
			epc.rt = nil
			continue
		}

		return resp, epc, err
	}
	return nil, nil, rterr
}

// findMux - find an EndpointCon that is able to accept new connections.
// Will also dial a connection as needed, and verify the mux can accept a new connection.
// LB should happen here.
// WIP: just one, no retry, only for testing.
// TODO: implement LB properly
func (c *Cluster) findMux(ctx context.Context) (*EndpointCon, error) {
	if len(c.EndpointCon) == 0 {
		var endp *Endpoint
		if len(c.Endpoints) > 0 {
			endp = c.Endpoints[0]
		} else {
			endp = &Endpoint{}
			c.Endpoints = append(c.Endpoints, endp)
		}

		ep := &EndpointCon{
			c:        c,
			Endpoint: endp,
		}
		c.EndpointCon = append(c.EndpointCon, ep)
	}
	ep := c.EndpointCon[0]
	if cc, ok := ep.rt.(*http2.ClientConn); ok {
		if !cc.CanTakeNewRequest() {
			ep.rt = nil
		}
		if cc.State().StreamsActive > 128 {
			// TODO: create new endpoint
		}
	}
	if ep.rt == nil {
		// TODO: on failure, try another endpoint
		err := ep.dialH2ClientConn(ctx)
		if err != nil {
			return nil, err
		}
	}
	return ep, nil
}

// Dial a single connection to the host, wrap it with a h2 RoundTripper.
// This is bypassing the http2 client - allowing custom LB and mesh options.
func (ep *EndpointCon) dialH2ClientConn(ctx context.Context) error {
	// TODO: untrusted proxy support

	// TODO: if ep has a URL or addr, use that instead of c.Addr
	addr := ep.Endpoint.HBoneAddress

	// HBone fixed port
	// TODO: check label
	// TODO: select based on CIDR range as well ( pods in a node pool or per node ).
	if addr == "" && ep.Endpoint.Address != "" {
		addr = ep.Endpoint.Address
		h, _, _ := net.SplitHostPort(addr)
		addr = net.JoinHostPort(h, "15008")
	}

	// fallback to cluster address, typical for external
	if addr == "" {
		addr = ep.c.Addr
	}

	_, err := ep.dialTLS(ctx, addr)
	if err != nil {
		return err
	}

	if ep.c.H2T == nil {
		ep.c.H2T = &http2.Transport{
			ReadIdleTimeout:            10000 * time.Second,
			StrictMaxConcurrentStreams: false,
			AllowHTTP:                  true,
		}
	}
	if useGrpcH2 {
		hc, err := transport.NewHTTP2Client(ctx, ctx, ep.tlsCon, transport.ConnectOptions{
			InitialConnWindowSize: 1 << 26,
			InitialWindowSize:     1 << 25,
			MaxFrameSize:          1 << 24,
		}, func() {
		}, func(reason transport.GoAwayReason) {
		}, func() {
		})
		if err != nil {
			return err
		}
		ep.rt = hc
		return nil
	}
	// Multiplex the connection
	rt, err := ep.c.H2T.NewClientConn(ep.tlsCon)
	if err != nil {
		return err
	}
	ep.rt = rt

	return err
}

func (hc *EndpointCon) Close() error {
	return hc.tlsCon.Close()
}

var ProxyCnt atomic.Int32

// Proxy forwards from nc to in/w.
// nc is typically the result of DialContext
func Proxy(nc net.Conn, in io.Reader, w io.Writer, dest string) error {
	t1 := time.Now()
	id := ProxyCnt.Add(1)
	ids := strconv.Itoa(int(id))
	ch := make(chan int)
	ch2 := make(chan int)
	s1 := &nio.ReaderCopier{
		ID:  dest + "-o-" + ids,
		Out: nc,
		In:  in,
	}
	go s1.Copy(ch, true)

	s2 := &nio.ReaderCopier{
		ID:  dest + "-i-" + ids,
		Out: w,
		In:  nc,
	}
	go s2.Copy(ch2, true)

	for i := 0; i < 2; i++ {
		select {
		case <-ch:
			if s1.Err != nil {
				s2.Close()
				break
			}
			log.Println("Proxy in done", id, s1.Err, s1.InError, s1.Written)
		case <-ch2:
			if s2.Err != nil {
				s1.Close()
				break
			}
			log.Println("Proxy out done", id, s2.Err, s2.InError, s2.Written)
		}
	}

	// This may never terminate - out may be in error, and this blocked

	err := proxyError(s1.Err, s2.Err, s1.InError, s2.InError)

	log.Println("proxy-copy-done", id,
		dest,
		//"conTime", t1.Sub(t0),
		"dur", time.Since(t1),
		"maxRead", s1.MaxRead, s2.MaxRead,
		"readCnt", s1.ReadCnt, s2.ReadCnt,
		"avgCnt", int(s1.Written)/(s1.ReadCnt+1), int(s2.Written)/(s2.ReadCnt+1),
		"in", s1.Written,
		"out", s2.Written,
		"err", err)

	nc.Close()
	if c, ok := in.(io.Closer); ok {
		c.Close()
	}
	if c, ok := w.(io.Closer); ok {
		c.Close()
	}

	return err
}

func proxyError(errout error, errorin error, outInErr bool, inInerr bool) error {
	if errout == nil && errorin == nil {
		return nil
	}
	if errout == nil {
		return errorin
	}
	if errorin == nil {
		return errout
	}

	return errors.New("IN+OUT " + errorin.Error() + " " + errout.Error())
}

// LocalForwardPort is a helper for port forwarding, similar with -L localPort:dest:destPort
func LocalForwardPort(localAddr, dest string, hb *HBone) error {
	l, err := net.Listen("tcp", localAddr)
	if err != nil {
		return err
	}

	for {
		a, err := l.Accept()
		if err != nil {
			log.Println("Error accepting", err)
			return err
		}
		go func() {
			a := a

			ctx := context.Background()

			nc, err := hb.DialContext(ctx, "", dest)
			if err != nil {
				log.Println("Dial error", dest, err)
				a.Close()
				return
			}
			err = Proxy(nc, a, a, dest)
			if err != nil {
				log.Println("FWD", localAddr, a.RemoteAddr(), err)
			} else {
				log.Println("FWD", localAddr, a.RemoteAddr())
			}
		}()
	}
}
