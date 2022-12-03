package h2

// WebTransport implements a QUIC multiplexed transport over a H2 (or other) stream.

// UDP flow control is based on dropping packets on congestion - sender should
// drop packets if out of flow control. TCP hides traffic dropped by the net, but
// will still slow down due to re-transmits.

// It effectively implements QUIC framing over the stream - this implementation attempts
// to combine the framing for gRPC and QUIC. Both have a TLV format, with gRPC using
// 0x00 [4]Len, and Quic using 0xnn VarintLen.
// Most useful is the datagram 0x31 - for forward streams regular CONNECT streams seem
// better suited and easier ( but may implement for compat ), and for reverse the reverse
// H2 seems also easier.

// It seems a bit overkill for streams - compared to using a CONNECT for each stream, it
// means double flow control and complexity.

// WIP

type WebTransport struct {
}
