// Copyright 2021 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package test

import (
	"context"
	"fmt"
	"log"
	"strings"
	"testing"

	"github.com/costinm/hbone"
	"github.com/costinm/hbone/ext/grpc"
	"github.com/costinm/hbone/urest/gcp"
	"github.com/costinm/hbone/urest/k8s"
	"github.com/costinm/ugate/auth"
	istioca "github.com/costinm/ugate/gen/proto/istio/v1/auth"
	"github.com/costinm/ugate/gen/proto/xds"

	"google.golang.org/protobuf/proto"
)

// Requires a GSA (either via GOOGLE_APPLICATION_CREDENTIALS, gcloud config, metadata) with hub and
// container access.
// Requires a kube config - the default cluster should be in same project.
//
// Will verify kube config loading and queries to hub and gke.
//
func TestURest(t *testing.T) {
	ctx, cf := context.WithCancel(context.Background())
	defer cf()

	urst := hbone.NewMesh(&hbone.MeshSettings{})

	// Support for "gcp" access tokens using ADS or MDS
	gcp.InitDefaultTokenSource(ctx, urst)

	// Init credentials and discovery server.
	dk, err := k8s.InitKubeconfig(urst, nil)
	if err != nil {
		t.Fatal(err)
	}

	// Get a config map containing Istio properties
	cm, err := k8s.GetConfigMap(ctx, dk, "istio-system", "mesh-env")
	if err != nil {
		t.Fatal("Failed to load k8s", err)
	}

	// Tokens using istio-ca audience for Istio
	catokenS := &k8s.K8STokenSource{Cluster: dk, AudOverride: "istio-ca", Namespace: urst.Namespace, KSA: urst.ServiceAccount}
	catokenSystem := &k8s.K8STokenSource{Cluster: dk, AudOverride: "istio-ca", Namespace: "istio-system", KSA: urst.ServiceAccount}

	istiodCA := cm["CAROOT_ISTIOD"]
	istiodAddr := cm["MCON_ADDR"]

	istiodC := urst.AddCluster("", &hbone.Cluster{
		Addr:          istiodAddr + ":15012",
		TokenProvider: catokenS.GetToken,
		Id:            "istiod",
		SNI:           "istiod.istio-system.svc",
		CACert:        []byte(istiodCA),
	})
	istiodSystem := urst.AddCluster("istiod-system", &hbone.Cluster{
		Addr:          istiodAddr + ":15012",
		TokenProvider: catokenSystem.GetToken,
		Id:            "istiod",
		SNI:           "istiod.istio-system.svc",
		CACert:        []byte(istiodCA),
	})

	id := auth.NewMeshAuth()
	meshCAID := auth.NewMeshAuth()

	t.Run("istiocert", func(t *testing.T) {
		err = getCertificate(ctx, id, istiodC)
		if err != nil {
			t.Fatal(err)
		}
	})

	t.Run("xdsc", func(t *testing.T) {
		err = xdsTest(t, ctx, istiodSystem, "istio-system", "istio/debug/events")
		if err != nil {
			t.Fatal(err)
		}
	})

	t.Run("xdsc-clusters", func(t *testing.T) {
		err = xdsTest(t, ctx, istiodC, "default", "type.googleapis.com/envoy.config.cluster.v3.Cluster")
		if err != nil {
			t.Fatal(err)
		}
	})

	// Used for GCP tokens and calls.
	projectNumber := cm["PROJECT_NUMBER"]
	clusterLocation := cm["CLUSTER_LOCATION"]
	clusterName := cm["CLUSTER_NAME"]
	projectId := cm["PROJECT_ID"]

	urst.ProjectId = projectId

	// Access tokens
	tok, err := urst.AuthProviders["gcp"](ctx, "")
	if err != nil {
		t.Fatal("Failed to load k8s", err)
	}

	t.Run("meshca-cert", func(t *testing.T) {
		sts := auth.NewFederatedTokenSource(&auth.AuthConfig{
			ProjectNumber: projectNumber,
			TrustDomain:   projectId + ".svc.id.goog",
			ClusterAddress: fmt.Sprintf("https://container.googleapis.com/v1/projects/%s/locations/%s/clusters/%s",
				projectId, clusterLocation, clusterName),

			// Will use TokenRequest to get tokens with AudOverride
			TokenSource: &k8s.K8STokenSource{
				Cluster:     dk,
				AudOverride: projectId + ".svc.id.goog",
				Namespace:   urst.Namespace,
				KSA:         urst.ServiceAccount},
		})

		meshca := urst.AddCluster("", &hbone.Cluster{
			Addr:          "meshca.googleapis.com:443",
			TokenProvider: sts.GetToken,
			// Used in the TLS request and to verify the DNS SANs
			SNI: "meshca.googleapis.com",
		})
		err := getCertificate(ctx, meshCAID, meshca)

		if err != nil {
			t.Fatal(err)
		}
	})

	t.Run("hublist", func(t *testing.T) {
		cd, err := gcp.Hub2RestClusters(ctx, urst, tok, urst.ProjectId)
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range cd {
			//uk.Current = c

			log.Println(c)
		}
	})

	t.Run("gkelist", func(t *testing.T) {
		cd, err := gcp.GKE2RestCluster(ctx, urst, tok, urst.ProjectId)
		if err != nil {
			if strings.HasPrefix(err.Error(), "403") {
				t.Skip("Account not authorized for GKE")
			} else {
				t.Fatal(err)
			}
		}
		for _, c := range cd {
			log.Println(c)
		}
	})

	t.Run("gkelist-in", func(t *testing.T) {

		ts := auth.NewGSATokenSource(&auth.AuthConfig{
			ProjectNumber: projectNumber,
			TrustDomain:   projectId + ".svc.id.goog",
			ClusterAddress: fmt.Sprintf("https://container.googleapis.com/v1/projects/%s/locations/%s/clusters/%s",
				projectId, clusterLocation, clusterName),
			TokenSource: &k8s.K8STokenSource{Cluster: dk},
		}, "") // use the default KSA

		tokA, err := ts.GetToken(context.Background(), "")
		if err != nil {
			t.Fatal(err)
		}
		cd, err := gcp.GKE2RestCluster(ctx, urst, tokA, urst.ProjectId)
		if err != nil {
			if strings.HasPrefix(err.Error(), "403") {
				t.Skip("Account not authorized for GKE")
			} else {
				t.Fatal(err)
			}
		}
		for _, c := range cd {
			log.Println(c)
		}
	})

	t.Run("secret", func(t *testing.T) {
		cd, err := gcp.GcpSecret(ctx, urst, tok, urst.ProjectId, "ca", "1")
		if err != nil {
			t.Fatal(err)
		}
		log.Println(string(cd))
	})

}

// getCertificate is using Istio CA gRPC protocol to get a certificate for the id.
// Google implementation of the protocol is also supported.
func getCertificate(ctx context.Context, id *auth.Auth, ca *hbone.Cluster) error {
	// TODO: Add ClusterID header
	var req istioca.IstioCertificateRequest

	_, csr, err := id.NewCSR("spiffe://cluster.local/ns/default/sa/default")
	req.Csr = string(csr)

	payload, err := proto.Marshal(&req)
	// TODO: Add ClusterID header

	path := "/istio.v1.auth.IstioCertificateService/CreateCertificate"
	if strings.Contains(ca.Addr, "meshca.googleapis.com") {
		path = "/google.security.meshca.v1.MeshCertificateService/CreateCertificate"
	}

	//gstr := urest.NewGRPCStream(ctx, ca, path)

	resd, err := grpc.DoGRPC(ctx, ca, path, payload)
	if err != nil {
		return err
	}
	var res istioca.IstioCertificateResponse
	proto.Unmarshal(resd, &res)
	log.Println("Cert: ", res.CertChain)
	return err
}

func xdsTest(t *testing.T, ctx context.Context, istiodC *hbone.Cluster, ns, typeurl string) error {

	// Will create a Request, populate with basic headers. The first Send will trigger the roundtrip.
	// Receive will return the results.
	gstr := grpc.NewGRPCStream(ctx, istiodC,
		"/envoy.service.discovery.v3.AggregatedDiscoveryService/StreamAggregatedResources")

	// Istio will wait for the first message to be received. Regular gRPC also requires
	// the body to be available.
	var req xds.DiscoveryRequest
	req.Node = &xds.Node{
		Id: fmt.Sprintf("sidecar~10.244.0.36~foo.%s~%s.svc.cluster.local", ns, ns),
	}
	//req.TypeUrl =
	//
	req.TypeUrl = typeurl

	b := gstr.GetWriteFrame()
	bout, err := proto.MarshalOptions{}.MarshalAppend(b.Bytes(), &req)
	if err != nil {
		t.Fatal(err)
	}
	b.UpdateAppend(bout)

	err = gstr.Send(b) // will roundtrip for first send
	if err != nil {
		t.Fatal(err)
	}

	for {
		rm, err := gstr.Recv()
		if err != nil {
			t.Fatal(err)
		}

		var res xds.DiscoveryResponse
		// Makes copy of byte[]
		proto.Unmarshal(rm.Data, &res)
		log.Println("XDSRes: ", res)
	}
	return err
}
