package hbone

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"runtime/debug"
	"sync"
	"time"

	"golang.org/x/net/http2"
)

// Auth is the interface expected by hbone for mTLS support.
type Auth interface {
	GenerateTLSConfigServer() *tls.Config
	GenerateTLSConfigClient(name string) *tls.Config
}

// Debug for dev support, will log verbose info.
// Avoiding dependency on logging - eventually a trace interface will be provided
// so any logger can be used.
var Debug = false

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

	// Auth plugs-in mTLS support. The generated configs should perform basic mesh
	// authentication.
	Auth Auth
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

	// h2Server is the server used for accepting HBONE connections
	h2Server *http2.Server

	Cert *tls.Certificate

	// Clients by name
	Clusters map[string]*Cluster

	// rp is used when HBone is used to proxy to a local http/1.1 server.
	rp *httputil.ReverseProxy

	// Non-local endpoints. Key is the 'pod id' of a H2R client
	Endpoints map[string]*EndpointCon

	// H2R holds H2 client (reverse) connections to the local server.
	// Will be used to route requests directly. Key is the SNI expected in forwarding requests.
	H2R map[string]http.RoundTripper

	h2rListener net.Listener
	sniListener net.Listener
	h2t         *http2.Transport

	SNIAddr string

	TcpAddr string

	// Ports is the equivalent of container ports in k8s.
	// Name follows the same conventions as Istio and should match the port name in the Service.
	// Port "*" means 'any' port - if set, allows connections to any port by number.
	// Currently this is loaded from env variables named PORT_name=value, with the default PORT_http=8080
	// TODO: refine the 'wildcard' to indicate http1/2 protocol
	// TODO: this can be populated from a WorkloadGroup object, loaded from XDS or mesh env.
	Ports map[string]string

	TokenCallback func(ctx context.Context, host string) (string, error)
	Mux           http.ServeMux

	// Timeout used for TLS handshakes. If not set, 3 seconds is used.
	HandsahakeTimeout time.Duration

	EndpointResolver func(sni string) *EndpointCon

	m           sync.RWMutex
	H2RConn     map[*http2.ClientConn]string
	H2RCallback func(string, *http2.ClientConn)

	// Transport returns a wrapper for the h2c RoundTripper.
	Transport func(tripper http.RoundTripper) http.RoundTripper

	HandlerWrapper func(h http.Handler) http.Handler

	// Client is a
	Client         *http.Client
	ConnectTimeout time.Duration
}

// New creates a new HBone node. It requires a workload identity, including mTLS certificates.
func New(auth Auth) *HBone {
	return NewMesh(&MeshSettings{
		Auth: auth,
	})
}

// NewMesh creates the mesh object. It requires an auth source. Configuring the auth source should also initialize
// the identity and basic settings.
func NewMesh(ms *MeshSettings) *HBone {

	// Need to set this to allow timeout on the read header
	h1 := &http.Transport{
		ExpectContinueTimeout: 3 * time.Second,
	}
	h2, _ := http2.ConfigureTransports(h1)
	h2.ReadIdleTimeout = 10 * time.Minute // TODO: much larger to support long-lived connections
	h2.AllowHTTP = true
	h2.StrictMaxConcurrentStreams = false
	hb := &HBone{
		ConnectTimeout: 5 * time.Second,
		MeshSettings:   ms,
		Endpoints:      map[string]*EndpointCon{},
		H2R:            map[string]http.RoundTripper{},
		H2RConn:        map[*http2.ClientConn]string{},
		TcpAddr:        "127.0.0.1:8080",
		h2t:            h2,
		Ports:          map[string]string{},
		Clusters:       map[string]*Cluster{},
		Client:         http.DefaultClient,
		AuthProviders:  map[string]func(context.Context, string) (string, error){},
		//&http2.Transport{
		//	ReadIdleTimeout: 10000 * time.Second,
		//	StrictMaxConcurrentStreams: false,
		//	AllowHTTP: true,
		//},

	}
	hb.h2t.ConnPool = hb
	hb.h2Server = &http2.Server{}

	u, _ := url.Parse("http://127.0.0.1:8080")
	hb.rp = httputil.NewSingleHostReverseProxy(u)

	return hb
}

func (uk *HBone) AddService(rc *Cluster) {
	uk.m.Lock()
	uk.Clusters[rc.Id] = rc
	uk.m.Unlock()
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
	conf := hb.Auth.GenerateTLSConfigServer()
	defer conn.Close()
	tls := tls.Server(conn, conf)

	err := nio.HandshakeTimeout(tls, hb.HandsahakeTimeout, conn)
	if err != nil {
		return
	}

	hb.HandleAcceptedH2C(tls)
}

// HandleAcceptedH2C handles a plain text H2 connection, for example
// in case of secure networks.
func (hb *HBone) HandleAcceptedH2C(conn net.Conn) {
	var hc http.Handler
	hc = &HBoneAcceptedConn{hb: hb, conn: conn}

	if hb.HandlerWrapper != nil {
		hc = hb.HandlerWrapper(hc)
	}

	hb.h2Server.ServeConn(
		conn,
		&http2.ServeConnOpts{
			Handler: hc, // Also plain text, needs to be upgraded
			Context: context.Background(),
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
	t0 := time.Now()
	var proxyErr error
	defer func() {
		log.Println(r.Method, r.URL, r.Proto, r.Host, r.RemoteAddr, time.Since(t0), proxyErr)

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

	// TODO: parse Envoy / hbone headers.
	w.(http.Flusher).Flush()

	// TCP Proxy for SSH ( no mTLS, SSH has its own equivalent)
	if r.RequestURI == "/_hbone/22" {
		proxyErr = hac.hb.HandleTCPProxy(w, r.Body, "localhost:15022")
		return
	}
	if r.RequestURI == "/_hbone/15003" {
		proxyErr = hac.hb.HandleTCPProxy(w, r.Body, "localhost:15003")
		return
	}
	if r.RequestURI == "/_hbone/tcp" {
		proxyErr = hac.hb.HandleTCPProxy(w, r.Body, hac.hb.TcpAddr)
		return
	}

	if r.RequestURI == "/_hbone/mtls" {
		// Create a stream, used for Proxy with caching.
		conf := hac.hb.Auth.GenerateTLSConfigServer()

		tls := tls.Server(&nio.HTTPConn{R: r.Body, W: w, Conn: hac.conn}, conf)

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
		hac.hb.HandleTCPProxy(tls, tls, hac.hb.TcpAddr)
		//if tls.ConnectionState().NegotiatedProtocol == "h2" {
		//	// http2 and http expect a net.Listener, and do their own accept()
		//	hb.Proxy.ServeConn(
		//		tls,
		//		&http2.ServeConnOpts{
		//			Handler: http.HandlerFunc(l.ug.H2Handler.httpHandleHboneCHTTP),
		//			Context: tc.Context(), // associated with the stream, with cancel
		//		})
		//} else {
		//	// HTTP/1.1
		//	// TODO. Typically we want to upgrade over the wire to H2
		//}
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
func (hb *HBone) HandleTCPProxy(w io.Writer, r io.Reader, hostPort string) error {
	nc, err := net.Dial("tcp", hostPort)
	if err != nil {
		log.Println("Error dialing ", hostPort, err)
		return err
	}

	s1 := &nio.ReaderCopier{
		ID:  "TCP-o",
		Out: nc,
		In:  r,
	}
	ch := make(chan int)
	go s1.Copy(ch, true)

	s2 := nio.ReaderCopier{
		ID:  "TCP-i",
		Out: w,
		In:  nc,
	}
	s2.Copy(nil, true)
	<-ch

	if s1.Err != nil {
		return s1.Err
	}
	if s2.Err != nil {
		return s2.Err
	}

	return nil
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
