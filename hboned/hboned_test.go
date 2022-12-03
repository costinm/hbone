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

package main

import (
	"context"
	"fmt"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/costinm/hbone"
	"github.com/costinm/hbone/hboned/gcp"
	"github.com/costinm/hbone/hboned/handlers"
	"github.com/costinm/hbone/urpc"
	"github.com/costinm/hbone/urpc/gen/xds"
	"github.com/costinm/hbone/urpc/k8s"
	auth "github.com/costinm/meshauth"
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

	// No certificate by default, but the test may be run with different params
	aliceCfg := &hbone.MeshSettings{}
	err := handlers.LoadMeshConfig(aliceCfg, "")
	if err != nil {
		panic(err)
	}

	aliceID := auth.NewMeshAuth(nil)

	// TODO: test FromEnv with different options

	alice := hbone.New(aliceID, aliceCfg)

	//otel.InitProm(hb)
	//otel.OTelEnable(hb)
	//otelC := setup.FileExporter(ctx, os.Stderr)
	//defer otelC()

	gcp.InitDefaultTokenSource(ctx, alice)

	// Init credentials and discovery server.
	k8sService, err := k8s.InitK8S(ctx, alice)
	if err != nil {
		t.Fatal(err)
	}

	istiodC := handlers.InitXDSCluster(alice)
	if istiodC == nil {
		t.Fatal("No Istiod cluster")
	}

	t.Run("get-cert", func(t *testing.T) {
		err = urpc.GetCertificate(ctx, aliceID, istiodC)
		if err != nil {
			t.Fatal(err)
		}
	})

	t.Run("xdsc-clusters", func(t *testing.T) {
		err = xdsTest(t, ctx, istiodC, alice)
		if err != nil {
			t.Fatal(err)
		}
	})

	// Test for 'system' streams
	t.Run("xdsc-system", func(t *testing.T) {
		system := hbone.New(nil, nil)
		gcp.InitDefaultTokenSource(ctx, system)

		catokenSystem := &k8s.K8STokenSource{
			Cluster:     k8sService,
			AudOverride: "istio-ca",
			Namespace:   "istio-system",
			KSA:         "default"}

		istiodSystem := system.AddService(&hbone.Cluster{
			Addr:          istiodC.Addr,
			TokenProvider: catokenSystem.GetToken,
			ID:            "istiod",
			SNI:           "istiod.istio-system.svc",
			CACert:        istiodC.CACert,
		})

		turl := "istio.io/debug"
		systemXDS, err := urpc.DialContext(ctx, "", &urpc.Config{
			Cluster: istiodSystem,
			HBone:   system,
			Meta: map[string]interface{}{
				"SERVICE_ACCOUNT": "default",
				"NAMESPACE":       "istio-system",
				"GENERATOR":       "event",
				// istio.io/debug
			},
			Namespace: "istio-system",
			InitialDiscoveryRequests: []*xds.DiscoveryRequest{
				{TypeUrl: turl, ResourceNames: []string{"syncz"}},
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		err = systemXDS.Send(&xds.DiscoveryRequest{
			TypeUrl:       turl,
			ResourceNames: []string{"syncz"},
		})
		if err != nil {
			t.Fatal(err)
		}

		_, err = systemXDS.Wait(turl, 3*time.Second)

		if err != nil {
			t.Fatal(err)
		}
		dr := systemXDS.Received[turl]
		log.Println(dr.Resources)
	})

	// Used for GCP tokens and calls.
	//projectNumber := hb.GetEnv("PROJECT_NUMBER", "")
	projectId := alice.GetEnv("PROJECT_ID", "")
	clusterLocation := alice.GetEnv("CLUSTER_LOCATION", "")
	clusterName := alice.GetEnv("CLUSTER_NAME", "")

	//hb.ProjectId = projectId

	t.Run("meshca-cert", func(t *testing.T) {
		meshCAID := auth.NewMeshAuth(nil)

		meshca := alice.AddService(&hbone.Cluster{
			Addr:        "meshca.googleapis.com:443",
			TokenSource: "sts",
		})
		err := urpc.GetCertificate(ctx, meshCAID, meshca)

		if err != nil {
			t.Fatal(err)
		}
		log.Println("Cert: ", meshCAID.String())
	})

	tok1, err := alice.AuthProviders["gsa"](ctx, "https://example.com")
	if err != nil {
		t.Fatal("Failed to load k8s", err)
	}
	tok1J := auth.DecodeJWT(tok1)
	log.Println("Tok:", tok1J)

	tok, err := alice.AuthProviders["gsa"](ctx, "")
	if err != nil {
		t.Fatal("Failed to load k8s", err)
	}
	t.Run("hublist", func(t *testing.T) {
		cd, err := gcp.Hub2RestClusters(ctx, alice, tok, projectId)
		if err != nil {
			log.Println("SA doesn't have permission", tok1J.Email)
			t.Fatal(err)
		}
		for _, c := range cd {
			//uk.Current = c

			log.Println(c)
		}
	})

	t.Run("gkelist", func(t *testing.T) {
		cd, err := gcp.GKE2RestCluster(ctx, alice, tok, projectId)
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
			TrustDomain: projectId + ".svc.id.goog",
			ClusterAddress: fmt.Sprintf("https://container.googleapis.com/v1/projects/%s/locations/%s/clusters/%s",
				projectId, clusterLocation, clusterName),
			TokenSource: &k8s.K8STokenSource{Cluster: k8sService},
		}, "k8s-default@"+projectId+".iam.gserviceaccount.com") // use the default KSA

		tokA, err := ts.GetToken(context.Background(), "")
		if err != nil {
			t.Fatal(err)
		}
		cd, err := gcp.GKE2RestCluster(ctx, alice, tokA, projectId)
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
		tok, err := alice.AuthProviders["gcp"](ctx, "")
		if err != nil {
			t.Fatal("Failed to load k8s", err)
		}
		cd, err := gcp.GcpSecret(ctx, alice, tok, projectId, "ca", "1")
		if err != nil {
			t.Fatal(err)
		}
		log.Println(string(cd))
	})

	t.Run("watch", func(t *testing.T) {
		req := k8s.WatchRequest(ctx, k8sService, "istio-system", "configmap", "")
		k8sService.AddToken(req, "https://"+k8sService.Addr)

		// Find or dial an h2 transport
		tr, err := k8sService.DialRequest(req)

		if err != nil {
			t.Fatal(err)
		}
		tr.WaitHeaders()
		if tr.Response.Status != "200" {
			t.Fatal("Response statuds ", tr.Response.Status, tr.Response.Header)
		}
		fr := urpc.NewFromStream(tr)
		for {
			bb, err := fr.Recv4()
			if err != nil {
				t.Fatal(err)
			}
			log.Println(string(bb.Bytes()))

		}
	})

}

var marshal = &jsonpb.Marshaler{OrigName: true, Indent: "  "}

func xdsTest(t *testing.T, ctx context.Context, istiodC *hbone.Cluster, ns *hbone.HBone) error {
	xdsc, err := urpc.DialContext(ctx, "", &urpc.Config{
		Cluster: istiodC,
		HBone:   ns,
		ResponseHandler: func(con *urpc.ADSC, r *xds.DiscoveryResponse) {
			log.Println("DR:", r.TypeUrl, r.VersionInfo, r.Nonce, len(r.Resources))
			for _, l := range r.Resources {
				b, err := marshal.MarshalToString(l)
				if err != nil {
					log.Printf("Error in LDS: %v", err)
				}

				if false {
					log.Println(b)
				}
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
