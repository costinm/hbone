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
	"time"

	"github.com/costinm/hbone"
	"github.com/costinm/hbone/auth"
	"github.com/costinm/hbone/ext/gcp"
	"github.com/costinm/hbone/ext/k8s"
	"github.com/costinm/hbone/ext/uxds"
	"github.com/costinm/hbone/ext/uxds/xds"
	"github.com/golang/protobuf/jsonpb"
)

// Requires a GSA (either via GOOGLE_APPLICATION_CREDENTIALS, gcloud config, metadata) with hub and
// container access.
// Requires a kube config - the default cluster should be in same project.
//
// Will verify kube config loading and queries to hub and gke.
func TestURest(t *testing.T) {
	ctx, cf := context.WithCancel(context.Background())
	defer cf()

	// No certificate !
	hc := &hbone.MeshSettings{}
	id := auth.NewMeshAuth()
	urst := hbone.NewHBone(hc, id)

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

	istiodCA := cm["CAROOT_ISTIOD"]
	istiodAddr := cm["MCON_ADDR"]
	//log.Println("Using " + istiodAddr + "\n" + istiodCA)

	// Istiod cluster, using tokens
	istiodC := urst.AddService(&hbone.Cluster{
		Addr:          istiodAddr + ":15012",
		TokenProvider: catokenS.GetToken,
		Id:            "istiod.istio-system.svc:15012",
		SNI:           "istiod.istio-system.svc",
		CACert:        istiodCA,
	})

	t.Run("xdsc-clusters", func(t *testing.T) {
		err = xdsTest(t, ctx, istiodC, urst)
		if err != nil {
			t.Fatal(err)
		}
	})

	// Cert provisioning using tokens.
	t.Run("istiocert", func(t *testing.T) {
		err = uxds.GetCertificate(ctx, id, istiodC)
		if err != nil {
			t.Fatal(err)
		}
	})

	// Test for 'system' streams
	t.Run("xdsc-system", func(t *testing.T) {
		urstSys := hbone.NewMesh(&hbone.MeshSettings{})
		gcp.InitDefaultTokenSource(ctx, urstSys)
		catokenSystem := &k8s.K8STokenSource{Cluster: dk, AudOverride: "istio-ca",
			Namespace: "istio-system",
			KSA:       "default"}

		istiodSystem := urstSys.AddService(&hbone.Cluster{
			Addr:          istiodAddr + ":15012",
			TokenProvider: catokenSystem.GetToken,
			Id:            "istiod",
			SNI:           "istiod.istio-system.svc",
			CACert:        istiodCA,
		})

		xdsc, err := uxds.DialContext(ctx, "", &uxds.Config{
			Cluster: istiodSystem,
			HBone:   urstSys,
			Meta: map[string]interface{}{
				"SERVICE_ACCOUNT": "default",
				"NAMESPACE":       "istio-system",
			},
			InitialDiscoveryRequests: []*xds.DiscoveryRequest{
				{TypeUrl: "istio/debug/events"},
			},
		})
		if err != nil {
			t.Fatal(err)
		}

		err = xdsc.Send(&xds.DiscoveryRequest{
			TypeUrl: "istio/debug/events"})
		if err != nil {
			t.Fatal(err)
		}

		xdsc.Wait("istio/debug/events", 5*time.Second)

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

	sts := gcp.NewFederatedTokenSource(&gcp.AuthConfig{
		TrustDomain: projectId + ".svc.id.goog",
		ClusterAddress: fmt.Sprintf("https://container.googleapis.com/v1/projects/%s/locations/%s/clusters/%s",
			projectId, clusterLocation, clusterName),

		// Will use TokenRequest to get tokens with AudOverride
		TokenSource: &k8s.K8STokenSource{
			Cluster:     dk,
			AudOverride: projectId + ".svc.id.goog",
			Namespace:   urst.Namespace,
			KSA:         urst.ServiceAccount},
	})
	urst.AuthProviders["sts"] = sts.GetToken

	t.Run("meshca-cert", func(t *testing.T) {
		meshCAID := auth.NewMeshAuth()

		meshca := urst.AddService(&hbone.Cluster{
			Addr:          "meshca.googleapis.com:443",
			TokenProvider: sts.GetToken,
		})
		err := uxds.GetCertificate(ctx, meshCAID, meshca)

		if err != nil {
			t.Fatal(err)
		}
		log.Println("Cert: ", meshCAID.String())
	})

	tok, err := urst.AuthProviders["gcp"](ctx, "")
	if err != nil {
		t.Fatal("Failed to load k8s", err)
	}
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

		ts := gcp.NewGSATokenSource(&gcp.AuthConfig{
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
		tok, err := urst.AuthProviders["gcp"](ctx, "")
		if err != nil {
			t.Fatal("Failed to load k8s", err)
		}
		cd, err := gcp.GcpSecret(ctx, urst, tok, urst.ProjectId, "ca", "1")
		if err != nil {
			t.Fatal(err)
		}
		log.Println(string(cd))
	})

}

var marshal = &jsonpb.Marshaler{OrigName: true, Indent: "  "}

func xdsTest(t *testing.T, ctx context.Context, istiodC *hbone.Cluster, ns *hbone.HBone) error {
	xdsc, err := uxds.DialContext(ctx, "", &uxds.Config{
		Cluster: istiodC,
		HBone:   ns,
		ResponseHandler: func(con *uxds.ADSC, r *xds.DiscoveryResponse) {
			log.Println("DR:", r.TypeUrl, r.VersionInfo, r.Nonce, len(r.Resources))
			for _, l := range r.Resources {
				b, err := marshal.MarshalToString(l)
				if err != nil {
					log.Printf("Error in LDS: %v", err)
				}

				log.Println(b)
			}
		},
	})

	if err != nil {
		t.Fatal(err)
	}

	for {
		res := <-xdsc.Updates
		if res == "eds" {
			break
		}
	}

	for _, c := range xdsc.Endpoints {

		if len(c.Endpoints) > 0 && len(c.Endpoints[0].LbEndpoints) > 0 {
			log.Println(c.ClusterName, c.Endpoints[0].LbEndpoints[0].Endpoint.Address)
		}
		//log.Println(prototext.MarshalOptions{Multiline: true}.Format(&c))
	}

	return err
}
