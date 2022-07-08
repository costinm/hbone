
# HTTP/2

HBone is based on http/2 and h3 protocol - which at the low level
are implemented as streams multiplexed using Frames. Any protocol
implementation will receive (encrypted) packets of data from the OS,
decrypt to a new set of (recycled) buffers holding a set of frames.

To minimize copy and alloc, ideally the receive/send buffers would
be used directly, and the high level implementation use slices
of the send/receive buffers as frames. This would likely require
reimplementing http/2 - or possibly using fast http2.

Next best thing is to use the http2 Framer - with SetReuseFrame
option set (Frame only valid until next call to ReadFrame).
This also requires a large effort to re-implement the flow control.

The next option is to use the low-level http ClientConn - this is what
initial implementation is using. It provides access to streams, with
the inefficient Read/Write interfaces.

Long term I want to explore the better options - and to end up with
an efficient interface. Even with Read/Write, it is possible to
reuse buffer and expose frames as first class - which in an optimized
implementation would avoid copy/alloc used by ClientConn.

http2.Framer defines a set of frames - DataFrame, HeadersFrame, etc - with
a common FrameHeader (Type, Flags, Length, StreamID). Main interface is
ReadFrame() (Frame, error) - may return the reused frame - and
WriteRawFrame(t FrameType, flags Flags, streamID uint32, payload []byte) error
plus helpers for the common frames. The framer has a wbuf where bytes are appended.
The wbuf is not exposed - so a copy must be made. For ReadFrame, 2 read operations
are made per frame - one for the fixed header and one for the rest in readBuf (or allocated
buf if readBuf is too small). lastFrame is held too.

The tricky part is flow control - it is implemented in clientConn by a Cond.Broadcast, which
blocks writeRequestBody for client. On server side it implements scheduleFrameWrite() which may
start or restart the frame writer. 

## FastHttp http/2

The main problem with FastHttp h2 is the lack of streaming - the Write loop in conn calls
writeRequest as result of Write(ctx). 

The FrameHeader supports ReadFrom(bufio.Reader) - using Peek, Discard from bufio.Reader,
which are exposing the read buffer directly, however the frame is using ReadFull, which is 
copying the data, even if it doesn't allocate. WriteTo is done using 2 Write operations.

## GRPC

For similar perf reasons gRPC is using a low-leve stack, in internal/transport. 
It is reusing http2.Framer - supplemented with a batch writer to reduce the Write, at 
the cost of more copy. This improves wire efficiency to some extent.

On read, it sets SetReuseFrames - but the data frame is copied to a pooled
buffer.

