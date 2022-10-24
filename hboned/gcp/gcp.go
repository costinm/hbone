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

// Equivalent config using shell:
//
//```shell
//CMD="gcloud container clusters describe ${CLUSTER} --zone=${ZONE} --project=${PROJECT}"
//
//K8SURL=$($CMD --format='value(endpoint)')
//K8SCA=$($CMD --format='value(masterAuth.clusterCaCertificate)' )
//```
//
//```yaml
//apiVersion: v1
//kind: Config
//current-context: my-cluster
//contexts: [{name: my-cluster, context: {cluster: cluster-1, user: user-1}}]
//users: [{name: user-1, user: {auth-provider: {name: gcp}}}]
//clusters:
//- name: cluster-1
//  cluster:
//    server: "https://${K8SURL}"
//    certificate-authority-data: "${K8SCA}"
//
//```

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
			ID:            "gke_" + p + "_" + c.Location + "_" + c.Name,
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
		ID:            "gke_" + p + "_" + c.Location + "_" + c.Name,
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
)
