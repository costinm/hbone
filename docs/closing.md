# Server-first closing

The golang HTTP stack behavior prevents a server from closing the write stream before the client has finished.

The 'FIN' is sent after the 'Handler' method has finished. It is likely other languages and stacks have similar
behavior.

One option is to implement a raw, low-level H2 framer - this is possible, but would make it difficult to integrate with
existing applications for 'proxyless' behavior.

The second options is to use some framing. Each data chunk in the HTTP stream will have a prefix - including the length
of the data frame, and a type to differentiate data and 'close'.

Instead of an arbitrary framing, we can use an existing format - like gRPC stream:

Regular data frames will be encoded as:

- '0'
- 4 bytes frame length
- 0x02 - equivalent with tag=0, type=2 (bytes)
- DATA

Close will be encoded as:

- '0'
- 4 bytes length - 00 00 00 02
- 0x08 - tag=1, type=1 (varint)
- 0 for FIN, 1 for RST (error close)
- (addional data may follow)

A third option is to fork the http2 implmentation and add a CloseWrite call, which is already a TODO at the top of
server.go.
