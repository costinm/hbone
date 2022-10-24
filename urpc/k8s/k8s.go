package k8s

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"os"

	"github.com/costinm/hbone"
	auth "github.com/costinm/meshauth"
	"gopkg.in/yaml.v3"
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
// Returns an error if map can't be parsed or request fails.
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

// InitK8S will detect k8s env, and if present will load the mesh defaults and init
// authenticators.
func InitK8S(ctx context.Context, hb *hbone.HBone) (*hbone.Cluster, error) {

	def, err := initKubeconfig(ctx, hb)
	if err != nil {
		return nil, err
	}

	if def == nil {
		def, err = inCluster(ctx, hb)
	}
	if err != nil {
		return nil, err
	}

	if def == nil {
		// Not in K8S env
		return nil, nil
	}

	// Found a K8S cluster, try to locate configs in K8S by getting a config map containing Istio properties
	cm, err := GetConfigMap(ctx, def, "istio-system", "mesh-env")
	if err == nil {
		// Tokens using istio-ca audience for Istio
		// If certificates exist, namespace/sa are initialized from the cert SAN
		for k, v := range cm {
			hb.Env[k] = v
		}
	} else {
		log.Println("Invalid mesh-env config map", err)
		return nil, err
	}

	// Tokens using istio-ca audience for Istio
	catokenS := &K8STokenSource{Cluster: def, AudOverride: "istio-ca", Namespace: hb.Namespace, KSA: hb.ServiceAccount}
	hb.AuthProviders["istio-ca"] = catokenS.GetToken

	// Init a GCP token source - using K8S provider and exchange.
	// TODO: if we already have a GCP GSA, we can use that directly.
	projectNumber := hb.GetEnv("PROJECT_NUMBER", "")
	projectId := hb.GetEnv("PROJECT_ID", "")
	clusterLocation := hb.GetEnv("CLUSTER_LOCATION", "")
	clusterName := hb.GetEnv("CLUSTER_NAME", "")

	if projectId != "" && clusterName != "" && clusterLocation != "" && projectNumber != "" {
		// This returns JWT tokens for k8s
		//audTokenS := k8s.K8STokenSource{Cluster: k8sdefault, Namespace: hb.Namespace,
		//	KSA: hb.ServiceAccount}
		audTokenS := auth.NewGSATokenSource(&auth.AuthConfig{
			ProjectNumber: projectNumber,
			TrustDomain:   projectId + ".svc.id.goog",
			ClusterAddress: fmt.Sprintf("https://container.googleapis.com/v1/projects/%s/locations/%s/clusters/%s",
				projectId, clusterLocation, clusterName),
			TokenSource: &K8STokenSource{Cluster: def, Namespace: hb.Namespace,
				KSA: hb.ServiceAccount},
		}, "")
		hb.AuthProviders["gsa"] = audTokenS.GetToken

		sts := auth.NewFederatedTokenSource(&auth.AuthConfig{
			TrustDomain: projectId + ".svc.id.goog",
			ClusterAddress: fmt.Sprintf("https://container.googleapis.com/v1/projects/%s/locations/%s/clusters/%s",
				projectId, clusterLocation, clusterName),

			// Will use TokenRequest to get tokens with AudOverride
			TokenSource: &K8STokenSource{
				Cluster:     def,
				AudOverride: projectId + ".svc.id.goog",
				Namespace:   hb.Namespace,
				KSA:         hb.ServiceAccount},
		})
		hb.AuthProviders["sts"] = sts.GetToken
	}
	return def, nil
}

func initKubeconfig(ctx context.Context, uk *hbone.HBone) (*hbone.Cluster, error) {
	kc := os.Getenv("KUBECONFIG")
	if kc == "" {
		kc = os.Getenv("HOME") + "/.kube/config"
	}
	var kcd []byte
	if kc != "" {
		if _, err := os.Stat(kc); err == nil {
			// Explicit kube config, using it.
			kcd, err = ioutil.ReadFile(kc)
			if err != nil {
				return nil, err
			}
		} else {
			return nil, nil
		}
	}

	kconf := &KubeConfig{}
	err := yaml.Unmarshal(kcd, kconf)
	if err != nil {
		return nil, err
	}

	def, _, err := AddKubeConfigClusters(uk, kconf)
	if err != nil {
		return nil, err
	}

	return def, nil
}

func inCluster(ctx context.Context, uk *hbone.HBone) (*hbone.Cluster, error) {
	host := os.Getenv("KUBERNETES_SERVICE_HOST")
	if host != "" {
		const (
			tokenFile  = "/var/run/secrets/kubernetes.io/serviceaccount/token"
			rootCAFile = "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"
		)
		host, port := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT")
		if len(host) == 0 || len(port) == 0 {
			return nil, nil
		}

		token, err := ioutil.ReadFile(tokenFile)
		if err != nil {
			return nil, err
		}

		namespace, err := ioutil.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace")
		if err == nil {
			uk.Namespace = string(namespace)
		}

		jwt := auth.DecodeJWT(string(token))
		if uk.Namespace == "" {
			uk.Namespace = jwt.K8S.Namespace
		}
		if uk.ServiceAccount == "" {
			uk.ServiceAccount = jwt.Name
		}

		ca, err := ioutil.ReadFile(rootCAFile)
		c := &hbone.Cluster{
			Addr:   net.JoinHostPort(host, port),
			ID:     "k8s",
			Token:  string(token),
			CACert: string(ca),
		}

		uk.Clusters["k8s"] = c
		return c, nil
	}
	return nil, nil
}
