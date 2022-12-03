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

# TODO

- test with 1M frame size instead of default 16k
- remove the use of the pipe for http request
- use Stream as net.Conn
- P0 implement deadlines properly, use an idle timer for streams too
- Read() seems to return one frame at a time - despite having more info. Should also report buffered in/iot

# Internals of gRPC stack

## API

- ServerTransport is an interface - gRPC allows plugging different implemenation, so hbone could
be one.

- HandleStreams() callbacks on stream received - returns after handshake !
- WriteHeader(s), Write(...), WriteStatus(), Close(), Drain(), IncrMsgSend/Recv 
- 'channelzData' has stats for the stream.

- Context includes the Stream ( with interface exposing the Method, SetHeader, SendHeader, SetTrailer)


Client:
- Close/GracefulClose for the connection
- Write(stream, 2 byte[], last)
- NewStrem() - sends headers, doesn't wait
- CloseStream() - must be called when stream is finished.
- Error() ch -> closed on connection error
- GoAway() ch for graceful connection close
- 

## Removed / fixed

- grpc specific headers
- auth - using http layer
- the h in data frames
- context, MD - since exposing the Stream directly.
- Write - was not blocking on write, since byte[] was result of marshal. Also no 
reuse
- Read - io semantics, partial reads instead of readfull.


## Code

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

# H2 notes - from H3 perspective

- At start, 64K flow window for both stream and connection. 
- receiver can increase both using WINDOW_UPDATE, cleaner than SETTINGS
- adjusting frame size doesn't seem very useful, increases bench but may be more harmful
  default is 16k

H2 frame header is 9B, QUIC is varint tag,len

QUIC 
- stream ID second bit is 2Way or 1Way
- Sender: STREAM, STREAM_DATA_BLOCKED, RESET_STREAM
- Receiver: MAX_STREAM_DATA, STOP_SENDING
- Also MAX_DATA for connection

Packets include a set of frames
Connections may migrate or use multiple paths - based on connection ID.
