package hbone

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"runtime/debug"
	"sync"
	"time"

	"github.com/costinm/hbone/ext/transport"
	"github.com/costinm/hbone/nio"
	"github.com/costinm/hbone/nio/syscall"

	//"golang.org/x/net/http2"
	"github.com/costinm/hbone/ext/http2"
)

// Auth is the interface expected by hbone for mTLS support.
type Auth interface {
	GenerateTLSConfigServer() *tls.Config
	GenerateTLSConfigClient(name string) *tls.Config
	GenerateTLSConfigClientRoots(name string, pool *x509.CertPool) *tls.Config
}

// Debug for dev support, will log verbose info.
// Avoiding dependency on logging - eventually a trace interface will be provided
// so any logger can be used.
var Debug = true

// MeshSettings has common settings for all clients
type MeshSettings struct {
	// If TransportWrapper is set, the http clients will be wrapped
	// This is intended for integration with OpenTelemetry or other transport wrappers.
	TransportWrapper func(transport http.RoundTripper) http.RoundTripper

	// Hub or user project ID. If set, will be used to lookup clusters.
	ProjectId      string
	Namespace      string
	ServiceAccount string

	// Location where the workload is running, to select local clusters.
	Location string

	ConnectTimeout time.Duration
	TCPUserTimeout time.Duration

	// Clients by name
	Clusters map[string]*Cluster

	// Ports is the equivalent of container ports in k8s.
	// Name follows the same conventions as Istio and should match the port name in the Service.
	// Port "*" means 'any' port - if set, allows connections to any port by number.
	// Currently this is loaded from env variables named PORT_name=value, with the default PORT_http=8080
	// TODO: refine the 'wildcard' to indicate http1/2 protocol
	// TODO: this can be populated from a WorkloadGroup object, loaded from XDS or mesh env.
	Ports map[string]string

	// Timeout used for TLS handshakes. If not set, 3 seconds is used.
	HandsahakeTimeout time.Duration

	// Auth plugs-in mTLS support. The generated configs should perform basic mesh
	// authentication.
	Auth Auth

	EnvironmentVariables map[string]string

	// Internal ports

	// Default to 0.0.0.0:15008
	HBone string

	// Reverse tunnel to this address if set
	RemoteTunnel string

	// If set, hbonec is enabled on this address
	// TrustedIPRanges should be used instead.
	HBoneC string

	// Default to localhost:1080
	SocksAddr string

	// SNI port, default to 15003
	SNI string

	AdminPort string

	LocalForward map[int]string
	// Envoy/Istio

	// ServiceCluster is mapped to Istio canonical service and envoy --serviceCluster
	// It will show up in x-envoy-downstream-service-cluster if user_agent is true
	ServiceCluster string

	// Secure is the list of secure networks (IPSec, wireguard, etc).
	// If both client and server are on a secure network, tls is not used.
	// WIP - for now any string will cause the cluster to use plaintext.
	SecureCIDR []string

	// ServiceNode is mapped to node name and envoy --service-node
	// It will show up in x-envoy-downstream-service-node
	ServiceNode string
}

// HBone represents a node using a HTTP/2 or HTTP/3 based overlay network environment.
// This can act as a minimal REST client and server - or can be used as a RoundTripper, Dialer and Listener
// compatible with HBONE protocol and mesh security.
//
// HBone by default uses mTLS, using spiffee identities encoding K8S namespace, KSA and a trust
// domain. Other forms of authentication can be supported - auth is handled via configurable
// interface, not part of the core package.
//
// HBone can be used as a client, server or proxy/gateway.
type HBone struct {
	*MeshSettings

	// AuthProviders - matching kubeconfig user.authProvider.name
	// It is expected to return tokens with the given audience - in case of GCP
	// returns access tokens. If not set the cluster can't be created.
	AuthProviders map[string]func(context.Context, string) (string, error)
	TokenCallback func(ctx context.Context, host string) (string, error)

	// rp is used when HBone is used to proxy to a local http/1.1 server.
	rp *httputil.ReverseProxy

	// h2Server is the server used for accepting HBONE connections
	h2Server *http2.Server
	// h2t is the transport used for all h2 connections used.
	// hb is the connection pool, gets notified when con is closed.
	h2t *http2.Transport

	Mux http.ServeMux

	// EndpointResolver hooks into the Dial process and return the configured
	// EndpointCon object. This integrates with the XDS/config plane, with
	// additional local configs.
	EndpointResolver func(sni string) *EndpointCon

	m           sync.RWMutex
	H2RConn     map[*http2.ClientConn]*EndpointCon
	H2RCallback func(string, *http2.ClientConn)

	// Transport returns a wrapper for the h2c RoundTripper.
	Transport func(tripper http.RoundTripper) http.RoundTripper

	HandlerWrapper func(h http.Handler) http.Handler

	Client *http.Client
	DialF  func(context.Context, *Cluster, net.Conn) (Mux, error)
	ServeF func(ctx context.Context, hb *HBone, conn net.Conn) error
}

type noAuth struct {
}

func (n noAuth) GenerateTLSConfigServer() *tls.Config {
	return nil
}

func (n noAuth) GenerateTLSConfigClient(name string) *tls.Config {
	return nil
}

func (n noAuth) GenerateTLSConfigClientRoots(name string, pool *x509.CertPool) *tls.Config {
	return &tls.Config{
		//MinVersion: tls.VersionTLS13,
		//PreferServerCipherSuites: ugate.preferServerCipherSuites(),

		ServerName: name,
		NextProtos: []string{"h2"},

		RootCAs: pool,
	}
}

// New creates a new HBone node. It requires a workload identity, including mTLS certificates.
func New(auth Auth) *HBone {
	return NewMesh(&MeshSettings{
		Auth: auth,
	})
}

func NewHBone(ms *MeshSettings, auth Auth) *HBone {
	if ms == nil {
		ms = &MeshSettings{}
	}
	ms.Auth = auth
	return NewMesh(ms)
}

// NewMesh creates the mesh object. It requires an auth source. Configuring the auth source should also initialize
// the identity and basic settings.
func NewMesh(ms *MeshSettings) *HBone {

	// Need to set this to allow timeout on the read header
	//h1 := &http.Transport{
	//	ExpectContinueTimeout: 3 * time.Second,
	//}
	//h2, _ := http2.ConfigureTransports(h1)
	//h2.ReadIdleTimeout = 10 * time.Minute // TODO: much larger to support long-lived connections
	//h2.AllowHTTP = true
	//h2.StrictMaxConcurrentStreams = false

	hb := &HBone{
		MeshSettings: ms,
		H2RConn:      map[*http2.ClientConn]*EndpointCon{},
		//h2t:           h2,
		Client:        http.DefaultClient,
		AuthProviders: map[string]func(context.Context, string) (string, error){},
		//&http2.Transport{
		//	ReadIdleTimeout: 10000 * time.Second,
		//	StrictMaxConcurrentStreams: false,
		//	AllowHTTP: true,
		//},

	}
	//hb.h2t.ConnPool = hb

	if ms.Auth == nil {
		ms.Auth = &noAuth{}
	}

	ms.HandsahakeTimeout = 10 * time.Second

	if ms.Clusters == nil {
		ms.Clusters = map[string]*Cluster{}
	} else {
		for _, c := range ms.Clusters {
			c.hb = hb
		}
	}
	if ms.ConnectTimeout == 0 {
		ms.ConnectTimeout = 5 * time.Second
	}

	hb.h2Server = &http2.Server{
		//MaxReadFrameSize: 1<<24 - 1, // 16M may be too much

		MaxUploadBufferPerConnection: math.MaxInt32 / 2, // not uint32.
		MaxUploadBufferPerStream:     math.MaxInt32 / 4,
	}

	// Init the HTTP reverse proxy, for apps listening for HTTP/1.1 on 8080
	// This is used for serverless but also support regular pods.
	// TODO: customize the port.
	// TODO: add a h2 reverse proxy as well on 8082, and grpc on 8081
	u, _ := url.Parse("http://127.0.0.1:8080")
	hb.rp = httputil.NewSingleHostReverseProxy(u)

	return hb
}

// StartBHoneD will listen on addr as H2C (typically :15009)
//
//
// Incoming streams for /_hbone/mtls will be treated as a mTLS connection,
// using the Istio certificates and root. After handling mTLS, the clear text
// connection will be forwarded to localhost:8080 ( TODO: custom port ).
//
// TODO: setting for app protocol=h2, http, tcp - initial impl uses tcp
//
// Incoming requests for /_hbone/22 will be forwarded to localhost:22, for
// debugging with ssh.
//

// HandleAcceptedH2 implements server-side handling of the conn - including
// TLS handshake.
// conn may be a wrapped connection.
func (hb *HBone) HandleAcceptedH2(conn net.Conn) {
	if hb.TCPUserTimeout != 0 {
		syscall.SetTCPUserTimeout(conn, hb.TCPUserTimeout)
	}
	conf := hb.Auth.GenerateTLSConfigServer()
	defer conn.Close()
	tls := tls.Server(conn, conf)

	err := nio.HandshakeTimeout(tls, hb.HandsahakeTimeout, conn)
	if err != nil {
		return
	}

	hb.HandleAcceptedH2C(tls)
}

// SecureConn return true if the connection the the specific endpoint is over a secure network and doesn't
// need encryption.
func (hb *HBone) SecureConn(ep *Endpoint) bool {
	//if strings.HasPrefix(ip, "localhost") {
	//	return true
	//}
	//if strings.HasPrefix(ip, "127.") {
	//	return true
	//}

	return ep.Secure
}

const useGrpcH2 = false

// HandleAcceptedH2C handles a plain text H2 connection, for example
// in case of secure networks.
func (hb *HBone) HandleAcceptedH2C(conn net.Conn) {
	if hb.TCPUserTimeout != 0 {
		// only for TCPConn - if this is used for tls no effect
		syscall.SetTCPUserTimeout(conn, hb.TCPUserTimeout)
	}
	var hc http.Handler
	hc = &HBoneAcceptedConn{hb: hb, conn: conn}

	if hb.HandlerWrapper != nil {
		hc = hb.HandlerWrapper(hc)
	}

	if hb.ServeF != nil {
		hb.ServeF(context.Background(), hb, conn)
		return
	}

	if useGrpcH2 {
		st, err := transport.NewServerTransport(conn, &transport.ServerConfig{
			MaxFrameSize:          1 << 24,
			InitialConnWindowSize: 1 << 26,
			InitialWindowSize:     1 << 25,
		})
		if err != nil {
			log.Println("H2 server err", err)
			conn.Close()
			return
		}

		// blocks - read frames
		st.HandleStreams(func(stream *transport.Stream) {
			go func() {

				host := stream.Request.Host
				_, p, _ := net.SplitHostPort(host)
				// TODO: verify host is endpoint IP ?
				// TODO: support gateway mode

				hostPort := "localhost:" + p

				log.Println("HBone-START", stream.Id, host, stream)
				nc, err := net.Dial("tcp", hostPort)
				if err != nil {
					log.Println("Error dialing ", hostPort, err)
					return
				}

				stream.Response.Status = "200"
				stream.Response.Header.Add("x-status", "200")
				st.WriteHeader(stream)
				proxyErr := Proxy(nc, stream, stream, hostPort)
				log.Println("HBone-END: ", stream.Id, host, proxyErr)
			}()

		}, func(ctx context.Context, s string) context.Context {
			log.Println("Trace", s)
			return ctx
		})
		return
	}

	hb.h2Server.ServeConn(
		conn,
		&http2.ServeConnOpts{
			Handler: hc, // Also plain text, needs to be upgraded
			Context: context.Background(),
			//Raw:     hb,
			//Context can be used to cancel, pass meta.
			// h2 adds http.LocalAddrContextKey(NetAddr), ServerContextKey (*Server)
		})
}

// HBoneAcceptedConn keeps track of one accepted H2 connection.
type HBoneAcceptedConn struct {
	hb   *HBone
	conn net.Conn
}

// ServeHTTP implements the basic TCP-over-H2 and H2 proxy protocol.
// Requests that are not HBone will be handled by the mux in HBone, and
// if they don't match a handler may be forwarded by the reverse HTTP
// proxy.
func (hac *HBoneAcceptedConn) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	log.Println("ServeHTTP", r.Header)
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
	_, p, err := net.SplitHostPort(host)
	if err != nil {
		proxyErr = err
		w.Header().Add("X-Error", err.Error())
		return
	}

	w.(http.Flusher).Flush()

	tunMode := r.Header.Get("X-tun")
	if tunMode != "" {
		_, p, err = net.SplitHostPort(tunMode)

		// Create a stream, used for Proxy with caching.
		conf := hac.hb.Auth.GenerateTLSConfigServer()

		tls := tls.Server(&HTTPConn{R: r.Body,
			W:    w,
			Req:  r,
			ResW: w,
			Conn: hac.conn}, conf)

		// TODO: replace with handshake with context
		err := nio.HandshakeTimeout(tls, hac.hb.HandsahakeTimeout, nil)
		if err != nil {
			log.Println("HBD-MTLS: error inner mTLS ", err)
			return
		}
		log.Println("HBD-MTLS:", tls.ConnectionState())

		// TODO: All Istio checks go here. The TLS handshake doesn't check
		// root cert or anything - this is proof of concept only, to eval
		// perf.

		// TODO: allow user to customize app port, protocol.
		// TODO: if protocol is not matching wire protocol, convert.

		hac.hb.HandleTCPProxy(w, tls, tls, "localhost:"+p)

		return
	}

	// TODO: other indicators/headers to identify HBONE ?

	// For connect, the requestURI is the same as host - probably for backward compat
	if r.Method == "CONNECT" || r.URL.Path == "" {
		// TODO: verify host is endpoint IP ?
		// TODO: support gateway mode
		proxyErr = hac.hb.HandleTCPProxy(w, w, r.Body, "localhost:"+p)
		return
	}

	rh, pat := hac.hb.Mux.Handler(r)
	if pat != "" {
		rh.ServeHTTP(w, r)
		return
	}

	// Make sure xfcc header is removed
	r.Header.Del("x-forwarded-client-cert")
	hac.hb.rp.ServeHTTP(w, r)
}

// HandleTCPProxy connects and forwards r/w to the hostPort
func (hb *HBone) HandleTCPProxy(hw http.ResponseWriter, w io.Writer, r io.Reader, hostPort string) error {
	log.Println("net.Dial", hostPort)
	nc, err := net.Dial("tcp", hostPort)
	if err != nil {
		log.Println("Error dialing ", hostPort, err)
		return err
	}

	return Proxy(nc, r, w, hostPort)
}

// HttpClient returns a http.Client configured with the specified root CA, and reasonable settings.
// The URest wrapper is added, for telemetry or other interceptors.
func (uK8S *HBone) HttpClient(caCert []byte) *http.Client {
	// The 'max idle conns, idle con timeout, etc are shorter - this is meant for
	// fast initial config, not as a general purpose client.
	tr := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,

		MaxIdleConns:    10,
		IdleConnTimeout: 30 * time.Second,
	}

	if caCert != nil && len(caCert) > 0 {
		roots := x509.NewCertPool()
		if !roots.AppendCertsFromPEM(caCert) {
			log.Println("Failed to decode PEM")
		}
		tr.TLSClientConfig = &tls.Config{
			RootCAs: roots,
		}
	}

	var rt http.RoundTripper
	rt = tr
	if uK8S.TransportWrapper != nil {
		rt = uK8S.TransportWrapper(rt)
	}

	return &http.Client{
		Transport: rt,
	}
}

//func (hb *HBone) handleIOCH(ioch chan *fh2.IOEvent, conn net.Conn) {
//	for {
//		select {
//		case ioe := <-ioch:
//			log.Println(ioe.Type, ioe.Frame)
//			switch fh2.FrameType(ioe.Type) {
//			case fh2.FrameHeaders:
//				ioe.Stream.IOChannel = ioch // will not use a go-routine per stream
//				go hb.handleIOStream(ioch, ioe.Stream, ioe)
//			case fh2.FrameData:
//
//			case fh2.FrameWindowUpdate:
//			case fh2.FrameResetStream:
//
//			}
//
//		}
//	}
//
//}
//
//func (hb *HBone) handleIOStream(ioch chan *fh2.IOEvent, s *fh2.Stream, ioe *fh2.IOEvent) {
//
//	fh := ioe.Frame.Body().(*fh2.Headers)
//	h := http.Header{}
//	s.ProcessHeaders(fh.Headers(), h)
//
//	ioe.Stream.SendHeaders(true, &http.Header{})
//
//}
