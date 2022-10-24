//Copyright 2021 Google LLC
//
//Licensed under the Apache License, Version 2.0 (the "License");
//you may not use this file except in compliance with the License.
//You may obtain a copy of the License at
//
//    https://www.apache.org/licenses/LICENSE-2.0
//
//Unless required by applicable law or agreed to in writing, software
//distributed under the License is distributed on an "AS IS" BASIS,
//WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
//See the License for the specific language governing permissions and
//limitations under the License.

package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	_ "net/http/pprof"

	"github.com/costinm/hbone"
	"github.com/costinm/hbone/ext/tel"
	"github.com/costinm/hbone/hboned/gcp"
	"github.com/costinm/hbone/hboned/handlers"
	"github.com/costinm/hbone/nio"
	"github.com/costinm/hbone/urpc"
	"github.com/costinm/hbone/urpc/k8s"
	auth "github.com/costinm/meshauth"
)

var (
	localForward = flag.String("L", "", "Local port, if set connections to this port will be forwarded to the mesh service")
)

// Create a HBONE tunnel, using provisioned certificates.
//
// For example, for ssh:
//
// ssh -o ProxyCommand='hbone %h:22' root@fortio-cr.fortio
// If the server doesn't have persistent SSH key, add to the ssh parameters:
//
//	-F /dev/null -o StrictHostKeyChecking=no -o "UserKnownHostsFile /dev/null"
//
// CLI:
// - args[0], if present will be used to create a simple tunnel, suitable for ssh ProxyCommand
// - "-L" is used to forward local ports to mesh addresses. Will also crete a reverse proxy if MESH_ADDR is set.
//
// If XDS_ADDR is set, will connect and get configs from the mesh.
// If MESH_ADDR is set, will register for reverse connections and use it as PEP/egress gateway.
// Otherwise, the "dest" is expected to support HBone.
func main() {
	flag.Parse()
	ctx, cf := context.WithTimeout(context.Background(), 10000*time.Second)
	defer cf()

	hc := &hbone.MeshSettings{}
	err := handlers.LoadMeshConfig(hc, "")
	if err != nil {
		panic(err)
	}

	id, err := auth.FromEnv("", true)
	if err != nil {
		panic(err)
	}
	// Best case: we have platform provided certificates. We can just use them.
	if id.Cert != nil {
		log.Println("Certs: ", id.TrustDomain, id.Namespace, id.Name, len(id.Cert.Certificate))
	}

	hb := hbone.New(id, hc)

	otel.InitProm(hb)

	// Initialize token-based auth. This is useful for making calls to GCP APIs
	// Optional, env specific.

	// Create AuthProvider["gcp"], returning access tokens using ADC or MDS
	// Not critical - will be used for clusters requiring google access tokens
	// ( GKE, googleapis.com )
	// GCP token source is optional, will be used if available
	gcp.InitDefaultTokenSource(ctx, hb)

	// Initialize K8S clusters and auth. Useful for making calls to K8S and authenticating with K8S JWTs.
	// Optional, env specific.

	// If available, init the k8s clusters - will be used for auth and as clusters
	// Init credentials and discovery server.
	_, err = k8s.InitK8S(ctx, hb)
	if err != nil {
		log.Fatal("Invalid K8S environment", err)
	}

	xdsC := handlers.InitXDSCluster(hb)

	if id.Cert == nil && xdsC != nil {
		// Need to get a cert - using Istio CA or equivalent.
		err = uxds.GetCertificate(ctx, id, xdsC)
		log.Println("Getting cert from Istiod", err)
	}

	if id.Cert == nil {
		// Need to get a cert - using Istio CA or equivalent.
		log.Println("Missing certificates - assuming trusted/secure network only")
	}

	if xdsC != nil {
		log.Println("XDS ", xdsC.Addr)
		// TODO: connect and get policies/configs
		err = uxds.SetupControlPlane(ctx, hb, xdsC)
		if err != nil {
			log.Println("Failed to connect to XDS ", err)
		}
	}

	// OAuth2 WorkloadID Tokens are needed:
	// - to connect to OSS Istio XDS server
	// - to authenticate with proxies (PEPs, gateways) that require JWTs authentication
	//
	// HBone works without tokens if mTLS is used by all components.
	// It also works without certs if IP or DNS security is trusted.
	// TODO: move the k8s and STS code to hbone package.
	//tokenProvider := sts.NewGSATokenSource(&sts.AuthConfig{}, "")
	//tcache := sts.NewTokenCache(tokenProvider)
	//hb.TokenCallback = tcache.Token

	hb.Mux.HandleFunc("/computeMetadata/v1/instance/service-accounts/", MDSHandler(hb))

	if len(flag.Args()) == 0 { // sidecar/server mode
		listenServe(hc.HBone, ":15008", hb.HandleAcceptedH2)

		// Create a reverse tunnel, also making this node accessible from the mesh
		h2r := hb.GetCluster("h2r")
		if h2r != nil {
			go handlers.RemoteForward(hb, h2r.Addr, id.Namespace, id.Name)
		}

		listenServe(hc.HBoneC, ":15009", hb.HandleAcceptedH2C)

		listenServe(hc.SocksAddr, "127.0.0.1:1080", func(conn net.Conn) {
			err = handlers.HandleSocksConn(hb, conn)
			if err != nil {
				log.Println("Error handling SOCKS", err)
			}
		})

		listenServe(hc.SNI, ":15003", func(conn net.Conn) {
			handlers.HandleSNIConn(hb, conn)
		})

		if hc.AdminPort != "-" {
			go func() {
				port := hc.AdminPort
				if port == "" {
					port = ":15000"
				}
				http.ListenAndServe(port, nil)
			}()
		}

		if *localForward != "" {
			parts := strings.SplitN(*localForward, ":", 2)
			if len(parts) != 2 {
				log.Fatal("Expecting 2 parts" + *localForward)
			}
			dest := parts[1]
			go func() {
				err := hbone.LocalForwardPort("127.0.0.1:"+parts[0], dest, hb)
				if err != nil {
					log.Fatal("Failed to forward port", err)
				}
			}()
		}
		for p, a := range hc.LocalForward {
			port := p
			addr := a
			go func() {
				err := hbone.LocalForwardPort(fmt.Sprintf("127.0.0.1:%d", port), addr, hb)
				if err != nil {
					log.Fatal("Failed to forward port", err)
				}
			}()
		}

		select {}
	}

	// Client mode, using a single dest address.
	dest := flag.Arg(0)
	nc, err := hb.DialContext(ctx, "", dest)
	if err != nil {
		fmt.Fprintln(os.Stderr, "Error forwarding ", err)
		log.Fatal(err)
	}

	err = hbone.Proxy(nc, os.Stdin, os.Stdout, "stdin")
	if err != nil {
		fmt.Fprintln(os.Stderr, "Error forwarding ", err)
		log.Fatal(err)
	}
}

// Adapter emulating MDS using an authenticator (K8S or GCP)
// Allows Envoy, gRPC to work without extra code to handle token exchanges.
// "aud" is the special provider returning access and audience tokens.
func MDSHandler(hb *hbone.HBone) func(writer http.ResponseWriter, request *http.Request) {
	return func(writer http.ResponseWriter, request *http.Request) {
		// Envoy request: Metadata-Flavor:[Google] X-Envoy-Expected-Rq-Timeout-Ms:[1000] X-Envoy-Internal:[true]
		pathc := strings.Split(request.URL.RawQuery, "=")
		if len(pathc) != 2 {
			log.Println("Token error", request.URL.RawQuery)
			writer.WriteHeader(500)
			return
		}
		aud := pathc[1]
		tp := hb.AuthProviders["gsa"]
		if tp == nil {
			tp = hb.AuthProviders["gcp"]
		}
		if tp == nil {
			writer.WriteHeader(500)
			return
		}
		tok, err := tp(context.Background(), aud)
		if err != nil {
			log.Println("Token error", err, pathc)
			writer.WriteHeader(500)
			return
		}
		writer.WriteHeader(200)
		log.Println(aud, tok)
		fmt.Fprintf(writer, "%s", tok)
	}
}

func listenServe(port string, def string, f func(net.Conn)) {
	if port == "" {
		port = def
	}
	if port != "-" && port != "" {
		_, err := nio.ListenAndServeTCP(port, f)
		if err != nil {
			log.Fatal("Failed to listen on ", port, err)
		}
	}
}
