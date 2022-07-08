# gRPC support

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
