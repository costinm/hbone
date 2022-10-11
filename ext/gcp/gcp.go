package gcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"strings"

	"github.com/costinm/hbone"

	"golang.org/x/oauth2/google"
)

// Subset of the generated GCP config - to avoid a dependency and bloat.
// This include just what we need for bootstraping from GKE, Hub or secret store.

// Clusters return the list of GKE clusters.
type Clusters struct {
	Clusters []*Cluster
}

type Cluster struct {
	Name string

	// nodeConfig
	MasterAuth struct {
		ClusterCaCertificate []byte
	}
	Location string

	Endpoint string

	ResourceLabels map[string]string

	// Extras:

	// loggingService, monitoringService
	//Network string "default"
	//Subnetwork string
	ClusterIpv4Cidr  string
	ServicesIpv4Cidr string
	// addonsConfig
	// nodePools

	// For regional clusters - each zone.
	// For zonal - one entry, equal with location
	Locations []string
	// ipAllocationPolicy - clusterIpv4Cider, serviceIpv4Cider...
	// masterAuthorizedNetworksConfig
	// maintenancePolicy
	// autoscaling
	NetworkConfig struct {
		// projects/NAME/global/networks/default
		Network    string
		Subnetwork string
	}
	// releaseChannel
	// workloadIdentityConfig

	// It seems zone and location are the same for zonal clusters.
	//Zone string // ex: us-west1
}

// HubClusters return the list of clusters registered in GKE Hub.
type HubClusters struct {
	Resources []HubCluster
}

type HubCluster struct {
	// Full name - projects/wlhe-cr/locations/global/memberships/asm-cr
	//Name     string
	Endpoint *struct {
		GkeCluster *struct {
			// //container.googleapis.com/projects/wlhe-cr/locations/us-central1-c/clusters/asm-cr
			ResourceLink string
		}
		// kubernetesMetadata: vcpuCount, nodeCount, api version
	}
	State *struct {
		// READY
		Code string
	}

	Authority struct {
		Issuer               string `json:"issuer"`
		WorkloadIdentityPool string `json:"workloadIdentityPool"`
		IdentityProvider     string `json:"identityProvider"`
	} `json:"authority"`

	// Membership labels - different from GKE labels
	Labels map[string]string
}

// Init GCP auth
// Will init AuthProviders["gcp"].
//
// DefaultTokenSource will:
// - check GOOGLE_APPLICATION_CREDENTIALS
// - ~/.config/gcloud/application_default_credentials.json"
// - use metadata
//
// This also works for K8S, using node MDS or GKE MDS - but only if the
// ServiceAccount is annotated with a GSA (with permissions to use).
// Also specific to GKE and GCP APIs.
func InitDefaultTokenSource(ctx context.Context, uk *hbone.HBone) error {
	ts, err := google.DefaultTokenSource(ctx, "https://www.googleapis.com/auth/cloud-platform")
	if err != nil {
		sk, err := google.NewSDKConfig("")
		if err != nil {
			return err
		}
		// TODO: also try a
		ts = sk.TokenSource(ctx)
	}
	t := func(ctx context.Context, s string) (string, error) {
		t, err := ts.Token()
		if err != nil {
			return "", err
		}
		// TODO: cache, use expiry
		return t.AccessToken, nil
	}
	uk.AuthProviders["gcp"] = t
	return nil
}

// GKE2RestCluster gets all the clusters for a project, and returns Cluster object.
func GKE2RestCluster(ctx context.Context, uk *hbone.HBone, token string, p string) ([]*hbone.Cluster, error) {
	req, _ := http.NewRequest("GET", "https://container.googleapis.com/v1/projects/"+p+"/locations/-/clusters", nil)
	req = req.WithContext(ctx)
	if token != "" {
		req.Header.Add("authorization", "Bearer "+token)
	}

	res, err := uk.Client.Do(req)
	if res.StatusCode != 200 {
		rd, err := ioutil.ReadAll(res.Body)
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("%d %s", res.StatusCode, string(rd))
	}
	rd, err := ioutil.ReadAll(res.Body)
	if err != nil {
		return nil, err
	}
	if hbone.Debug {
		log.Println(string(rd))
	}

	cl := &Clusters{}
	err = json.Unmarshal(rd, cl)
	if err != nil {
		return nil, err
	}
	rcl := []*hbone.Cluster{}
	for _, c := range cl.Clusters {
		rc := &hbone.Cluster{
			Client: uk.HttpClient(c.MasterAuth.ClusterCaCertificate),
			CACert: string(c.MasterAuth.ClusterCaCertificate),
			// Endpoint is the IP typically
			Addr:          c.Endpoint + ":443",
			Location:      c.Location,
			TokenProvider: uk.AuthProviders["gcp"],
			Id:            "gke_" + p + "_" + c.Location + "_" + c.Name,
		}
		rcl = append(rcl, rc)

		uk.AddService(rc)
	}

	return rcl, err
}

// GetCluster returns a cluster config using the GKE API. Path must follow GKE API spec: /projects/P/locations/L/l
func GetCluster(ctx context.Context, uk *hbone.HBone, token, path string) (*hbone.Cluster, error) {
	req, _ := http.NewRequestWithContext(ctx, "GET", "https://container.googleapis.com/v1"+path, nil)
	req.Header.Add("authorization", "Bearer "+token)

	parts := strings.Split(path, "/")
	p := parts[2]
	res, err := uk.Client.Do(req)
	log.Println(res.StatusCode)
	rd, err := ioutil.ReadAll(res.Body)
	if err != nil {
		return nil, err
	}
	c := &Cluster{}
	err = json.Unmarshal(rd, c)
	if err != nil {
		return nil, err
	}

	rc := &hbone.Cluster{
		Client:        uk.HttpClient(c.MasterAuth.ClusterCaCertificate),
		CACert:        string(c.MasterAuth.ClusterCaCertificate),
		Addr:          c.Endpoint + ":443",
		Location:      c.Location,
		TokenProvider: uk.AuthProviders["gcp"],
		Id:            "gke_" + p + "_" + c.Location + "_" + c.Name,
	}

	return rc, err
}

func Hub2RestClusters(ctx context.Context, uk *hbone.HBone, tok, p string) ([]*hbone.Cluster, error) {
	req, _ := http.NewRequestWithContext(ctx, "GET",
		"https://gkehub.googleapis.com/v1/projects/"+p+"/locations/-/memberships", nil)
	req.Header.Add("authorization", "Bearer "+tok)

	res, err := uk.Client.Do(req)
	log.Println(res.StatusCode)
	rd, err := ioutil.ReadAll(res.Body)
	if err != nil {
		return nil, err
	}
	cl := []*hbone.Cluster{}
	log.Println(string(rd))
	if res.StatusCode == 403 {
		log.Println("Hub not authorized ", string(rd))
		// This is not considered an error - but user intent.
		return cl, nil
	}
	hcl := &HubClusters{}
	json.Unmarshal(rd, hcl)

	for _, hc := range hcl.Resources {
		// hc doesn't provide the endpoint. Need to query GKE - but instead of going over each cluster we can do
		// batch query on the project and filter.
		if hc.Endpoint != nil && hc.Endpoint.GkeCluster != nil {
			ca := hc.Endpoint.GkeCluster.ResourceLink
			if strings.HasPrefix(ca, "//container.googleapis.com") {
				rc, err := GetCluster(ctx, uk, tok, ca[len("//container.googleapis.com"):])
				if err != nil {
					log.Println("Failed to get ", ca, err)
				} else {
					uk.AddService(rc)
					cl = append(cl, rc)
				}
			}
		}
	}
	return cl, err
}

// Get a GCP secrets - used for bootstraping the credentials and provisioning.
//
// Example for creating a secret:
//
//	gcloud secrets create ca \
//	  --data-file <PATH-TO-SECRET-FILE> \
//	  --replication-policy automatic \
//	  --project dmeshgate \
//	  --format json \
//	  --quiet
func GcpSecret(ctx context.Context, uk *hbone.HBone, token, p, n, v string) ([]byte, error) {
	req, _ := http.NewRequestWithContext(ctx, "GET",
		"https://secretmanager.googleapis.com/v1/projects/"+p+"/secrets/"+n+
				"/versions/"+v+":access", nil)
	req.Header.Add("authorization", "Bearer "+token)

	res, err := uk.Client.Do(req)
	log.Println(res.StatusCode)
	rd, err := ioutil.ReadAll(res.Body)
	if err != nil {
		return nil, err
	}

	var s struct {
		Payload struct {
			Data []byte
		}
	}
	err = json.Unmarshal(rd, &s)
	if err != nil {
		return nil, err
	}
	return s.Payload.Data, err
}

// REST based interface with the CAs - to keep the binary size small.
// We just need to make 1 request at startup and maybe one per hour.

var (
	// access token for the p4sa.
	// Exchanged k8s token to p4sa access token.
	meshcaEndpoint = "https://meshca.googleapis.com:443/google.security.meshca.v1.MeshCertificateService/CreateCertificate"

	// JWT token with istio-ca or gke trust domain
	istiocaEndpoint = "/istio.v1.auth.IstioCertificateService/CreateCertificate"
)
