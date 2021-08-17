# HTTP Based Overlay Network Environment

There are already many VPN, VPC and tunneling protocols in broad use, 
using UDP, custom TCP protocols, SSH or even HTTP and Websocket.

This package attempts to implement a minimal mesh overlay network
based on mTLS and compatible with existing HTTP infrastructure. It will
mirror Istio implementation and model - but with few extra features to 
work with non-Istio load balancers and infra.

The core protocol is very simple and can be implemented in any language
and part of almost any framework - we hope that core gRPC libraries and 
other proxies will support it natively, and multiple implementations will
be available. 

## Goals

- mTLS based identity and end to end security between workloads
- support metadata
- compatible with plain/standard gateways and load balancers, with core HTTP/2 support

## Protocol

1. TCP streams are sent as HTTP CONNECT requests. 
2. For compatibility with existing infra, POST is also supported as equivalent to 
   CONNECT. A POST request will have the prefix /_hbone/, followed by an address. 
   The request will be treated the same as a CONNECT with same address.

### Plain text (TCP)

### mTLS over H2

## SNI routing

This is compatible with Istio East-West gateway, accepting without handshake the mTLS connections on 15443 and 
using the ClientHello info to find the ServerName (SNI). The Istio clients should treat it as any regular Istio
gateway. 

The HBONE SNI gateway will forward the mTLS connection using mtls-over-H2 to an external address, including 
JWT authentication if needed. 

## H2R - Reverse connections support (remote accept)




# Execution environment

# CLI



# TODO

## P1 
[] Docker and helm
[] Callbacks for events, 
[] hook to k8s slice
[] Timeouts/deadlines/keepalives
[] Move to separate git repo

## P2
[] convert sshd to use h2r (still over mTLS, but not exposed on the public address)
