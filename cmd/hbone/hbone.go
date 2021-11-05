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
	"os"
	"strings"
	"time"

	"github.com/costinm/hbone"
	"github.com/costinm/hbone/tcpproxy"

	"github.com/costinm/krun/pkg/mesh"
	"github.com/costinm/krun/pkg/sts"
)

var (
	localForward = flag.String("L", "", "Local port, if set connections to this port will be forwarded to the mesh service")
)

// Create a HBONE tunnel, using K8S credentials and config.
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
	// WIP - multiple ports
	//flag.Var(&localForwards, "LocalForward", "SSH-style local forward")
	flag.Parse()

	kr := mesh.New("")

	ctx, cf := context.WithTimeout(context.Background(), 10000*time.Second)
	defer cf()

	// Use kubeconfig or gcp to find the cluster
	err := kr.LoadConfig(ctx)
	if err != nil {
		log.Fatal("Failed to connect to K8S ", time.Since(kr.StartTime), kr, os.Environ(), err)
	}

	// Not calling RefreshAndSaveTokens - hbone is not creating files, jwts and certs in memory only.
	// Also not initializing pilot-agent or envoy - this is just using k8s to configure the hbone tunnel

	auth := hbone.NewAuth()
	tokenProvider, err := sts.NewSTS(kr)

	if kr.MeshConnectorAddr == "" {
		log.Fatal("Failed to find in-cluster, missing 'hgate' service in mesh env")
	}

	kr.XDSAddr = kr.MeshConnectorAddr + ":15012"

	// fmt.Fprintln(os.Stderr, "Starting ", kr)
	// TODO: move to library, possibly to separate CLI (authtool ?)
	// Hbone 'base' should just use the mesh cert files, call tool or expect cron to renew

	hb := hbone.New(auth)

	tcache := sts.NewTokenCache(kr, tokenProvider)

	hb.TokenCallback = tcache.Token

	if kr.MeshConnectorAddr != "" {
		go tcpproxy.RemoteForwardPort("", hb, kr.MeshConnectorAddr+":15441", kr.Namespace, kr.Name)
	}
	if *localForward != "" {
		parts := strings.SplitN(*localForward, ":", 2)
		if len(parts) != 2 {
			log.Fatal("Expecting 2 parts" + *localForward)
		}
		dest := parts[1]
		go tcpproxy.LocalForwardPort("127.0.0.1:"+parts[0], dest, hb, kr.MeshConnectorAddr, auth)

		select {}
	}

	if len(flag.Args()) == 0 {
		flag.Usage()
		os.Exit(1)
	}

	dest := flag.Arg(0)
	err = tcpproxy.Forward(dest, hb, kr.MeshConnectorAddr, auth, os.Stdin, os.Stdout)
	if err != nil {
		fmt.Fprintln(os.Stderr, "Error forwarding ", err)
		log.Fatal(err)
	}
}
