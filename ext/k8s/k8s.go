package k8s

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/costinm/hbone"
)

// To keep things simple and dependency-free, this is a copy of few structs
// used in K8S and Istio.
//
// K8S stores the .json format in CRDS - which is what this package uses directly.
//
// Changes:
// - only a subset, few unused fields commented out
// - enums converted to 'string', to make json conversion easy.

func Request(ctx context.Context, uk8s *hbone.Cluster, ns, kind, name string, postdata []byte) *http.Request {
	path := fmt.Sprintf("/api/v1/namespaces/%s/%ss/%s",
		ns, kind, name)
	var req *http.Request
	if postdata == nil {
		req, _ = http.NewRequestWithContext(ctx, "GET", "https://"+uk8s.Addr+uk8s.Path+path, nil)
	} else {
		req, _ = http.NewRequestWithContext(ctx, "POST", "https://"+uk8s.Addr+uk8s.Path+path, bytes.NewReader(postdata))
	}
	ct := "application/json"
	req.Header.Add("content-type", ct)

	return req
}

// Wrapper around Secret - returns the data content
func GetSecret(ctx context.Context, uk8s *hbone.Cluster, ns string, name string) (map[string][]byte, error) {
	data, err := uk8s.DoRequest(Request(ctx, uk8s, ns, "secret", name, nil))
	if err != nil {
		return nil, err
	}
	var secret Secret
	err = json.Unmarshal(data, &secret)
	if err != nil {
		return nil, err
	}

	return secret.Data, nil
}

// Wrapper around ConfigMap - returns the data content.
func GetConfigMap(ctx context.Context, uK8S *hbone.Cluster, ns string, name string) (map[string]string, error) {
	data, err := uK8S.DoRequest(Request(ctx, uK8S, ns, "configmap", name, nil))
	if err != nil {
		return nil, err
	}
	var secret ConfigMap
	err = json.Unmarshal(data, &secret)
	if err != nil {
		return nil, err
	}

	return secret.Data, nil
}

// K8STokenSource returns K8S JWTs via "/token" requests.
// TODO: or file-mounted secrets
type K8STokenSource struct {
	Cluster *hbone.Cluster

	// Namespace and KSA - the 'cluster' credentials must have the RBAC permissions.
	Namespace, KSA string

	// Force this audience instead of derived from request URI.
	AudOverride string
}

func NewK8STokenSource() *K8STokenSource {
	return &K8STokenSource{}
}

func (ts *K8STokenSource) GetToken(ctx context.Context, aud string) (string, error) {
	if ts.AudOverride != "" {
		aud = ts.AudOverride
	}

	// TODO: file based access, using /var/run/secrets/ file pattern and mounts.
	// TODO: Exec access, using /usr/lib/google-cloud-sdk/bin/gke-gcloud-auth-plugin (11M) for example
	//  name: gke_costin-asm1_us-central1-c_td1
	//  user:
	//    exec:
	//      apiVersion: client.authentication.k8s.io/v1beta1
	//      command: gke-gcloud-auth-plugin
	//      installHint: Install gke-gcloud-auth-plugin for use with kubectl by following
	//        https://cloud.google.com/blog/products/containers-kubernetes/kubectl-auth-changes-in-gke
	//      provideClusterInfo: true

	// /usr/lib/google-cloud-sdk/bin/gke-gcloud-auth-plugin
	// {
	//    "kind": "ExecCredential",
	//    "apiVersion": "client.authentication.k8s.io/v1beta1",
	//    "spec": {
	//        "interactive": false
	//    },
	//    "status": {
	//        "expirationTimestamp": "2022-07-01T15:55:01Z",
	//        "token": "ya29.a0ARrdaM_iVZGW8jWHQihYP72gZkOLcy8ZDOKWWNjHn45Xvv9P6K5-RTV_qcaUQBav_d3aijjyrVHSWdsruSEOWDlpoDAd56fXg18hxLVMiie63JJXkJPT9XlStccJuX4KLl-K0WKI1dQwySdQ6x9c-1b3YS108p3-CEL-HKA"
	//    }
	//}
	return GetTokenRaw(ctx, ts.Cluster, ts.Namespace, ts.KSA, aud)

}

// GetTokenRaw returns a K8S JWT with specified namespace, name and audience. Caller must have the RBAC
// permission to act as the name.ns.
//
// Equivalent curl request:
//
//	token=$(echo '{"kind":"TokenRequest","apiVersion":"authentication.k8s.io/v1","spec":{"audiences":["istio-ca"], "expirationSeconds":2592000}}' | \
//	   kubectl create --raw /api/v1/namespaces/default/serviceaccounts/default/token -f - | jq -j '.status.token')
func GetTokenRaw(ctx context.Context, uK8S *hbone.Cluster, ns, name, aud string) (string, error) {
	// If no audience is specified, something like
	//   https://container.googleapis.com/v1/projects/costin-asm1/locations/us-central1-c/clusters/big1
	// is generated ( on GKE ) - which seems to be the audience for K8S
	if ns == "" {
		ns = "default"
	}
	if name == "" {
		name = "default"
	}

	body := []byte(fmt.Sprintf(`
{"kind":"TokenRequest","apiVersion":"authentication.k8s.io/v1","spec":{"audiences":["%s"]}}
`, aud))
	data, err := uK8S.DoRequest(Request(ctx, uK8S, ns, "serviceaccount", name+"/token", body))

	if err != nil {
		return "", err
	}
	var secret CreateTokenResponse
	err = json.Unmarshal(data, &secret)
	if err != nil {
		return "", err
	}

	return secret.Status.Token, nil
}
