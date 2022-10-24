package k8s

// Gateway describes a load balancer operating at the edge of the mesh
// receiving incoming or outgoing HTTP/TCP connections.
//
// <!-- crd generation tags
// +cue-gen:Gateway:groupName:networking.istio.io
// +cue-gen:Gateway:version:v1beta1
// +cue-gen:Gateway:annotations:helm.sh/resource-policy=keep
// +cue-gen:Gateway:labels:app=istio-pilot,chart=istio,heritage=Tiller,release=istio
// +cue-gen:Gateway:subresource:status
// +cue-gen:Gateway:scope:Namespaced
// +cue-gen:Gateway:resource:categories=istio-io,networking-istio-io,shortNames=gw
// +cue-gen:Gateway:preserveUnknownFields:false
// -->
//
// <!-- go code generation tags
// +kubetype-gen
// +kubetype-gen:groupVersion=networking.istio.io/v1beta1
// +genclient
// +k8s:deepcopy-gen=true
// -->
type IstioGatewayCR struct {
	TypeMeta `json:",inline"`
	// +optional
	ObjectMeta `json:"metadata,omitempty" protobuf:"bytes,1,opt,name=metadata"`

	// Spec defines the implementation of this definition.
	// +optional
	Spec IstioGateway `json:"spec,omitempty" protobuf:"bytes,2,opt,name=spec"`

	//Status v1alpha1.IstioStatus `json:"status"`
}

type IstioGateway struct {
	// A list of server specifications.
	Servers []*Server `protobuf:"bytes,1,rep,name=servers,proto3" json:"servers,omitempty"`
	// One or more labels that indicate a specific set of pods/VMs
	// on which this gateway configuration should be applied.
	// By default workloads are searched across all namespaces based on label selectors.
	// This implies that a gateway resource in the namespace "foo" can select pods in
	// the namespace "bar" based on labels.
	// This behavior can be controlled via the `PILOT_SCOPE_GATEWAY_TO_NAMESPACE`
	// environment variable in istiod. If this variable is set
	// to true, the scope of label search is restricted to the configuration
	// namespace in which the the resource is present. In other words, the Gateway
	// resource must reside in the same namespace as the gateway workload
	// instance.
	// If selector is nil, the Gateway will be applied to all workloads.
	Selector map[string]string `protobuf:"bytes,2,rep,name=selector,proto3" json:"selector,omitempty" protobuf_key:"bytes,1,opt,name=key,proto3" protobuf_val:"bytes,2,opt,name=value,proto3"`
	//XXX_NoUnkeyedLiteral struct{}          `json:"-"`
	//XXX_unrecognized     []byte            `json:"-"`
	//XXX_sizecache        int32             `json:"-"`
}

type Server struct {
	// The Port on which the proxy should listen for incoming
	// connections.
	Port *Port `protobuf:"bytes,1,opt,name=port,proto3" json:"port,omitempty"`
	// $hide_from_docs
	// The ip or the Unix domain socket to which the listener should be bound
	// to. Format: `x.x.x.x` or `unix:///path/to/uds` or `unix://@foobar`
	// (Linux abstract namespace). When using Unix domain sockets, the port
	// number should be 0.
	Bind string `protobuf:"bytes,4,opt,name=bind,proto3" json:"bind,omitempty"`
	// One or more hosts exposed by this gateway.
	// While typically applicable to
	// HTTP services, it can also be used for TCP services using TLS with SNI.
	// A host is specified as a `dnsName` with an optional `namespace/` prefix.
	// The `dnsName` should be specified using FQDN format, optionally including
	// a wildcard character in the left-most component (e.g.,
	// `prod/*.example.com`). Set the `dnsName` to `*` to select all
	// `VirtualService` hosts from the specified namespace (e.g.,`prod/*`).
	//
	// The `namespace` can be set to `*` or `.`, representing any or the current
	// namespace, respectively. For example, `*/foo.example.com` selects the
	// service from any available namespace while `./foo.example.com` only selects
	// the service from the namespace of the sidecar. The default, if no
	// `namespace/` is specified, is `*/`, that is, select services from any
	// namespace. Any associated `DestinationRule` in the selected namespace will
	// also be used.
	//
	// A `VirtualService` must be bound to the gateway and must have one or
	// more hosts that match the hosts specified in a server. The match
	// could be an exact match or a suffix match with the server's hosts. For
	// example, if the server's hosts specifies `*.example.com`, a
	// `VirtualService` with hosts `dev.example.com` or `prod.example.com` will
	// match. However, a `VirtualService` with host `example.com` or
	// `newexample.com` will not match.
	//
	// NOTE: Only virtual services exported to the gateway's namespace
	// (e.g., `exportTo` value of `*`) can be referenced.
	// Private configurations (e.g., `exportTo` set to `.`) will not be
	// available. Refer to the `exportTo` setting in `VirtualService`,
	// `DestinationRule`, and `ServiceEntry` configurations for details.
	Hosts []string `protobuf:"bytes,2,rep,name=hosts,proto3" json:"hosts,omitempty"`
	// Set of TLS related options that govern the server's behavior. Use
	// these options to control if all http requests should be redirected to
	// https, and the TLS modes to use.
	Tls *ServerTLSSettings `protobuf:"bytes,3,opt,name=tls,proto3" json:"tls,omitempty"`
	// The loopback IP endpoint or Unix domain socket to which traffic should
	// be forwarded to by default. Format should be `127.0.0.1:PORT` or
	// `unix:///path/to/socket` or `unix://@foobar` (Linux abstract namespace).
	// NOT IMPLEMENTED.
	// $hide_from_docs
	DefaultEndpoint string `protobuf:"bytes,5,opt,name=default_endpoint,json=defaultEndpoint,proto3" json:"default_endpoint,omitempty"`
	// An optional name of the server, when set must be unique across all servers.
	// This will be used for variety of purposes like prefixing stats generated with
	// this name etc.
	Name                 string   `protobuf:"bytes,6,opt,name=name,proto3" json:"name,omitempty"`
	XXX_NoUnkeyedLiteral struct{} `json:"-"`
	XXX_unrecognized     []byte   `json:"-"`
	XXX_sizecache        int32    `json:"-"`
}

type ServerTLSSettings struct {
	// If set to true, the load balancer will send a 301 redirect for
	// all http connections, asking the clients to use HTTPS.
	HttpsRedirect bool `protobuf:"varint,1,opt,name=https_redirect,json=httpsRedirect,proto3" json:"https_redirect,omitempty"`
	// Optional: Indicates whether connections to this port should be
	// secured using TLS. The value of this field determines how TLS is
	// enforced.
	Mode string `protobuf:"varint,2,opt,name=mode,proto3,enum=istio.networking.v1beta1.ServerTLSSettings_TLSmode" json:"mode,omitempty"`
	// REQUIRED if mode is `SIMPLE` or `MUTUAL`. The path to the file
	// holding the server-side TLS certificate to use.
	ServerCertificate string `protobuf:"bytes,3,opt,name=server_certificate,json=serverCertificate,proto3" json:"server_certificate,omitempty"`
	// REQUIRED if mode is `SIMPLE` or `MUTUAL`. The path to the file
	// holding the server's private key.
	PrivateKey string `protobuf:"bytes,4,opt,name=private_key,json=privateKey,proto3" json:"private_key,omitempty"`
	// REQUIRED if mode is `MUTUAL`. The path to a file containing
	// certificate authority certificates to use in verifying a presented
	// client side certificate.
	CaCertificates string `protobuf:"bytes,5,opt,name=ca_certificates,json=caCertificates,proto3" json:"ca_certificates,omitempty"`
	// For gateways running on Kubernetes, the name of the secret that
	// holds the TLS certs including the CA certificates. Applicable
	// only on Kubernetes, and only if the dynamic credential fetching
	// feature is enabled in the proxy by setting
	// `ISTIO_META_USER_SDS` metadata variable.  The secret (of type
	// `generic`) should contain the following keys and values: `key:
	// <privateKey>` and `cert: <serverCert>`. For mutual TLS,
	// `cacert: <CACertificate>` can be provided in the same secret or
	// a separate secret named `<secret>-cacert`.
	// Secret of type tls for server certificates along with
	// ca.crt key for CA certificates is also supported.
	// Only one of server certificates and CA certificate
	// or credentialName can be specified.
	CredentialName string `protobuf:"bytes,10,opt,name=credential_name,json=credentialName,proto3" json:"credential_name,omitempty"`
	// A list of alternate names to verify the subject identity in the
	// certificate presented by the client.
	SubjectAltNames []string `protobuf:"bytes,6,rep,name=subject_alt_names,json=subjectAltNames,proto3" json:"subject_alt_names,omitempty"`
	// An optional list of base64-encoded SHA-256 hashes of the SKPIs of
	// authorized client certificates.
	// Note: When both verify_certificate_hash and verify_certificate_spki
	// are specified, a hash matching either value will result in the
	// certificate being accepted.
	VerifyCertificateSpki []string `protobuf:"bytes,11,rep,name=verify_certificate_spki,json=verifyCertificateSpki,proto3" json:"verify_certificate_spki,omitempty"`
	// An optional list of hex-encoded SHA-256 hashes of the
	// authorized client certificates. Both simple and colon separated
	// formats are acceptable.
	// Note: When both verify_certificate_hash and verify_certificate_spki
	// are specified, a hash matching either value will result in the
	// certificate being accepted.
	VerifyCertificateHash []string `protobuf:"bytes,12,rep,name=verify_certificate_hash,json=verifyCertificateHash,proto3" json:"verify_certificate_hash,omitempty"`
	// Optional: Minimum TLS protocol version.
	MinProtocolVersion string `protobuf:"varint,7,opt,name=min_protocol_version,json=minProtocolVersion,proto3,enum=istio.networking.v1beta1.ServerTLSSettings_TLSProtocol" json:"min_protocol_version,omitempty"`
	// Optional: Maximum TLS protocol version.
	MaxProtocolVersion string `protobuf:"varint,8,opt,name=max_protocol_version,json=maxProtocolVersion,proto3,enum=istio.networking.v1beta1.ServerTLSSettings_TLSProtocol" json:"max_protocol_version,omitempty"`
	// Optional: If specified, only support the specified cipher list.
	// Otherwise default to the default cipher list supported by Envoy.
	CipherSuites         []string `protobuf:"bytes,9,rep,name=cipher_suites,json=cipherSuites,proto3" json:"cipher_suites,omitempty"`
	XXX_NoUnkeyedLiteral struct{} `json:"-"`
	XXX_unrecognized     []byte   `json:"-"`
	XXX_sizecache        int32    `json:"-"`
}

// Port describes the properties of a specific port of a service.
type Port struct {
	// A valid non-negative integer port number.
	Number uint32 `protobuf:"varint,1,opt,name=number,proto3" json:"number,omitempty"`
	// The protocol exposed on the port.
	// MUST BE one of HTTP|HTTPS|GRPC|HTTP2|MONGO|TCP|TLS.
	// TLS implies the connection will be routed based on the SNI header to
	// the destination without terminating the TLS connection.
	Protocol string `protobuf:"bytes,2,opt,name=protocol,proto3" json:"protocol,omitempty"`
	// Label assigned to the port.
	Name string `protobuf:"bytes,3,opt,name=name,proto3" json:"name,omitempty"`
	// The port number on the endpoint where the traffic will be
	// received. Applicable only when used with ServiceEntries.
	TargetPort           uint32   `protobuf:"varint,4,opt,name=target_port,json=targetPort,proto3" json:"target_port,omitempty"`
	XXX_NoUnkeyedLiteral struct{} `json:"-"`
	XXX_unrecognized     []byte   `json:"-"`
	XXX_sizecache        int32    `json:"-"`
}
