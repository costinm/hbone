package urpc

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math"
	"net"
	"sort"
	"sync"
	"time"

	"github.com/costinm/hbone"
	"github.com/golang/protobuf/jsonpb"
	"google.golang.org/protobuf/reflect/protoreflect"

	"google.golang.org/protobuf/encoding/prototext"
	"google.golang.org/protobuf/proto"

	// Generated protos
	"github.com/costinm/hbone/urpc/gen/xds"
)

var dump = false // true

var marshal = &jsonpb.Marshaler{OrigName: true, Indent: "  "}

// Resource types in xDS v3.
const (
	apiTypePrefix       = "type.googleapis.com/"
	EndpointType        = apiTypePrefix + "envoy.config.endpoint.v3.ClusterLoadAssignment"
	ClusterType         = apiTypePrefix + "envoy.config.cluster.v3.Cluster"
	RouteType           = apiTypePrefix + "envoy.config.route.v3.RouteConfiguration"
	ScopedRouteType     = apiTypePrefix + "envoy.config.route.v3.ScopedRouteConfiguration"
	ListenerType        = apiTypePrefix + "envoy.config.listener.v3.Listener"
	SecretType          = apiTypePrefix + "envoy.extensions.transport_sockets.tls.v3.Secret"
	ExtensionConfigType = apiTypePrefix + "envoy.config.core.v3.TypedExtensionConfig"
	RuntimeType         = apiTypePrefix + "envoy.service.runtime.v3.Runtime"

	// AnyType is used only by ADS
	AnyType = ""
)

const (
	defaultClientMaxReceiveMessageSize = math.MaxInt32
	defaultInitialConnWindowSize       = 1024 * 1024 // default gRPC InitialWindowSize
	defaultInitialWindowSize           = 1024 * 1024 // default gRPC ConnWindowSize
)

// Config for the ADS connection.
type Config struct {
	// Namespace defaults to 'default'
	Namespace string

	// Workload defaults to 'test'
	Workload string

	// Revision for this control plane instance. We will only read configs that match this revision.
	Revision string

	// Meta includes additional metadata for the node
	Meta map[string]interface{}

	Locality *xds.Locality

	XDSHeaders map[string]string

	// NodeType defaults to sidecar. "ingress" and "router" are also supported.
	NodeType string

	// IP is currently the primary key used to locate inbound configs. It is sent by client,
	// must match a known endpoint IP. Tests can use a ServiceEntry to register fake IPs.
	IP string

	// Context used for early cancellation
	Context context.Context

	NodeId string

	// InitialDiscoveryRequests is a list of resources to watch at first, represented as URLs (for new XDS resource naming)
	// or type URLs.
	InitialDiscoveryRequests []*xds.DiscoveryRequest

	// ResponseHandler will be called on each DiscoveryResponse.
	// TODO: mirror Generator, allow adding handler per type
	ResponseHandler func(con *ADSC, response *xds.DiscoveryResponse)

	HBone   *hbone.HBone
	Cluster *hbone.Cluster
}

// ADSC implements a basic client for ADS, for use in stress tests and tools
// or libraries that need to connect to Istio pilot or other ADS servers.
type ADSC struct {
	ctx context.Context

	stream *UGRPC

	// NodeID is the node identity sent to Pilot.
	nodeID string
	node   *xds.Node

	done chan error

	url string

	watchTime time.Time

	// InitialLoad tracks the time to receive the initial configuration.
	InitialLoad time.Duration

	// HTTPListeners contains received listeners with a http_connection_manager filter.
	HTTPListeners map[string]*xds.Listener

	// TCPListeners contains all listeners of type TCP (not-HTTP)
	TCPListeners map[string]*xds.Listener

	// All received Clusters of no-eds type, keyed by name
	Clusters map[string]*xds.Cluster

	// All received Routes, keyed by route name
	Routes map[string]*xds.RouteConfiguration

	// All received endpoints, keyed by cluster name
	Endpoints map[string]*xds.ClusterLoadAssignment

	// Metadata has the node metadata to send to pilot.
	// If nil, the defaults will be used.
	Metadata map[string]interface{}
	Config   *Config

	// Updates includes the type of the last update received from the server, or "close".
	// Short names for eds/cds/lds/rds, as well as init, error-xxxx, long names for other types.
	Updates chan string

	// errChan is used to report errors or success connecting to server
	errChan chan error

	XDSUpdates chan *xds.DiscoveryResponse

	VersionInfo map[string]string

	// Last received message, by type
	Received map[string]*xds.DiscoveryResponse

	mutex sync.Mutex

	watches map[string]Watch

	// LocalCacheDir is set to a base name used to save fetched resources.
	// If set, each update will be saved.
	// TODO: also load at startup - so we can support warm up in init-container, and survive
	// restarts.
	LocalCacheDir string

	// RecvWg is for letting goroutines know when the goroutine handling the ADS stream finishes.
	RecvWg sync.WaitGroup
	// sendNodeMeta is set to true if the connection is new - and we need to send node meta.,
	sendNodeMeta bool

	sync      map[string]time.Time
	sendCount int
}

type Watch struct {
	resources   []string
	lastNonce   string
	lastVersion string
}

var mapper = map[string]protoreflect.Message{}

func init() {
	mapper[ListenerType] = (&xds.Listener{}).ProtoReflect()
}

// ErrTimeout is returned by Wait if no update is received in the given time.
var ErrTimeout = errors.New("timeout")

func Dial(url string, opts *Config) (*ADSC, error) {
	ctx := opts.Context
	if ctx == nil {
		ctx = context.Background()
	}
	return DialContext(ctx, url, opts)
}

// Dial connects to a ADS server, with optional MTLS authentication if a cert dir is specified.
func DialContext(ctx context.Context, url string, opts *Config) (*ADSC, error) {
	if opts.Cluster.Addr == "-" {
		return nil, errors.New("Disabled")
	}
	adsc := &ADSC{
		Config:      opts,
		done:        make(chan error),
		Updates:     make(chan string, 100),
		watches:     map[string]Watch{},
		url:         url,
		ctx:         ctx,
		XDSUpdates:  make(chan *xds.DiscoveryResponse, 100),
		VersionInfo: map[string]string{},
		Received:    map[string]*xds.DiscoveryResponse{},
		RecvWg:      sync.WaitGroup{},
		sync:        map[string]time.Time{},
		errChan:     make(chan error, 10),
	}

	if opts.Namespace == "" {
		opts.Namespace = "default"
	}
	if opts.NodeType == "" {
		opts.NodeType = "sidecar"
	}
	if opts.IP == "" {
		opts.IP = getPrivateIPIfAvailable().String()
	}
	if opts.Workload == "" {
		opts.Workload = "test-1"
	}
	adsc.Metadata = opts.Meta

	adsc.nodeID = fmt.Sprintf("%s~%s~%s.%s~%s.svc.cluster.local", opts.NodeType, opts.IP,
		opts.Workload, opts.Namespace, opts.Namespace)
	if adsc.Config.NodeId != "" {
		adsc.nodeID = adsc.Config.NodeId
	}

	adsc.node = adsc.makeNode()
	err := adsc.Run()
	return adsc, err
}

// Returns a private IP address, or unspecified IP (0.0.0.0) if no IP is available
func getPrivateIPIfAvailable() net.IP {
	addrs, _ := net.InterfaceAddrs()
	for _, addr := range addrs {
		var ip net.IP
		switch v := addr.(type) {
		case *net.IPNet:
			ip = v.IP
		case *net.IPAddr:
			ip = v.IP
		default:
			continue
		}
		if !ip.IsLoopback() {
			return ip
		}
	}
	return net.IPv4zero
}

// Close the stream.
func (a *ADSC) Close() {
	a.mutex.Lock()
	if a.stream != nil {
		_ = a.stream.Stream.CloseWrite()
	}
	a.mutex.Unlock()
}

// Run will run the ADS client.
func (a *ADSC) Run() error {

	ctx := a.ctx
	gstr, err := New(ctx, a.Config.Cluster, a.Config.Cluster.Addr, "/envoy.service.discovery.v3.AggregatedDiscoveryService/StreamAggregatedResources")
	if err != nil {
		return err
	}
	a.Config.Cluster.AddToken(gstr.Stream.Request, "istio-ca")

	if a.Config.XDSHeaders != nil {
		for k, v := range a.Config.XDSHeaders {
			gstr.Stream.Request.Header.Add(k, v)
		}
	}
	_, err = gstr.Stream.Transport().DialStream(gstr.Stream)
	if err != nil {
		return err
	}

	a.stream = gstr
	//gstr.ErrChan = a.errChan

	go a.handleRecv()

	// wait for firsts message
	//err := <-a.errChan

	// TD only returns an error after sending the first request.
	// The ugrpc only starts the stream after the first request.
	return err
}

func (a *ADSC) handleRecv() {
	//a.stream.RoundTripStart()

	// Watch will start watching resources, starting with CDS. Based on the CDS response
	// it will start watching RDS and CDS.
	if len(a.Config.InitialDiscoveryRequests) == 0 {
		a.Config.InitialDiscoveryRequests = []*xds.DiscoveryRequest{
			{TypeUrl: ClusterType},
		}
	}

	a.Config.InitialDiscoveryRequests[0].Node = a.node

	for _, d := range a.Config.InitialDiscoveryRequests {
		err := a.send(d, ReasonInit)
		if err != nil {
			log.Printf("Error sending request: %v", err)
			a.Updates <- "error-" + err.Error()
			return
		}
	}
	a.Updates <- "init"

	a.stream.Stream.WaitHeaders()
	// TODO: check 200

	for {
		var msg xds.DiscoveryResponse
		err := a.stream.RecvMsg(&msg)
		if err != nil {
			log.Printf("Connection closed for %v: %v", a.nodeID, err)
			a.Close()
			a.WaitClear()
			a.Updates <- "close"
			return
		}
		// Makes copy of byte[]
		//proto.Unmarshal(raw.Bytes(), &msg)

		if dump {
			log.Printf("XDS in: %v %s %s %d", msg.TypeUrl, msg.Nonce, msg.VersionInfo, len(msg.Resources))
		}
		if a.Config.ResponseHandler != nil {
			a.Config.ResponseHandler(a, &msg)
		}

		a.mutex.Lock()
		a.VersionInfo[msg.TypeUrl] = msg.VersionInfo
		a.Received[msg.TypeUrl] = &msg
		a.mutex.Unlock()

		listeners := map[string]*xds.Listener{}
		clusters := map[string]*xds.Cluster{}
		routes := map[string]*xds.RouteConfiguration{}
		eds := map[string]*xds.ClusterLoadAssignment{}
		names := []string{}

		for _, rsc := range msg.Resources {
			valBytes := rsc.Value
			if rsc.TypeUrl == ListenerType {
				ll := &xds.Listener{}
				_ = proto.Unmarshal(valBytes, ll)
				listeners[ll.Name] = ll
			} else if rsc.TypeUrl == ClusterType {
				ll := &xds.Cluster{}
				_ = proto.Unmarshal(valBytes, ll)
				clusters[ll.Name] = ll
			} else if rsc.TypeUrl == EndpointType {
				ll := &xds.ClusterLoadAssignment{}
				_ = proto.Unmarshal(valBytes, ll)
				eds[ll.ClusterName] = ll

				names = append(names, ll.ClusterName)
			} else if rsc.TypeUrl == RouteType {
				ll := &xds.RouteConfiguration{}
				_ = proto.Unmarshal(valBytes, ll)
				routes[ll.Name] = ll
				names = append(names, ll.Name)
			}
		}

		switch msg.TypeUrl {
		case ListenerType:
			a.handleLDS(listeners)
		case ClusterType:
			a.handleCDS(clusters)
			a.Clusters = clusters
			HandleCDS(a.Config.HBone, clusters)
		case EndpointType:
			a.handleEDS(eds)
			HandleEDS(a.Config.HBone, eds)
			a.Endpoints = eds
		case RouteType:
			a.handleRDS(routes)
			a.Routes = routes
		default:
			a.Updates <- msg.TypeUrl
		}

		a.ack(&msg, names)
	}
}

func getFilterChains(l *xds.Listener) []*xds.FilterChain {
	chains := l.FilterChains
	//if l.DefaultFilterChain != nil {
	//	chains = append(chains, l.DefaultFilterChain)
	//}
	return chains
}

// nolint: staticcheck
func (a *ADSC) handleLDS(ll map[string]*xds.Listener) {
	routes := []string{}
	tcpL := map[string]*xds.Listener{}
	httpL := map[string]*xds.Listener{}
	for _, l := range ll {
		for _, fc := range getFilterChains(l) {
			for _, f := range fc.GetFilters() {

				if f.GetTypedConfig().GetTypeUrl() == "type.googleapis.com/envoy.extensions.filters.network.http_connection_manager.v3.HttpConnectionManager" {

					hcm := &xds.HttpConnectionManager{}
					_ = proto.Unmarshal(f.GetTypedConfig().Value, hcm)
					if r := hcm.GetRds().GetRouteConfigName(); r != "" {
						routes = append(routes, r)
					}
					httpL[l.Name] = l
				} else {
					tcpL[l.Name] = l
				}
			}
		}
	}
	sort.Strings(routes)

	if dump {
		for i, l := range ll {
			b, err := marshal.MarshalToString(l)
			if err != nil {
				log.Printf("Error in LDS: %v", err)
			}

			log.Printf("lds %d: %v", i, b)
		}
	}

	a.mutex.Lock()
	a.HTTPListeners = httpL
	a.TCPListeners = tcpL
	a.mutex.Unlock()

	a.handleResourceUpdate(RouteType, routes)

	select {
	case a.Updates <- "lds":
	default:
	}
}

// Used for RDS and EDS (via LDS/CDS handlers) - if the list of resources to watch is different, re-ask.
func (a *ADSC) handleResourceUpdate(typeUrl string, resources []string) {
	if !listEqual(a.watches[typeUrl].resources, resources) {
		if dump {
			log.Printf("%v type resources changed: %v -> %v", typeUrl, a.watches[typeUrl].resources, resources)
		}
		watch := a.watches[typeUrl]
		watch.resources = resources
		a.watches[typeUrl] = watch
		a.request(typeUrl, watch)
	}
}

// listEqual checks that two lists contain all the same elements
func listEqual(a []string, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// compact representations, for simplified debugging/testing

// TCPListener extracts the core elements from envoy Listener.
type TCPListener struct {
	// Address is the address, as expected by go Dial and Listen, including port
	Address string

	// LogFile is the access log address for the listener
	LogFile string

	// Target is the destination cluster.
	Target string
}

type Target struct {

	// Address is a go address, extracted from the mangled cluster name.
	Address string

	// Endpoints are the resolved endpoints from EDS or cluster static.
	Endpoints map[string]Endpoint
}

type Endpoint struct {
	// Weight extracted from EDS
	Weight int
}

func (a *ADSC) handleCDS(ll map[string]*xds.Cluster) {
	cn := []string{}
	for _, c := range ll {
		if c.GetType() != xds.Cluster_EDS {
			continue
		}

		cn = append(cn, c.Name)
	}
	sort.Strings(cn)

	a.handleResourceUpdate(EndpointType, cn)

	if dump {
		for i, c := range ll {
			// jsonpb deprecated, use protojson instead
			//m := &protojson.MarshalOptions{
			//	EmitUnpopulated: false,
			//	UseProtoNames:   false,
			//	AllowPartial:    true,
			//}
			// Doesn't work with unknown protos.

			m := &prototext.MarshalOptions{
				AllowPartial: true,
				Indent:       " ",
				Multiline:    true,
			}

			b, err := m.Marshal(c)
			if err != nil {
				log.Printf("Error in CDS: %v", err)
			}

			log.Printf("cds %d: %v", i, string(b))
		}
	}

	a.mutex.Lock()
	defer a.mutex.Unlock()
	if !a.sendNodeMeta {
		// first load - Envoy loads listeners after endpoints
		_ = a.send(&xds.DiscoveryRequest{
			Node:    a.node,
			TypeUrl: ListenerType,
		}, ReasonInit)
		a.sendNodeMeta = true
	}

	select {
	case a.Updates <- "cds":
	default:
	}
}

func (a *ADSC) makeNode() *xds.Node {
	n := &xds.Node{
		Id: a.nodeID,
	}
	n.Locality = a.Config.Locality
	n.Metadata = &xds.Struct{Fields: map[string]*xds.Value{}}
	for k, v := range a.Metadata {
		if sv, ok := v.(string); ok {
			n.Metadata.Fields[k] = &xds.Value{Kind: &xds.Value_StringValue{sv}}
		}
	}
	//js, err := json.Marshal(a.Metadata)
	//if err != nil {
	//	panic("invalid metadata " + err.Error())
	//}
	//
	//meta := &structpb.Struct{}
	//err = jsonpb.UnmarshalString(string(js), meta)
	//if err != nil {
	//	panic("invalid metadata " + err.Error())
	//}
	//
	//n.Metadata = meta

	return n
}

func (a *ADSC) handleEDS(eds map[string]*xds.ClusterLoadAssignment) {
	if dump {
		for i, e := range eds {
			b, err := marshal.MarshalToString(e)
			if err != nil {
				log.Printf("Error in EDS: %v", err)
			}

			log.Printf("eds %d: %v", i, b)
		}
	}

	a.mutex.Lock()
	defer a.mutex.Unlock()

	select {
	case a.Updates <- "eds":
	default:
	}
}

func (a *ADSC) handleRDS(configurations map[string]*xds.RouteConfiguration) {
	rds := map[string]*xds.RouteConfiguration{}

	for _, r := range configurations {
		rds[r.Name] = r
	}

	if dump {
		for i, r := range configurations {
			b, err := marshal.MarshalToString(r)
			if err != nil {
				log.Printf("Error in RDS: %v", err)
			}

			log.Printf("rds %d: %v", i, b)
		}
	}

	select {
	case a.Updates <- "rds":
	default:
	}
}

// WaitClear will clear the waiting events, so next call to Wait will get
// the next push type.
func (a *ADSC) WaitClear() {
	for {
		select {
		case <-a.Updates:
		default:
			return
		}
	}
}

// Wait for an update of the specified type. If type is empty, wait for next update.
func (a *ADSC) Wait(update string, to time.Duration) (string, error) {
	t := time.NewTimer(to)

	for {
		select {
		case t := <-a.Updates:
			if len(update) == 0 || update == t {
				return t, nil
			}
		case <-t.C:
			return "", ErrTimeout
		}
	}
}

func (a *ADSC) Send(dr *xds.DiscoveryRequest) error {
	return a.send(dr, ReasonRequest)
}

var Debug = false

func (a *ADSC) send(dr *xds.DiscoveryRequest, reason string) error {
	dr.ResponseNonce = time.Now().String()

	if dump {
		log.Printf("send message for type %v (%v) for %v", dr.TypeUrl, reason, dr.ResourceNames)
	}

	err := a.stream.SendMsg(dr)

	a.sendCount++
	return err
}

const (
	ReasonAck     = "ack"
	ReasonRequest = "request"
	ReasonInit    = "init"
)

func (a *ADSC) request(typeUrl string, watch Watch) {
	_ = a.send(&xds.DiscoveryRequest{
		ResponseNonce: watch.lastNonce,
		TypeUrl:       typeUrl,
		Node:          a.node,
		VersionInfo:   watch.lastVersion,
		ResourceNames: watch.resources,
	}, ReasonRequest)
}

func (a *ADSC) ack(msg *xds.DiscoveryResponse, names []string) {
	watch := a.watches[msg.TypeUrl]
	watch.lastNonce = msg.Nonce
	watch.lastVersion = msg.VersionInfo
	a.watches[msg.TypeUrl] = watch

	// Incremental EDS can send partial endpoints, but in ack envoy should ack for all.
	if msg.TypeUrl == EndpointType {
		names = watch.resources
	}
	_ = a.send(&xds.DiscoveryRequest{
		ResponseNonce: msg.Nonce,
		TypeUrl:       msg.TypeUrl,
		Node:          a.node,
		VersionInfo:   msg.VersionInfo,
		ResourceNames: names,
	}, ReasonAck)
}
