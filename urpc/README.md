# uRPC

This is a low-dependencies RPC, using gRPC binary protocol and compatible with
gRPC clients and servers - but not with the stubs. The package also includes
low-level interface with K8S - just enough to support mesh implementations 
using XDS protocol.

## uXDS

uXDS implements a minimal XDS, using the raw H2 stack and protocol to keep 
size small.

It includes other protocols used to bootstrap the mesh, using same mechanism.
