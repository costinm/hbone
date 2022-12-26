# HTTP Based Overlay Network Environment

There are already many VPN, VPC and tunneling protocols in broad use, using UDP, custom TCP protocols, SSH or even HTTP
and Websocket.

This package attempts to implement a minimal mesh overlay network based on mTLS and compatible with existing HTTP
infrastructure. It will mirror Istio implementation and model - but with few extra features to work with non-Istio load
balancers and infra.

The core protocol is very simple and can be implemented in any language and part of almost any framework - we hope that
core gRPC libraries and other proxies will support it natively, and multiple implementations will be available.

## Goals

- mTLS based identity and end to end security between workloads
- zero trust - all middle boxes will be mutually authenticated and have independent policies
- support metadata - 'baggage' header, source/destination info
- compatible with plain/standard gateways and load balancers with core HTTP/2 support

The repository contains a library that can be used in go application using
the protocol natively, and a server that can be used as:

- uProxy with TPROXY, 'whitebox', SOCKS capture
- minimal Egress or East-West gateway
- minimal Ingress gateway

## Protocol

### Basic CONNECT mode

All TCP streams are sent as HTTP/2 CONNECT requests, with few optional extra headers.

Both ends are expected to use workload identity certificates (optional DNS),
using mTLS to authenticate.

By default, only 'same trust domain and namespace' communication is allowed. The API/config
allows custom policies.

When used as an Ingress - the gateway terminates TCP, HTTP, HTTPS, TLS
connections, applies policies and forwards to workloads using CONNECT.

When used as Egress, the gateway terminates mTLS CONNECT, applies policies
and forwards to the destination, optionally adding TLS. 

It can also be used as a PEP or 'policy enforcing' East-West gateway. 

In all 'middle box' cases, the gateway has access to the original plain text
data and may apply policies or modify it.


### mTLS over JWT-authenticated POST/CONNECT

For compatibility with existing infra, POST is also supported as equivalent to CONNECT. 

This mode is enabled using 'tun' label on an endpoint, with value 'POST' or 'CONNECT'.

The original stream from the user will be end-to-end encrypted and authenticated 
using mTLS. 

The proxy infrastructure is expected to handle the client-to-proxy authentication
and forward the inner stream as a HTTP/2 connection. The ":authority" header
will be set as expected to the hostname of the proxy. 

A new 'X-tun' header will include the original address - sent as :authority 
in the basic CONNECT mode. 

### Legacy

TODO:
- regular HTTP CONNECT proxy support

## SNI routing

This is compatible with Istio East-West gateway, accepting without handshake the mTLS connections on 15443 and using the
ClientHello info to find the ServerName (SNI). The old Istio clients should treat it as any regular Istio gateway.

The HBONE SNI gateway will forward the mTLS connection using mtls-over-H2 to an external address, including JWT
authentication if needed.

## H2R - Reverse connections support (remote accept)

# Execution environment

# CLI

# TODO

## P1

- [ ] Docker and helm
- [ ] Callbacks for events,
- [ ] hook to k8s slice
- [ ] Timeouts/deadlines/keepalives
- [ ] Move to separate git repo

## P2

- [ ] convert sshd to use h2r (still over mTLS, but not exposed on the public address)

# uREST 

It is extremely common for applications to make REST or gRPC requests - this 
package is focused on implementing the mesh and HBONE protocols, and provides 
some minimal support for gRPC and K8S calls without extra dependencies and using
the same code.

It is based on/inspired from kelseyhightower/konfig - which uses the 'raw' 
K8S protocol to avoid a big dependency. Google, K8S and many other APIs have
a raw representation and don't require complex client libraries and depdencies.
No code generation or protos are used - raw JSON in []byte is used, caller 
can handle marshalling.

For gRPC only the basic framing and protocol are support - exposed as frames
containing []byte. Caller or generated code can handle marshalling.

This is intended for apps that make few small requests and want to keep 
size small, or want to take advantage of HBONE and mesh auto-setup. Also
for low-level testing.

# Other projects

- https://github.com/xnuter/http-tunnel
- IPFS / libP2P - one of the supported transports is a modified H2, also Quic. Reinvents cert format.
- Syncthing - reverse tunnels, custom protocol, certs
- Tor - of course.
- BitTorrent
- [Konectivity](https://github.com/kubernetes-sigs/apiserver-network-proxy.git)
  Narrow use case of 'reverse connections' for Nodes (agents) getting calls from the APIserver via proxy, when the
  APIserver doesn't have direct connectivity to the node.

  gRPC or HTTP CONNECT based on the agent-proxy connection, and gRPC for APIserver to proxy.

   ``` 
   service AgentService {
     // Agent Identifier?
     rpc Connect(stream Packet) returns (stream Packet) {}
   }
   service ProxyService {
     rpc Proxy(stream Packet) returns (stream Packet) {}
   }
   
   Packet can be Data, Dial Req/Res, Close Req/Res
   ```

# Gost

[gost](https://github.com/ginuerzh/gost/blob/master/README_en.md) provides multiple integration points, focuses on
similar TCP proxy modes.

Usage:

```shell

# socks+http proxy
gost -L=:8080


```

# gRPC framing support

HBone provides a low-level H2-based overlay network. The gRPC protocol
defines a framing (1 byte TAG 4 byte LEN) and a set of headers. This library
does not make a distinction between unary or stream - just like http library
doesn't make distinction between HEAD, GET and POST, input/output are a
stream of zero, 1 or more frames.

To integrate with XDS servers we need at least minimal protobuf support. The micro XDS
implementation is based on HBone raw protocol, but we need to understand a minimal
set of config options.

Taking a full dependency on gRPC is another option - it may allow using
the optimized H2 stack by tunneling over gRPC, still H2 but with the extra
5-byte framing.

The minimal gRPC implementation supports the basic protocol - without
generated stubs or any high level feature. It is intended for proxy
and sniffing - for future support for GRPCRoute and content filtering.
Both proto and byte frames are suported.

## Codec

At low level, gRPC frames are parsed as Buffers, using the nio model to
minimize copy. This is useful for proxy or filtering without unmarshal.

For most practical uses gRPC needs translation to protos - this is done
automatically to avoid buffer allocations. "google.golang.org/protobuf/prot"
library is used - the old one is deprecated ( but still a dependency since
the new library is using some). 
