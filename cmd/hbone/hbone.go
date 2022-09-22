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
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	_ "net/http/pprof"

	"github.com/costinm/hbone"
	"github.com/costinm/hbone/auth"
	"github.com/costinm/hbone/ext/gcp"
	"github.com/costinm/hbone/ext/k8s"
	"github.com/costinm/hbone/ext/socks5"
	"github.com/costinm/hbone/ext/uxds"
	"github.com/costinm/hbone/nio"
	"sigs.k8s.io/yaml"
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

	hc := &hbone.MeshSettings{}
	err := loadMeshConfig(hc)
	if err != nil {
		panic(err)
	}

	a := auth.NewMeshAuth()
	a.FindCerts()
	if a.Cert != nil {
		log.Println("Certs: ", a.TrustDomain, a.Namespace, a.Name, len(a.Cert.Certificate))
	}

	ctx, cf := context.WithTimeout(context.Background(), 10000*time.Second)
	defer cf()

	hb := hbone.NewHBone(hc, a)

	// Initialize token-based auth.

	// Create AuthProvider["gcp"], returning access tokens using ADC or MDS
	// Not critical - will be used for clusters requiring google access tokens
	// ( GKE, googleapis.com )
	err = gcp.InitDefaultTokenSource(ctx, hb)
	if err != nil {
		log.Println("Missing GCP credentials", err)
	}

	id := hb.GetCluster("istiod.istio-system.svc:15012")
	// If available, init the k8s clusters - will be used for auth and as clusters
	// Init credentials and discovery server.
	k8sdefault, err := k8s.InitKubeconfig(hb, nil)
	if err != nil {
		log.Println("No k8s")
	} else {
		// This token source returns tokens for "istio-ca" audience, used by default by istiod and citadel
		catokenS := &k8s.K8STokenSource{Cluster: k8sdefault, AudOverride: "istio-ca",
			Namespace: hb.Namespace, KSA: hb.ServiceAccount}
		hb.AuthProviders["istio-ca"] = catokenS.GetToken

		if id != nil {
			id.TokenProvider = catokenS.GetToken
		} else {
			// Istiod cluster not configured - attempt to load it from k8s or env variables.
			// If one env is set - assume all of them are from env.

		}

		istiodAddr := hb.GetEnv("MCON_ADDR", "")
		if istiodAddr == "" {
			// Found a K8S cluster, try to locate configs in K8S by getting a config map containing Istio properties
			cm, err := k8s.GetConfigMap(ctx, k8sdefault, "istio-system", "mesh-env")
			if err != nil {
				log.Println("No mesh-env configuration", err)
			} else {
				// Tokens using istio-ca audience for Istio
				// If certificates exist, namespace/sa are initialized from the cert SAN
				for k, v := range cm {
					hb.Env[k] = v
				}

				istiodCA := hb.GetEnv("CAROOT_ISTIOD", "")
				istiodAddr = hb.GetEnv("MCON_ADDR", "")
				//log.Println("Using " + istiodAddr + "\n" + istiodCA)

				if id == nil {
					// Istiod cluster, using tokens
					c := hb.AddService(&hbone.Cluster{
						Addr:          istiodAddr + ":15012",
						TokenProvider: catokenS.GetToken,
						Id:            "istiod.istio-system.svc:15012",
						SNI:           "istiod.istio-system.svc",
						CACert:        istiodCA,
					})
					log.Println("XDS from k8s mesh-env", c.Addr, c.Id)
				}

				projectNumber := cm["PROJECT_NUMBER"]
				projectId := cm["PROJECT_ID"]
				clusterLocation := cm["CLUSTER_LOCATION"]
				clusterName := cm["CLUSTER_NAME"]
				// This returns JWT tokens for k8s
				//audTokenS := k8s.K8STokenSource{Cluster: k8sdefault, Namespace: hb.Namespace,
				//	KSA: hb.ServiceAccount}
				audTokenS := gcp.NewGSATokenSource(&gcp.AuthConfig{
					ProjectNumber: projectNumber,
					TrustDomain:   projectId + ".svc.id.goog",
					ClusterAddress: fmt.Sprintf("https://container.googleapis.com/v1/projects/%s/locations/%s/clusters/%s",
						projectId, clusterLocation, clusterName),
					TokenSource: &k8s.K8STokenSource{Cluster: k8sdefault, Namespace: hb.Namespace,
						KSA: hb.ServiceAccount},
				}, "")
				hb.AuthProviders["aud"] = audTokenS.GetToken
			}
		}

	}

	// Control plane - must be configured. May setup the cert using Citadel or meshca
	xdsC := hb.GetCluster("istiod.istio-system.svc:15012")
	if xdsC != nil {
		// TODO: connect and get policies/configs
		err = uxds.SetupControlPlane(ctx, hb, xdsC)
		if err != nil {
			log.Println("Failed to connect to XDS ", err)
		}
	}

	if a.Cert == nil {
		// Need to get a cert - using Istio CA or equivalent.
		log.Println("Missing certificates - assuming trusted network")
	}

	// OAuth2 ID Tokens are needed:
	// - to connect to OSS Istio XDS server
	// - to authenticate with proxies (PEPs, gateways) that require JWTs authentication
	//
	// HBone works without tokens if mTLS is used by all components.
	// It also works without certs if IP or DNS security is trusted.
	// TODO: move the k8s and STS code to hbone package.
	//tokenProvider := sts.NewGSATokenSource(&sts.AuthConfig{}, "")
	//tcache := sts.NewTokenCache(tokenProvider)
	//hb.TokenCallback = tcache.Token

	hb.Mux.HandleFunc("/computeMetadata/v1/instance/service-accounts/",
		func(writer http.ResponseWriter, request *http.Request) {
			// Envoy request: Metadata-Flavor:[Google] X-Envoy-Expected-Rq-Timeout-Ms:[1000] X-Envoy-Internal:[true]
			pathc := strings.Split(request.URL.RawQuery, "=")
			if len(pathc) != 2 {
				log.Println("Token error", err, request.URL.RawQuery)
				writer.WriteHeader(500)
				return
			}
			aud := pathc[1]
			tp := hb.AuthProviders["aud"]
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
		})

	if len(flag.Args()) == 0 { // sidecar/server mode
		listenServe(hc.HBone, ":15008", hb.HandleAcceptedH2)

		// Create a reverse tunnel, also making this node accessible from the mesh
		h2r := hb.GetCluster("h2r")
		if h2r != nil {
			go hbone.RemoteForward(hb, h2r.Addr, a.Namespace, a.Name)
		}

		listenServe(hc.HBoneC, ":15008", hb.HandleAcceptedH2C)

		listenServe(hc.SocksAddr, "127.0.0.1:1080", func(conn net.Conn) {
			err = socks5.HandleSocksConn(hb, conn)
			if err != nil {
				log.Println("Error handling SOCKS", err)
			}
		})
		listenServe(hc.SNI, ":15003", func(conn net.Conn) {
			hbone.HandleSNIConn(hb, conn)
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

func loadMeshConfig(hc *hbone.MeshSettings) error {
	var cfg []byte
	var err error

	cfgEnv := os.Getenv("HBONE_CFG")
	if cfgEnv != "" {
		cfg = []byte(cfgEnv)
	} else {
		cfg, err = ioutil.ReadFile("hbone.yaml")
		if err != nil {
			log.Println("Missing hbone.yaml, using defaults from env")
		}
	}

	//json.Unmarshal(cfg, &hc)
	err = yaml.Unmarshal(cfg, hc)
	if err != nil {
		log.Fatal("Failed to decode config", err)
	}
	return err
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
