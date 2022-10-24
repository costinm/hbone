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
	"github.com/costinm/hbone/ext/tel"
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
	hc := &hbone.MeshSettings{}
	err := handlers.LoadMeshConfig(hc, "")
	if err != nil {
		panic(err)
	}

	id := auth.NewMeshAuth()
	// TODO: test FromEnv with different options

	hb := hbone.New(id, hc)

	otel.InitProm(hb)

	//otel.OTelEnable(hb)
	//otelC := setup.FileExporter(ctx, os.Stderr)
	//defer otelC()

	gcp.InitDefaultTokenSource(ctx, hb)

	// Init credentials and discovery server.
	dk, err := k8s.InitK8S(ctx, hb)
	if err != nil {
		t.Fatal(err)
	}

	istiodC := handlers.InitXDSCluster(hb)
	if istiodC == nil {
		t.Fatal("No Istiod cluster")
	}
	err = uxds.GetCertificate(ctx, id, istiodC)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("xdsc-clusters", func(t *testing.T) {
		err = xdsTest(t, ctx, istiodC, hb)
		if err != nil {
			t.Fatal(err)
		}
	})

	// Test for 'system' streams
	t.Run("xdsc-system", func(t *testing.T) {
		urstSys := hbone.New(nil, nil)
		gcp.InitDefaultTokenSource(ctx, urstSys)

		catokenSystem := &k8s.K8STokenSource{
			Cluster:     dk,
			AudOverride: "istio-ca",
			Namespace:   "istio-system",
			KSA:         "default"}

		istiodSystem := urstSys.AddService(&hbone.Cluster{
			Addr:          istiodC.Addr,
			TokenProvider: catokenSystem.GetToken,
			ID:            "istiod",
			SNI:           "istiod.istio-system.svc",
			CACert:        istiodC.CACert,
		})

		turl := "istio.io/debug"
		xdsc, err := uxds.DialContext(ctx, "", &uxds.Config{
			Cluster: istiodSystem,
			HBone:   urstSys,
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
		err = xdsc.Send(&xds.DiscoveryRequest{
			TypeUrl:       turl,
			ResourceNames: []string{"syncz"},
		})
		if err != nil {
			t.Fatal(err)
		}

		_, err = xdsc.Wait(turl, 3*time.Second)

		if err != nil {
			t.Fatal(err)
		}
		dr := xdsc.Received[turl]
		log.Println(dr.Resources)
	})

	// Used for GCP tokens and calls.
	//projectNumber := hb.GetEnv("PROJECT_NUMBER", "")
	projectId := hb.GetEnv("PROJECT_ID", "")
	clusterLocation := hb.GetEnv("CLUSTER_LOCATION", "")
	clusterName := hb.GetEnv("CLUSTER_NAME", "")

	//hb.ProjectId = projectId

	t.Run("meshca-cert", func(t *testing.T) {
		meshCAID := auth.NewMeshAuth()

		meshca := hb.AddService(&hbone.Cluster{
			Addr:        "meshca.googleapis.com:443",
			TokenSource: "sts",
		})
		err := uxds.GetCertificate(ctx, meshCAID, meshca)

		if err != nil {
			t.Fatal(err)
		}
		log.Println("Cert: ", meshCAID.String())
	})

	tok1, err := hb.AuthProviders["gsa"](ctx, "https://example.com")
	if err != nil {
		t.Fatal("Failed to load k8s", err)
	}
	tok1J := auth.DecodeJWT(tok1)
	log.Println("Tok:", tok1J)

	tok, err := hb.AuthProviders["gsa"](ctx, "")
	if err != nil {
		t.Fatal("Failed to load k8s", err)
	}
	t.Run("hublist", func(t *testing.T) {
		cd, err := gcp.Hub2RestClusters(ctx, hb, tok, projectId)
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
		cd, err := gcp.GKE2RestCluster(ctx, hb, tok, projectId)
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
			TokenSource: &k8s.K8STokenSource{Cluster: dk},
		}, "k8s-default@"+projectId+".iam.gserviceaccount.com") // use the default KSA

		tokA, err := ts.GetToken(context.Background(), "")
		if err != nil {
			t.Fatal(err)
		}
		cd, err := gcp.GKE2RestCluster(ctx, hb, tokA, projectId)
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
		tok, err := hb.AuthProviders["gcp"](ctx, "")
		if err != nil {
			t.Fatal("Failed to load k8s", err)
		}
		cd, err := gcp.GcpSecret(ctx, hb, tok, projectId, "ca", "1")
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
