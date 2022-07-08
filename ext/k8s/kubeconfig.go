package k8s

// Kubeconfig support, for bootstraping from .kubeconfig or in-cluser k8s.

// Auth storage is loosely based on K8S kube.config:
// - no yaml - all files must be converted to JSON ( to avoid dep on yaml lib )
//   Caller can read yaml and convert before calling this.
// - default config is kube.json
// - expects that the first user is the 'default', has client cert
// - 'context' is the used to locate the URL, cert, user.

// "Clusters" are known nodes.
// ID can be the public-key based ID, or a hostname or "default"
//
// "User" has credentials - "default" is the workload ID.
//
// "Context" pairs a Cluster with a User.
// "default" is the upstream / primary server
//

// The reasons:
// - intend to have light integration with K8S port forward
// - very few other formats allow expressing this info,
// didn't want to invent a new one.

// KubeConfig is the JSON representation of the kube config.
// The format supports most of the things we need and also allows connection to real k8s clusters.
// UGate implements a very light subset - should be sufficient to connect to K8S, but without any
// generated stubs. Based in part on https://github.com/ericchiang/k8s (abandoned), which is a light
// client.
type KubeConfig struct {
	// Must be v1
	ApiVersion string `json:"apiVersion"`
	// Must be Config
	Kind string `json:"kind"`

	// Clusters is a map of referencable names to cluster configs
	Clusters []KubeNamedCluster `json:"clusters"`

	// AuthInfos is a map of referencable names to user configs
	Users []KubeNamedUser `json:"users"`

	// Contexts is a map of referencable names to context configs
	Contexts []KubeNamedContext `json:"contexts"`

	// CurrentContext is the name of the context that you would like to use by default
	CurrentContext string `json:"current-context" yaml:"current-context"`
}

type KubeNamedCluster struct {
	Name    string      `json:"name"`
	Cluster KubeCluster `json:"cluster"`
}
type KubeNamedUser struct {
	Name string   `json:"name"`
	User KubeUser `json:"user"`
}
type KubeNamedContext struct {
	Name    string  `json:"name"`
	Context Context `json:"context"`
}

type KubeCluster struct {
	// LocationOfOrigin indicates where this object came from.  It is used for round tripping config post-merge, but never serialized.
	// +k8s:conversion-gen=false
	//LocationOfOrigin string
	// Server is the address of the kubernetes cluster (https://hostname:port).
	Server string `json:"server"`
	// InsecureSkipTLSVerify skips the validity check for the server's certificate. This will make your HTTPS connections insecure.
	// +optional
	InsecureSkipTLSVerify bool `json:"insecure-skip-tls-verify,omitempty"`
	// CertificateAuthority is the path to a cert file for the certificate authority.
	// +optional
	CertificateAuthority string `json:"certificate-authority,omitempty" yaml:"certificate-authority"`
	// CertificateAuthorityData contains PEM-encoded certificate authority certificates. Overrides CertificateAuthority
	// +optional
	CertificateAuthorityData string `json:"certificate-authority-data,omitempty"  yaml:"certificate-authority-data"`
	// Extensions holds additional information. This is useful for extenders so that reads and writes don't clobber unknown fields
	// +optional
	//Extensions map[string]runtime.Object `json:"extensions,omitempty"`
}

// KubeUser contains information that describes identity information.  This is use to tell the kubernetes cluster who you are.
type KubeUser struct {
	// LocationOfOrigin indicates where this object came from.  It is used for round tripping config post-merge, but never serialized.
	// +k8s:conversion-gen=false
	//LocationOfOrigin string
	// ClientCertificate is the path to a client cert file for TLS.
	// +optional
	ClientCertificate string `json:"client-certificate,omitempty"`
	// ClientCertificateData contains PEM-encoded data from a client cert file for TLS. Overrides ClientCertificate
	// +optional
	ClientCertificateData []byte `json:"client-certificate-data,omitempty"`
	// ClientKey is the path to a client key file for TLS.
	// +optional
	ClientKey string `json:"client-key,omitempty"`
	// ClientKeyData contains PEM-encoded data from a client key file for TLS. Overrides ClientKey
	// +optional
	ClientKeyData []byte `json:"client-key-data,omitempty"`
	// Token is the bearer token for authentication to the kubernetes cluster.
	// +optional
	Token string `json:"token,omitempty"`
	// TokenFile is a pointer to a file that contains a bearer token (as described above).  If both Token and TokenFile are present, Token takes precedence.
	// +optional
	TokenFile string `json:"tokenFile,omitempty"`
	// Impersonate is the username to act-as.
	// +optional
	//Impersonate string `json:"act-as,omitempty"`
	// ImpersonateGroups is the groups to imperonate.
	// +optional
	//ImpersonateGroups []string `json:"act-as-groups,omitempty"`
	// ImpersonateUserExtra contains additional information for impersonated user.
	// +optional
	//ImpersonateUserExtra map[string][]string `json:"act-as-user-extra,omitempty"`
	// Username is the username for basic authentication to the kubernetes cluster.
	// +optional
	Username string `json:"username,omitempty"`
	// Password is the password for basic authentication to the kubernetes cluster.
	// +optional
	Password string `json:"password,omitempty"`
	// AuthProvider specifies a custom authentication plugin for the kubernetes cluster.
	// +optional
	AuthProvider UserAuthProvider `json:"auth-provider,omitempty" yaml:"auth-provider,omitempty"`
	// Exec specifies a custom exec-based authentication plugin for the kubernetes cluster.
	// +optional
	//Exec *ExecConfig `json:"exec,omitempty"`
	// Extensions holds additional information. This is useful for extenders so that reads and writes don't clobber unknown fields
	// +optional
	//Extensions map[string]runtime.Object `json:"extensions,omitempty"`
}

type UserAuthProvider struct {
	Name string `json:"name,omitempty"`
}

// Context is a tuple of references to a cluster (how do I communicate with a kubernetes cluster), a user (how do I identify myself), and a namespace (what subset of resources do I want to work with)
type Context struct {
	// Cluster is the name of the cluster for this context
	Cluster string `json:"cluster"`
	// AuthInfo is the name of the authInfo for this context
	User string `json:"user"`
	// Namespace is the default namespace to use on unspecified requests
	// +optional
	Namespace string `json:"namespace,omitempty"`
}
