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

	"github.com/costinm/hbone/h2"
	"github.com/costinm/hbone/nio"
	"github.com/costinm/hbone/nio/syscall"
)

// Cluster represents a set of endpoints, with a common configuration.
// Can be a K8S Service with VIP and DNS name, an external service, etc.
//
// Similar with Envoy Cluster or K8S service, can also represent single
// endpoint with multiple paths/IPs.
type Cluster struct {
	// Cluster WorkloadID - the cluster name in kube config, hub, gke - cluster name in XDS
	// Defaults to Base addr - but it is possible to have multiple clusters for
	// same address ( ex. different users / token providers).
	//
	// Examples:
	// GKE cluster: gke_PROJECT_LOCATION_NAME
	ID string `json:"id,omitempty"`

	// ServiceAddr is the TCP address of the cluster - VIP:port or DNS:port
	// For K8S service it should be in the form serviceName.namespace.svc:svcPort
	//
	// This can also be an 'original dst' address for single-endpoint clusters.
	// Can be a DNS name that resolves, or some other node.
	// Individual IPs (relay, etc) will be in the info.addrs field
	// May also be a URL (webpush endpoint).
	Addr string `json:"addr,omitempty"`

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

	// Primary public key of the node.
	// EC256: 65 bytes, uncompressed format
	// RSA: DER
	// ED25519: 32B
	// Used for sending encryted webpush message
	// If not known, will be populated after the connection.
	PublicKey []byte `json:"pub,omitempty"`

	// cached value
	roots *x509.CertPool

	// TODO: UserAgent, DefaultHeaders

	// If set, a token source with this name is used.
	// If not found, no tokens will be added. If found, errors getting tokens will result
	// in errors connecting.
	TokenSource string

	// Optional TokenProvider - not needed if client wraps google oauth
	// or mTLS is used.
	TokenProvider func(context.Context, string) (string, error)

	// Static token to use. May be a long lived K8S service account secret or other long-lived creds.
	Token string

	// For GKE K8S clusters - extracted from ID.
	// This is the default location for the endpoints.
	Location string

	// From CDS

	// timeout for new network connections to endpoints in cluster
	ConnectTimeout           time.Duration
	TCPKeepAlive             time.Duration
	TCPUserTimeout           time.Duration
	MaxRequestsPerConnection int

	// Default values for initial window size, initial window, max frame size
	InitialConnWindowSize int32
	InitialWindowSize     int32
	MaxFrameSize          uint32

	// Client configured with the root CA of the K8S cluster, used
	// for HTTP/1.1 requests. If set, the cluster is not HBone/H2 but a fallback
	// or non-mesh destination.
	// TODO: customize Dialer to still use mesh LB
	// TODO: attempt ws for tunneling H2
	Client *http.Client

	// TLS config used when dialing using workload identity, shared
	TLSClientConfig *tls.Config

	//// Shared by all endpoints for this cluster
	//H2T *http2.Transport

	// If set, will be used to select the next endpoint. Based on lb_policy
	// May dial a new connection.
	//LB func(*Cluster) *EndpointCon

	LastUsed time.Time
	Dynamic  bool

	h2.Events
}

// Endpoint represents a connection/association with a cluster.
// May be a separate pod, or another IP for an existing pod.
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
	Cluster  *Cluster
	Endpoint *Endpoint

	rt     http.RoundTripper // *http2.ClientConn or custom (wrapper)
	tlsCon net.Conn
	// The stream connection - may be a real TCP or not
	streamCon       net.Conn
	ConnectionStart time.Time
	SSLEnd          time.Time
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
	if c.ID != "" {
		hb.Clusters[c.ID] = c
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
		c = &Cluster{Addr: addr, hb: hb, Dynamic: true}
		hb.AddService(c)
	}
	c.LastUsed = time.Now()
	return c, nil
}

// DialTLS creates an outer layer TLS connection with the H2 CONNECT (or POST) address.
// Will initiate a TCP connection first - possibly using the SNI gate, and do the handshake.
func (hc *EndpointCon) DialTLS(ctx context.Context, addr string) (net.Conn, error) {
	c := hc.Cluster
	d := &net.Dialer{
		Timeout:   c.ConnectTimeout,
		KeepAlive: c.TCPKeepAlive,
	}

	if hc.Endpoint.SNIGate != "" {
		addr = hc.Endpoint.SNIGate
		// TODO: mangle the address of the hbone port and/or endpoint
	}

	// TODO: DNSStart/End

	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}

	hc.ConnectionStart = time.Now()

	// If net connection is cut, by default the socket may linger for up to 20 min without detecting this.
	// Extracted from gRPC - needs to apply at TCP socket level
	if c.TCPUserTimeout != 0 {
		syscall.SetTCPUserTimeout(conn, c.TCPUserTimeout)
	}

	hc.streamCon = conn

	// TODO: skip TLS if trusted network
	if hc.Cluster.hb.SecureConn(hc.Endpoint) {
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

	err = nio.HandshakeTimeout(tlsCon, hc.Cluster.hb.HandsahakeTimeout, conn)
	if err != nil {
		return nil, err
	}

	// tlsCon.VerifyHostname(c.SNI) is handled in the verifier

	hc.SSLEnd = time.Now()

	hc.tlsCon = tlsCon
	return tlsCon, nil
}

func (c *Cluster) DoRequest(req *http.Request) ([]byte, error) {
	var resp *http.Response
	var err error

	resp, err = c.RoundTrip(req) // Client.Do(req)
	if Debug {
		log.Println("DoRequest", req, resp, err)
	}

	if err != nil {
		return nil, err
	}

	defer resp.Body.Close()
	data, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		log.Println("readall", err)
		return nil, err
	}
	if len(data) == 0 {
		log.Println("readall", err)
		return nil, io.EOF
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
	_, nc, err := c.dial(ctx, r)
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

	return nc, err
}

func (c *Cluster) AddToken(req *http.Request, aut string) error {
	if c.TokenSource != "" {
		tp := c.hb.AuthProviders[c.TokenSource]
		if tp != nil {
			t, err := tp(req.Context(), aut)
			if err != nil {
				return err
			}
			req.Header.Add("authorization", "Bearer "+t)
		}
	}
	if c.TokenProvider != nil {
		t, err := c.TokenProvider(req.Context(), aut)
		if err != nil {
			return err
		}
		req.Header.Add("authorization", "Bearer "+t)
	}

	if c.Token != "" {
		req.Header.Add("authorization", c.Token)
	}
	return nil
}

// TODO(costin): use the hostname, get IP override from x-original-dst header or cookie.
func (c *Cluster) dial(ctx context.Context, req *http.Request) (*EndpointCon, net.Conn, error) {
	epc, err := c.findMux(ctx)
	if err != nil {
		return nil, nil, err
	}

	// Hacky dial-using-proxy. Should be handled by rt(). The RoundTripStart method takes the result and does
	// an extra mTLS handshake.
	//
	// For http requests calling Roundtrip, the same should happen.
	if epc.Endpoint.Labels["http_proxy"] != "" {
		// TODO: only POST mode supported right now, address not from label.

		// Tunnel mode, untrusted proxy authentication.
		req, _ := http.NewRequestWithContext(ctx, "POST", "https://"+epc.Endpoint.HBoneAddress, nil)

		err = c.AddToken(req, "https://"+epc.Endpoint.HBoneAddress)
		if err != nil {
			return nil, nil, err
		}

		req.Header.Add("x-service", c.Addr)
		req.Header.Add("x-tun", epc.Endpoint.Address)

		res, err := epc.rt.RoundTrip(req)
		if err != nil {
			epc.rt = nil
			return nil, nil, err
		}

		nc := res.Body.(net.Conn)
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
			return nil, nil, err
		}
		//log.Println("client-rt tun handshake", tlsTun.ConnectionState())
		return epc, tlsTun, err

		// TLS wrapper will be added on response.
		//return epc, res.Body.(net.Conn), err //
		// &HTTPConn{R: res.Body, W: o, Conn: epc.tlsCon, Req: req, Res: res,
		//	Cluster: c}, err
	}

	if req == nil {
		req, _ = http.NewRequestWithContext(ctx, "CONNECT", "https://"+epc.Endpoint.Address, nil)
	}

	req.Header.Add("x-service", c.Addr)

	res, _, err := c.rt(epc, req)
	if err != nil {
		return nil, nil, err
	}

	nc := res.Body.(net.Conn)
	// TODO: return nc directly instead of HTTPConn
	return epc, nc, err
}

//var InitH2ClientConn func(ctx context.Context, req *http.Request, epc *EndpointCon, c *Cluster) (*EndpointCon, net.Conn, error)

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

	c.AddToken(req, "https://"+c.Addr)

	for i := 0; i < 3; i++ {

		// Find a channel - LB would go here if multiple addresses and sockets
		if epc == nil || epc.rt == nil {
			epc, err = c.findMux(req.Context())
			if err != nil {
				return nil, nil, err
			}
		}

		// IMPORTANT: some servers will not return the headers until the first byte of the response is sent, which
		// may not happen until request bytes have been sent.
		// For CONNECT, we will require that the server is flushing the headers as soon as the request is received,
		// to emulate the connection semantics - at least initially.
		// For POST and other methods - we can't assume this. That means read() on the conn will need to be blocked
		// and wait for the Header frame to be received, and any metadata too.
		resp, rterr = epc.rt.RoundTrip(req)
		if Debug {
			log.Println("RoundTrip", req, resp, rterr)
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
			Cluster:  c,
			Endpoint: endp,
		}
		c.EndpointCon = append(c.EndpointCon, ep)
	}
	ep := c.EndpointCon[0]
	if cc, ok := ep.rt.(*h2.HTTP2ClientMux); ok {
		if !cc.CanTakeNewRequest() {
			ep.rt = nil
		}
		//if cc.State().StreamsActive > 128 {
		//	// TODO: create new endpoint
		//}
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

// RoundTripStart a single connection to the host, wrap it with a h2 RoundTripper.
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
	hc, err := h2.NewHTTP2Client(ctx,
		h2.H2Config{
			InitialConnWindowSize: c.InitialConnWindowSize,
			InitialWindowSize:     c.InitialWindowSize, // 1 << 25,
			MaxFrameSize:          c.MaxFrameSize,      // 1 << 24,
		}, func(reason h2.GoAwayReason) {
			ep.rt = nil
		}, func() {
			log.Println("Muxc: Close ", addr)
			ep.rt = nil
			// TODO: close all associated pending streams ?
		})
	if err != nil {
		return err
	}

	hc.Events.OnEvent(h2.Event_Settings, h2.EventHandlerFunc(func(evt h2.EventType, t *h2.H2Transport, s *h2.H2Stream, f *nio.Buffer) {
		okch <- 1
		log.Println("Muxc: Preface received ", s)
	}))

	hc.Events.Add(ep.Cluster.hb.Events)
	hc.Events.Add(ep.Cluster.Events)

	// TODO: on-demand discovery using XDS, report discovery start.
	hc.MuxEvent(h2.Event_Connect_Start)

	hc.MuxConnStart = time.Now()

	_, err = ep.DialTLS(ctx, addr)
	if err != nil {
		return err
	}

	hc.MuxEvent(h2.Event_Connect_Done)

	alpn := ep.tlsCon.(*tls.Conn).ConnectionState().NegotiatedProtocol
	if alpn != "h2" {
		log.Println("Invalid alpn")
	}

	hc.StartConn(ep.tlsCon)

	<-okch

	ep.rt = hc
	return nil
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
				log.Println("RoundTripStart error", dest, err)
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
