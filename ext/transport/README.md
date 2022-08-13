Fork of grpc transport, for use with HBone.

# Rationale

The golang H2 stack is optimized for the http use case - 'ease of use', consistency with the golang IO and http model.
It is not optimized for performance or uses of H2 as a generic transport or tunneling.

This is the reason gRPC has its own stack - optimized for gRPC use case and performance.

There are few options:
- fork and modify the golang h2 stack
- fork and modify the gRPC h2 transport - this module
- fork the FastHTTP H2 implementation - initial evaluation is that gRPC stack has far more usage/maturity, but it may
change, FastHTTP is pretty perf-oriented.

The changes made to gRPC are around few goals:
- remove gRPC specific functionality, which shouldn't be in the h2 transport
- remove mTLS related code - it should be handled in the L4 layer and generic
- explore options for further optimizations and reuse.

Ideally the framer can also be optimized and an event based, non-blocking low level can be added. 

# Internals

- bpdEstimator - evaluate bandwidth ( Bandwidth * Delay product), for dynamicWindow (unless InitialConnWindowSize is set),
updates the window and sends SettingsInitialWindowSize dynamically.
- controlbuf - loopyWriter, buffer logic for sending
- flowcontrol - in and out flow control handling. When app reads data it sends window updates.
- handler_server - used for adapting the H2 handler to gRPC. Not used, removed.
Handles grpc-timeout, content-type, meta, decodes binary headers.
- http_util - deal with grpc headers, newFramer using a bufWriter - wraps http2.NewFramer.
- proxy - authenticate with basicAuth with proxies, implement CONNECT 1.1. Not used.
- transport - bufferPool, recvMsg, recvBuffer and reader. Interfaces - Stream, transportReader. 

Other changes:
- Removed channelz, statsHandler
- removed TLS/auth related code.

Client code: 
1. Initialization - replacing dial with a net.Conn from L4

The http2 core implementation:
- has a databuffer - pool of chunks of different sizes
- client conn pool handling for http
- supports priorities - not used in hbone
