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

var (
	VarzReadFrom  = expvar.NewInt("nio.sReadFrom")
	VarzReadFromC = expvar.NewInt("nio.cReadFrom")
	VarzMaxRead   = expvar.NewInt("nio.maxRead")
)
