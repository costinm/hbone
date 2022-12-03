package nio

import (
	"expvar"
	"time"
)

// Statistics for streams and hosts
type Stats struct {
	Open time.Time

	// last receive from local (and send to remote)
	LastWrite time.Time

	// last receive from remote (and send to local)
	LastRead time.Time

	// Sent from client to server ( client is initiator of the proxy )
	SentBytes   int
	SentPackets int

	// Received from server to client
	RcvdBytes   int
	RcvdPackets int
}

var StreamId uint32

// Keyed by Hostname:port (if found in dns tables) or IP:port
type HostStats struct {
	// First open
	Open time.Time

	// Last usage
	Last time.Time

	SentBytes   int
	RcvdBytes   int
	SentPackets int
	RcvdPackets int

	Count int
}

// Varz interface.
// Varz is a wrapper for atomic operation, with a json http interface.
// Prometheus, OTel etc can directly use them.
var (
	// Number of copy operations using slice.
	VarzReadFromC = expvar.NewInt("io_copy_slice_total")
)
