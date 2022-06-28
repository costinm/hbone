package auth

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io/ioutil"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// This handles the ideal mesh case - platform managed workload certificates.
// Also handles pilot-agent managing the certs.

// TLS notes:
// - using HandshakeContext with custom verification for workload identity (spiffe)
//   - instead of x509.VerifyOptions{DNSName} - based on ServerName from the config, which
//     can be overriden by client.
//
// - native library also supports nested TLS - if the Dial method is overriden and scheme is https,
//    it will do a TLS handshake anyway and Dial can implement TLS for the outer tunnel.

// MeshAuth represents a workload identity and associated info required for minimal Mesh-compatible security.
type MeshAuth struct {
	// Will attempt to load certificates from this directory, defaults to
	// "./var/run/secrets/istio.io/"
	CertDir string

	// Current certificate, after calling GetCertificate("")
	Cert *tls.Certificate

	// MeshTLSConfig is a tls.Config that requires mTLS with a spiffee identity,
	// using the configured roots, trustdomains.
	//
	// By default only same namespace or istio-system are allowed - can be changed by
	// setting AllowedNamespaces. A "*" will allow all.
	MeshTLSConfig *tls.Config

	// TrustDomain is extracted from the cert or set by user, used to verify
	// peer certificates.
	TrustDomain string

	// Namespace and SA are extracted from the certificate or set by user.
	// Namespace is used to verify peer certificates
	Namespace string
	SA        string

	// Additional namespaces to allow access from. By default 'same namespace' and 'istio-system' are allowed.
	AllowedNamespaces []string

	// Trusted roots
	// TODO: copy Istiod multiple trust domains code. This will be a map[trustDomain]roots and a
	// list of TrustDomains. XDS will return the info via ProxyConfig.
	// This can also be done by krun - loading a config map with same info.
	TrustedCertPool *x509.CertPool

	// GetCertificateHook allows plugging in an alternative certificate provider. By default files are used.
	GetCertificateHook func(host string) (*tls.Certificate, error)
}

// NewMeshAuth creates the auth object.
// SetKeyPEM or SetKeysDir must be called to populate the key.
func NewMeshAuth() *MeshAuth {
	a := &MeshAuth{
		TrustedCertPool: x509.NewCertPool(),
	}
	return a
}

// mesh certificates - new style
const (
	WorkloadCertDir = "/var/run/secrets/workload-spiffe-credentials"

	// Different from typical Istio  and CertManager key.pem - we can check both
	privateKey = "private_key.pem"

	// Also different, we'll check all. CertManager uses cert.pem
	cert = "certificates.pem"

	// This is derived from CA certs plus all TrustAnchors.
	// In GKE, it is expected that Citadel roots will be configure using TrustConfig - so they are visible
	// to all workloads including TD proxyless GRPC.
	//
	// Outside of GKE, this is loaded from the mesh.env - the mesh gate is responsible to keep it up to date.
	WorkloadRootCAs = "ca_certificates.pem"
)

// Will load the credentials and create an Auth object.
//
// This uses pilot-agent or some other platform tool creating ./var/run/secrets/istio.io/{key,cert-chain}.pem
// or /var/run/secrets/workload-spiffe-credentials
func (a *MeshAuth) SetKeysDir(dir string) error {
	a.CertDir = dir
	err := a.waitAndInitFromDir()
	if err != nil {
		return err
	}
	return nil
}

func (a *MeshAuth) SetKeysPEM(privatePEM []byte, chainPEM []string) error {
	chainPEMCat := strings.Join(chainPEM, "\n")
	tlsCert, err := tls.X509KeyPair([]byte(chainPEMCat), privatePEM)
	if err != nil {
		return err
	}
	if tlsCert.Certificate == nil || len(tlsCert.Certificate) == 0 {
		return errors.New("missing certificate")
	}

	return a.SetTLSCertificate(&tlsCert)
}

func (a *MeshAuth) SetTLSCertificate(cert *tls.Certificate) error {
	a.Cert = cert
	a.initTLS()
	return nil
}

func (a *MeshAuth) leaf() *x509.Certificate {
	if a.Cert == nil {
		return nil
	}
	if a.Cert.Leaf == nil {
		a.Cert.Leaf, _ = x509.ParseCertificate(a.Cert.Certificate[0])
	}
	return a.Cert.Leaf
}

// GetCertificate is typically called during handshake, both server and client.
// "sni" will be empty for client certificates, and set for server certificates - if not set, workload id is returned.
//
// ctx is the handshake context - may include additional metadata about the operation.
func (a *MeshAuth) GetCertificate(ctx context.Context, sni string) (*tls.Certificate, error) {
	// TODO: if host != "", allow returning DNS certs for the host.
	// Default (and currently only impl) is to return the spiffe cert
	// May refresh.

	// Have cert, not expired
	if a.Cert != nil {
		if !a.leaf().NotAfter.Before(time.Now()) {
			return a.Cert, nil
		}
	}

	if a.CertDir != "" {
		c, err := a.loadCertFromDir(a.CertDir)
		if err == nil {
			if !c.Leaf.NotAfter.Before(time.Now()) {
				a.Cert = c
			}
		} else {
			log.Println("Cert from dir failed", err)
		}
	}

	if a.GetCertificateHook != nil {
		c, err := a.GetCertificateHook(sni)
		if err != nil {
			return nil, err
		}
		a.Cert = c
	}

	return a.Cert, nil
}

func (a *MeshAuth) loadCertFromDir(dir string) (*tls.Certificate, error) {
	// Load cert from file
	keyFile := filepath.Join(dir, "key.pem")
	keyBytes, err := ioutil.ReadFile(keyFile)
	if err != nil {
		return nil, err
	}
	certBytes, err := ioutil.ReadFile(filepath.Join(dir, "cert-chain.pem"))
	if err != nil {
		return nil, err
	}

	tlsCert, err := tls.X509KeyPair(certBytes, keyBytes)
	if err != nil {
		return nil, err
	}
	if tlsCert.Certificate == nil || len(tlsCert.Certificate) == 0 {
		return nil, errors.New("missing certificate")
	}
	tlsCert.Leaf, _ = x509.ParseCertificate(tlsCert.Certificate[0])

	return &tlsCert, nil
}

func (a *MeshAuth) waitAndInitFromDir() error {
	if a.CertDir == "" {
		a.CertDir = "./var/run/secrets/istio.io/"
	}
	keyFile := filepath.Join(a.CertDir, "key.pem")
	err := waitFile(keyFile, 5*time.Second)
	if err != nil {
		return err
	}

	err = a.initFromDir()
	if err != nil {
		return err
	}

	time.AfterFunc(30*time.Minute, a.initFromDirPeriodic)
	return nil
}

func (a *MeshAuth) initFromDirPeriodic() {
	err := a.initFromDir()
	if err != nil {
		log.Println("certRefresh", err)
	}
	time.AfterFunc(30*time.Minute, a.initFromDirPeriodic)
}

func (a *MeshAuth) initFromDir() error {

	if a.Cert == nil {
		_, err := a.GetCertificate(context.Background(), "")
		if err != nil {
			return err
		}
	}

	rootCert, _ := ioutil.ReadFile(filepath.Join(a.CertDir, "root-cert.pem"))
	if rootCert != nil {
		err2 := a.AddRoots(rootCert)
		if err2 != nil {
			return err2
		}
	}

	istioCert, _ := ioutil.ReadFile("./var/run/secrets/istio/root-cert.pem")
	if istioCert != nil {
		err2 := a.AddRoots(istioCert)
		if err2 != nil {
			return err2
		}
	}

	// Similar with /etc/ssl/certs/ca-certificates.crt - the concatenated list of PEM certs.
	rootCertExtra, _ := ioutil.ReadFile(filepath.Join(a.CertDir, "ca-certificates.crt"))
	if rootCertExtra != nil {
		err2 := a.AddRoots(rootCertExtra)
		if err2 != nil {
			return err2
		}
	}
	// If the certificate has a chain, use the last cert - similar with Istio
	if len(a.Cert.Certificate) > 1 {
		last := a.Cert.Certificate[len(a.Cert.Certificate)-1]

		rootCAs, err := x509.ParseCertificates(last)
		if err == nil {
			for _, c := range rootCAs {
				log.Println("Adding root CA from cert chain: ", c.Subject)
				a.TrustedCertPool.AddCert(c)
			}
		}
	}

	a.initTLS()
	return nil
}

// InitRoots will find the mesh roots.
//
// - if Zatar or another CSI provider are enabled, we do nothing - Zatar config is the root of trust for everything
// - otherwise the roots are expected to be part of mesh-env. The mesh connector or other tools will
//  populate it - ideally from the CSI/Zatar or TrustConfig CRD.
func (kr *MeshAuth) InitRoots(ctx context.Context, outDir string) error {
	if outDir != "" {
		rootFile := filepath.Join(outDir, WorkloadRootCAs)
		rootCertPEM, err := ioutil.ReadFile(rootFile)
		if err == nil {
			block, rest := pem.Decode(rootCertPEM)

			var blockBytes []byte
			for block != nil {
				blockBytes = append(blockBytes, block.Bytes...)
				block, rest = pem.Decode(rest)
			}

			rootCAs, err := x509.ParseCertificates(blockBytes)
			if err != nil {
				return err
			}
			for _, c := range rootCAs {
				kr.TrustedCertPool.AddCert(c)
			}
			return nil
		}
	}

	// File not found - extract it from mesh env, and save it.
	// This includes Citadel root (if active in the mesh) or other roots.
	roots := ""
	block, rest := pem.Decode([]byte(roots))
	var blockBytes []byte
	for block != nil {
		blockBytes = append(blockBytes, block.Bytes...)
		block, rest = pem.Decode(rest)
	}

	rootCAs, err := x509.ParseCertificates(blockBytes)
	if err != nil {
		return err
	}
	for _, c := range rootCAs {
		kr.TrustedCertPool.AddCert(c)
	}

	return nil
}

// Common setup for cert management.
// After the 'mesh-env' is loaded (from env, k8s, URL) the next step is to init the workload identity.
// This must happen before connecting to XDS - since certs is one of the possible auth methods.
//
// The logic is:
// - (best case) certificates already provisioned by platform. Detects GKE paths (CAS), old Istio, CertManager style
//   If workload certs are platform-provisioned: extract trust domain, namespace, name, pod id from cert.
//
// - Detect the WORKLOAD_SERVICE_ACCOUNT, trust domain from JWT or mesh-env
// - Use WORKLOAD_CERT json to load the config for the CSR, create a CSR
// - Call CSRSigner.
// - Save the certificates if running as root or an output dir is set. This will use CAS naming convention.
//
// If envoy + pilot-agent are used, they should be configured to use the cert files.
// This is done by setting "CA_PROVIDER=GoogleGkeWorkloadCertificate" when starting pilot-agent
func (kr *MeshAuth) InitCertificates(ctx context.Context, certDir string) error {
	if certDir == "" {
		certDir = WorkloadCertDir
	}
	var err error
	keyFile := filepath.Join(certDir, privateKey)
	chainFile := filepath.Join(certDir, cert)
	privPEM, err := ioutil.ReadFile(keyFile)
	certPEM, err := ioutil.ReadFile(chainFile)

	kp, err := tls.X509KeyPair(certPEM, privPEM)
	if err == nil && len(kp.Certificate) > 0 {
		kr.CertDir = certDir

		kp.Leaf, _ = x509.ParseCertificate(kp.Certificate[0])

		exp := kp.Leaf.NotAfter.Sub(time.Now())
		if exp > -5*time.Minute {
			kr.Cert = &kp
			log.Println("Existing Cert", "expires", exp)
			return nil
		}
	}
	return nil
}

// Extract the trustDomain, namespace and SA from a spiffee certificate
func (a *MeshAuth) Spiffee() (*url.URL, string, string, string) {
	cert, err := x509.ParseCertificate(a.Cert.Certificate[0])
	if err != nil {
		return nil, "", "", ""
	}
	if len(cert.URIs) > 0 {
		c0 := cert.URIs[0]
		pathComponetns := strings.Split(c0.Path, "/")
		if c0.Scheme == "spiffe" && pathComponetns[1] == "ns" && pathComponetns[3] == "sa" {
			return c0, c0.Host, pathComponetns[2], pathComponetns[4]
		}
	}
	return nil, "", "", ""
}

func (a *MeshAuth) ID() string {
	su, _, _, _ := a.Spiffee()
	return su.String()
}

func (a *MeshAuth) String() string {
	cert, err := x509.ParseCertificate(a.Cert.Certificate[0])
	if err != nil {
		return ""
	}
	id := ""
	if len(cert.URIs) > 0 {
		id = cert.URIs[0].String()
	}
	return fmt.Sprintf("ID=%s,iss=%s,exp=%v,org=%s", id, cert.Issuer,
		cert.NotAfter, cert.Subject.Organization)
}

func (a *MeshAuth) AddRoots(rootCertPEM []byte) error {
	block, rest := pem.Decode(rootCertPEM)
	var blockBytes []byte
	for block != nil {
		blockBytes = append(blockBytes, block.Bytes...)
		block, rest = pem.Decode(rest)
	}

	rootCAs, err := x509.ParseCertificates(blockBytes)
	if err != nil {
		return err
	}
	for _, c := range rootCAs {
		a.TrustedCertPool.AddCert(c)
	}
	return nil
}

// GenerateTLSConfigClient will provide mesh client certificate config.
// TODO: SecureNaming or expected SANs
func (a *MeshAuth) GenerateTLSConfigClient(name string) *tls.Config {
	return &tls.Config{
		//MinVersion: tls.VersionTLS13,
		//PreferServerCipherSuites: ugate.preferServerCipherSuites(),
		InsecureSkipVerify: true,                  // This is not insecure here. We will verify the cert chain ourselves.
		ClientAuth:         tls.RequestClientCert, // not require - we'll fallback to JWT

		GetCertificate: func(ch *tls.ClientHelloInfo) (*tls.Certificate, error) {
			return a.GetCertificate(ch.Context(), ch.ServerName)
		},

		GetClientCertificate: func(cri *tls.CertificateRequestInfo) (*tls.Certificate, error) {
			return a.GetCertificate(cri.Context(), "")
		},
		NextProtos: []string{"istio", "h2"},

		VerifyPeerCertificate: a.VerifyServerCert,
	}
}

// Verify the server certificate. The client TLS context is called with InsecureSkipVerify,
// so 'normal' verification is disabled - only rawCerts are available.
//
// Will check:
// - certificate is valid
// -
func (a *MeshAuth) VerifyServerCert(rawCerts [][]byte, _ [][]*x509.Certificate) error {
	if len(rawCerts) == 0 {
		return errors.New("client certificate required")
	}
	var peerCert *x509.Certificate
	intCertPool := x509.NewCertPool()

	for id, rawCert := range rawCerts {
		cert, err := x509.ParseCertificate(rawCert)
		if err != nil {
			return err
		}
		if id == 0 {
			peerCert = cert
		} else {
			intCertPool.AddCert(cert)
		}
	}
	if peerCert == nil || len(peerCert.URIs) == 0 && len(peerCert.DNSNames) == 0 {
		return errors.New("peer certificate does not contain URI or DNS SANs")
	}

	if len(peerCert.URIs) > 0 {
		c0 := peerCert.URIs[0]

		// Verify the trust domain of the peer's is same.
		// TODO: aliases
		trustDomain := c0.Host
		if trustDomain != a.TrustDomain {
			log.Println("MTLS: invalid trust domain", trustDomain, peerCert.URIs)
			return errors.New("invalid trust domain " + trustDomain + " " + a.TrustDomain)
		}

		// No DomainName since we verified spiffee
		_, err := peerCert.Verify(x509.VerifyOptions{
			Roots:         a.TrustedCertPool,
			Intermediates: intCertPool,
		})
		if err != nil {
			return err
		}

		parts := strings.Split(c0.Path, "/")
		if len(parts) < 4 {
			log.Println("MTLS: invalid path", peerCert.URIs)
			return errors.New("invalid path " + c0.String())
		}

		ns := parts[2]
		if ns == "istio-system" || ns == a.Namespace {
			return nil
		}

		return nil
	} else {
		// DNS certificate

		return nil
	}
}

func (a *MeshAuth) VerifyClientCert(rawCerts [][]byte, _ [][]*x509.Certificate) error {
	if len(rawCerts) == 0 {
		log.Println("MTLS: missing client cert")
		return errors.New("client certificate required")
	}
	var peerCert *x509.Certificate
	intCertPool := x509.NewCertPool()

	for id, rawCert := range rawCerts {
		cert, err := x509.ParseCertificate(rawCert)
		if err != nil {
			return err
		}
		if id == 0 {
			peerCert = cert
		} else {
			intCertPool.AddCert(cert)
		}
	}
	if peerCert == nil || len(peerCert.URIs) == 0 {
		log.Println("MTLS: missing URIs in Istio cert", peerCert)
		return errors.New("peer certificate does not contain URI type SAN")
	}
	c0 := peerCert.URIs[0]
	trustDomain := c0.Host
	if trustDomain != a.TrustDomain {
		log.Println("MTLS: invalid trust domain", trustDomain, peerCert.URIs)
		return errors.New("invalid trust domain " + trustDomain + " " + a.TrustDomain)
	}

	_, err := peerCert.Verify(x509.VerifyOptions{
		Roots:         a.TrustedCertPool,
		Intermediates: intCertPool,
	})
	if err != nil {
		return err
	}

	parts := strings.Split(c0.Path, "/")
	if len(parts) < 4 {
		log.Println("MTLS: invalid path", peerCert.URIs)
		return errors.New("invalid path " + c0.String())
	}

	ns := parts[2]
	if ns == "istio-system" || ns == a.Namespace {
		return nil
	}

	// TODO: also validate namespace is same with this workload or in list of namespaces ?
	if len(a.AllowedNamespaces) == 0 {
		log.Println("MTLS: namespace not allowed", peerCert.URIs)
		return errors.New("Namespace not allowed")
	}

	if a.AllowedNamespaces[0] == "*" {
		return nil
	}

	for _, ans := range a.AllowedNamespaces {
		if ns == ans {
			return nil
		}
	}

	log.Println("MTLS: namespace not allowed", peerCert.URIs)
	return errors.New("Namespace not allowed")
}

// GenerateTLSConfigServer is used to provide the server tls.Config for handshake.
// Will use the workload identity and do basic checks on client certs.
func (a *MeshAuth) GenerateTLSConfigServer() *tls.Config {
	return &tls.Config{
		//MinVersion: tls.VersionTLS13,
		//PreferServerCipherSuites: ugate.preferServerCipherSuites(),
		InsecureSkipVerify: true,                  // This is not insecure here. We will verify the cert chain ourselves.
		ClientAuth:         tls.RequestClientCert, // not require - we'll fallback to JWT

		GetCertificate: func(ch *tls.ClientHelloInfo) (*tls.Certificate, error) {
			return a.GetCertificate(ch.Context(), ch.ServerName)
		},

		GetClientCertificate: func(cri *tls.CertificateRequestInfo) (*tls.Certificate, error) {
			// The server can include a list of 'acceptable CAs' - as list of DER-encoded DN
			return a.GetCertificate(cri.Context(), "")
		},

		// Will check the peer certificate, using the trust roots.
		VerifyPeerCertificate: a.VerifyClientCert,

		NextProtos: []string{"istio", "h2"},
	}
}

// initTLS initializes the MeshTLSConfig with the workload certificate
func (a *MeshAuth) initTLS() {
	_, a.TrustDomain, a.Namespace, a.SA = a.Spiffee()

	a.MeshTLSConfig = a.GenerateTLSConfigServer()
}

// waitFile will check for the file to show up - the agent is running in a separate process.
func waitFile(keyFile string, d time.Duration) error {
	t0 := time.Now()
	var err error
	for {
		// Wait for key file to show up - pilot agent creates it.
		if _, err := os.Stat(keyFile); os.IsNotExist(err) {
			if time.Since(t0) > d {
				return err
			}
			time.Sleep(50 * time.Millisecond)
			continue
		}
		return nil
	}

	return err
}
