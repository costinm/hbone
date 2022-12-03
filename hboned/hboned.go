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
	"log"
	"strings"
	"time"

	_ "net/http/pprof"

	"github.com/costinm/hbone"
	"github.com/costinm/hbone/hboned/gcp"
	"github.com/costinm/hbone/hboned/handlers"
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

	// Load mesh settings - may include settings for auth, etc.
	hc := &hbone.MeshSettings{}
	err := handlers.LoadMeshConfig(hc, "")
	if err != nil {
		panic(err)
	}

	if *localForward != "" {
		parts := strings.SplitN(*localForward, ":", 2)
		if len(parts) != 2 {
			log.Fatal("Expecting 2 parts" + *localForward)
		}
		dest := parts[1]
		hc.LocalForward[0] = dest
	}

	// Initialize the identity from existing files.
	id, err := auth.FromEnv(nil)
	if err != nil {
		panic(err)
	}

	// Init H2 node.
	hb := hbone.New(id, hc)

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
	if xdsC != nil {
		if id.Cert == nil {
			// Need to get a cert - using Istio CA or equivalent.
			err = urpc.GetCertificate(ctx, id, xdsC)
			log.Println("Getting cert from Istiod", err)
		}

		log.Println("XDS ", xdsC.Addr)
		// TODO: connect and get policies/configs
		err = urpc.SetupControlPlane(ctx, hb, xdsC)
		if err != nil {
			log.Println("Failed to connect to XDS ", err)
		}
	}

	if id.Cert != nil {
		log.Println("Certs: ", id.TrustDomain, id.Namespace, id.Name, len(id.Cert.Certificate))
	} else {
		id.InitSelfSigned("")
		id.SaveCerts(".")
		log.Println("No Certs: ", id.TrustDomain, id.Namespace, id.Name)
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

	handlers.Start(hb)

	select {}

}
