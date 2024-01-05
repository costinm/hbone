# HTTP/2 attempted optimizations

This package contains an in-progress (but extremely slow) attempt to improve the Golang H2 stack.

It is a fork for grpc stack, with few extensions around using the protocol as a transport instead of strictly HTTP stack, and without the compatibility requirements with HTTP/1.1 APIs.

At the moment I believe for most use cases SSH is a better secure L4 transport for small devices / on prem and most of the cloud - since almost everything has SSH already. For high throughput and
low latency - native support for HTTP with mTLS or H3, WebRTC, etc is a better option.

Making small changes to native applications or libraries seems more long-term efficient.

SSH (or any capture-based automated transport security) is good enough for most operations - it is 
commonly used for rsync and filesystems - and it can be a good signaling and automation protocol to
bootsrap/simplify the use of native protocols.

-----------------------
Old content: 

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

