package handlers

import (
	"log"
	"net"
	"strings"

	"github.com/costinm/hbone"
	"github.com/costinm/hbone/nio"
)

// HandleSNIConn implements SNI based routing. This can be used for compat
// with Istio. Was original method to tunnel for serverless.
//
// This can be used for a legacy CNI to HBone bridge. The old Istio client expects an mTLS connection
// to the other end - the HBone proxy is untrusted.
func HandleSNIConn(hb *hbone.HBone, conn net.Conn) {
	s := nio.NewBufferReader(conn)
	defer conn.Close()
	defer s.Buffer.Recycle()

	sni, err := nio.ParseTLS(s)
	if err != nil {
		log.Println("SNI invalid TLS", sni, err)
		return
	}

	// At this point we have a SNI service name. Need to convert it to a real service
	// name, RoundTripStart and proxy.

	addr := sni + ":443"
	// Based on SNI, make a hbone request, using JWT auth.
	if strings.HasPrefix(sni, "outbound_.") {
		// Current Istio SNI looks like:
		//
		// outbound_.9090_._.prometheus-1-prometheus.mon.svc.cluster.local
		// We need to map it to a cloudrun external address, add token based on the audience, and
		// make the call using the tunnel.
		//
		// Also supports the 'natural' form and egress

		//
		//
		parts := strings.SplitN(sni, ".", 4)
		remoteService := parts[3]
		// TODO: extract 'version' from URL, convert it to cloudrun revision ?
		addr = net.JoinHostPort(remoteService, parts[1])
	}

	nc, err := hb.Dial("tcp", addr)
	if err != nil {
		log.Println("Error connecting ", err)
		return
	}
	err = hbone.Proxy(nc, s, conn, addr)
	if err != nil {
		log.Println("Error proxy ", err)
		return
	}
}
