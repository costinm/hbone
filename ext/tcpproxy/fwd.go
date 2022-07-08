package tcpproxy

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/url"
	"strings"

	"github.com/costinm/hbone"
)

func Forward(dest string, hb *hbone.HBone, hg string, auth hbone.Auth, in io.Reader, out io.WriteCloser) error {
	host := ""
	if strings.Contains(dest, "//") {
		u, _ := url.Parse(dest)

		host, _, _ = net.SplitHostPort(u.Host)
	} else {
		host, _, _ = net.SplitHostPort(dest)
	}
	// TODO: k8s discovery for hgate
	// TODO: -R to register to the gate, reverse proxy
	// TODO: get certs

	hc := hb.NewEndpointCon(dest)

	if strings.HasSuffix(host, ".svc") {
		hc.H2Gate = hg + ":15008" // hbone/mtls
		hc.ExternalMTLSConfig = auth.GenerateTLSConfigServer()
	}
	// Initialization done - starting the proxy either on a listener or stdin.

	err := hc.Proxy(context.Background(), in, out)
	if err != nil {
		return err
	}
	return nil
}

// WIP: RemoteForwardPort is similar with ssh -R remotePort. Will use the H2R protocol to open a remote H2C connection
// attached to the Hbone remote server.
func RemoteForwardPort(port string, hb *hbone.HBone, hg string, sn, ns string) {
	attachC := &hbone.Cluster{
		Addr: sn + "." + ns + ":15009",
	}
	hb.AddCluster(attachC.Addr, attachC)
	attachE := attachC.NewEndpoint("")
	attachE.SNI = fmt.Sprintf("outbound_.%s_._.%s.%s.svc.cluster.local", port, sn, ns)
	go func() {
		_, err := attachE.DialH2R(context.Background(), hg)
		log.Println("H2R connected", hg, err)
	}()
}

// LocalForwardPort is a helper for port forwarding, similar with -L localPort:dest:destPort
//
func LocalForwardPort(localAddr, dest string, hb *hbone.HBone, hg string, auth hbone.Auth) error {
	l, err := net.Listen("tcp", localAddr)
	if err != nil {
		return err
	}

	for {
		a, err := l.Accept()
		if err != nil {
			log.Println("Error accepting", err)
			return err
		}
		go func() {
			Forward(dest, hb, hg, auth, a, a)
		}()
	}
}
