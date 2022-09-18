Forked from golang http2 - this is the only dependency between gRPC transport and http2, no need to have 2 
http/2 stacks linked in.

WIP: optimize the framing and serialization to reduce mem copy and allocs.
