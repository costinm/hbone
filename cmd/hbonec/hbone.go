package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"os"

	"github.com/costinm/hbone"
)

var (
	port = flag.String("l", "", "local port")
	tls  = flag.String("tls", "", "Cert dir for mTLS over hbone")
)

// Create a HBONE tunnel to a service.
//
// Will attempt to discover an east-west gateway and get credentials using KUBE_CONFIG or google credentials.
//
// For example:
//
// ssh -o ProxyCommand='hbone %h:22' root@fortio-cr.fortio
//
// If the server doesn't have persistent SSH key, add to the ssh parameters:
//      -F /dev/null -o StrictHostKeyChecking=no -o "UserKnownHostsFile /dev/null"
//
func main() {
	flag.Parse()

	if len(flag.Args()) == 0 {
		log.Fatal("Expecting URL or host:port")
	}
	url := flag.Arg(0)

	// TODO: k8s discovery for hgate
	// TODO: -R to register to the gate, reverse proxy
	// TODO: get certs

	hb := &hbone.HBone{}
	hc := hb.NewEndpoint(url)

	if *port != "" {
		fmt.Println("Listening on ", *port, " for ", url)
		l, err := net.Listen("tcp", *port)
		if err != nil {
			panic(err)
		}
		for {
			a, err := l.Accept()
			if err != nil {
				panic(err)
			}
			go func() {
				err := hc.Proxy(context.Background(), a, a)
				//err := hbone.HboneCat(http.DefaultClient, url, a, a)
				if err != nil {
					log.Println(err)
				}
			}()
		}
	}

	err := hc.Proxy(context.Background(), os.Stdin, os.Stdout)
	if err != nil {
		log.Fatal(err)
	}
}
