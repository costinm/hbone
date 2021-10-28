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

func Forward(dest string, hb *hbone.HBone, hg string, auth *hbone.Auth, in io.Reader, out io.WriteCloser) error {
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

	hc := hb.NewEndpoint(dest)

	if strings.HasSuffix(host, ".svc") {
		hc.H2Gate = hg + ":15008" // hbone/mtls
		hc.ExternalMTLSConfig = auth.MeshTLSConfig
	}
	// Initialization done - starting the proxy either on a listener or stdin.

	err := hc.Proxy(context.Background(), in, out)
	if err != nil {
		return err
	}
	return nil
}

func RemoteForwardPort(port string, hb *hbone.HBone, hg string, sn, ns string) {
	attachC := hb.NewClient(sn + "." + ns + ":15009")
	attachE := attachC.NewEndpoint("")
	attachE.SNI = fmt.Sprintf("outbound_.%s_._.%s.%s.svc.cluster.local", port, sn, ns)
	go func() {
		_, err := attachE.DialH2R(context.Background(), hg)
		log.Println("H2R connected", hg, err)
	}()
}

func LocalForwardPort(lf, dest string,hb *hbone.HBone, hg string, auth *hbone.Auth) error {

	l, err := net.Listen("tcp", lf)
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

