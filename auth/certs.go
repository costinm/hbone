package auth

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
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
	// Will attempt to load certificates from this directory.
	CertDir string

	// Primary certificate and private key. Loaded or generated.
	Cert *tls.Certificate

	// MeshTLSConfig is a tls.Config that requires mTLS with a spiffee identity,
	// using the configured roots, trustdomains.
	//
	// By default only same namespace or istio-system are allowed - can be changed by
	// setting AllowedNamespaces. A "*" will allow all.
	MeshTLSConfig *tls.Config

	// TrustDomain is extracted from the cert or set by user, used to verify
	// peer certificates. If not set, will be populated when cert is loaded.
	// Should be 'cluster.local', a real domain with OIDC keys or platform specific.
	TrustDomain string

	// Namespace and Name are extracted from the certificate or set by user.
	// Namespace is used to verify peer certificates
	Namespace string
	Name      string

	// Additional namespaces to allow access from. By default 'same namespace' and 'istio-system' are allowed.
	AllowedNamespaces []string

	// Trusted roots - used for verification. RawSubject is used as key - Subjects() return the DER list.
	// This is 'write only', used as a cache in verification.
	//
	// TODO: copy Istiod multiple trust domains code. This will be a map[trustDomain]roots and a
	// list of TrustDomains. XDS will return the info via ProxyConfig.
	// This can also be done by krun - loading a config map with same info.
	TrustedCertPool *x509.CertPool

	// Root CAs, in DER format. CertPool only stores, can't retrieve.
	// The key is generated as in envoy SPKI or cert pinning format
	//  openssl x509 -in path/to/client.crt -noout -pubkey
	//     | openssl pkey -pubin -outform DER
	//     | openssl dgst -sha256 -binary
	//     | openssl enc -base64
	RootCertificates map[string]*x509.Certificate

	// Private key to use in both server and client authentication.
	// ED22519: 32B
	// EC256: DER
	// RSA: DER
	Priv []byte

	// GetCertificateHook allows plugging in an alternative certificate provider.
	GetCertificateHook func(host string) (*tls.Certificate, error)
}

// NewMeshAuth creates the auth object.
// SetKeyPEM or SetKeysDir must be called to populate the key.
func NewMeshAuth() *MeshAuth {
	a := &MeshAuth{
		TrustedCertPool:  x509.NewCertPool(),
		RootCertificates: map[string]*x509.Certificate{},
	}
	return a
}

// mesh certificates - new style
const (
	WorkloadCertDir = "/var/run/secrets/workload-spiffe-credentials"

	// Different from typical Istio  and CertManager key.pem - we can check both
	privateKey = "private_key.pem"

	WorkloadRootCAs = "ca-certificates.crt"

	// Also different, we'll check all. CertManager uses cert.pem
	cert = "certificates.pem"

	// This is derived from CA certs plus all TrustAnchors.
	// In GKE, it is expected that Citadel roots will be configure using TrustConfig - so they are visible
	// to all workloads including TD proxyless GRPC.
	//
	// Outside of GKE, this is loaded from the mesh.env - the mesh gate is responsible to keep it up to date.
	rootCAs = "ca_certificates.pem"

	legacyCertDir = "/etc/certs"
)

// FindCerts will attempt to identify and load the certificates.
// - default GKE/Istio location for workload identity
// - /var/run/secrets/...FindC
// - /etc/istio/certs
// - $HOME/
func (a *MeshAuth) FindCerts() error {
	if a.CertDir != "" {
		return a.initFromDir()
	}
	if _, err := os.Stat(filepath.Join("./", "key.pem")); !os.IsNotExist(err) {
		a.CertDir = legacyCertDir
	} else if _, err := os.Stat(filepath.Join(WorkloadCertDir, privateKey)); !os.IsNotExist(err) {
		a.CertDir = WorkloadCertDir
	} else if _, err := os.Stat(filepath.Join(legacyCertDir, "key.pem")); !os.IsNotExist(err) {
		a.CertDir = legacyCertDir
	} else if _, err := os.Stat(filepath.Join("/var/run/secrets/istio", "key.pem")); !os.IsNotExist(err) {
		a.CertDir = legacyCertDir
	}
	if a.CertDir != "" {
		return a.initFromDir()
	}

	return nil
}

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
	a.initFromCert()
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

	if a.Cert == nil {
		return &tls.Certificate{}, nil
	}
	return a.Cert, nil
}

func (a *MeshAuth) loadCertFromDir(dir string) (*tls.Certificate, error) {
	// Load cert from file
	keyFile := filepath.Join(dir, "key.pem")
	keyBytes, err := ioutil.ReadFile(keyFile)
	if err != nil {
		keyFile = filepath.Join(dir, privateKey)
		keyBytes, err = ioutil.ReadFile(keyFile)
	}
	if err != nil {
		return nil, err
	}
	certBytes, err := ioutil.ReadFile(filepath.Join(dir, "cert-chain.pem"))
	if err != nil {
		certBytes, err = ioutil.ReadFile(filepath.Join(dir, cert))
	}
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
	rootCertExtra, _ := ioutil.ReadFile(filepath.Join(a.CertDir, WorkloadRootCAs))
	if rootCertExtra != nil {
		err2 := a.AddRoots(rootCertExtra)
		if err2 != nil {
			return err2
		}
	}

	// If the certificate has a chain, use the last cert - similar with Istio
	if a.Cert != nil && len(a.Cert.Certificate) > 1 {
		last := a.Cert.Certificate[len(a.Cert.Certificate)-1]

		rootCAs, err := x509.ParseCertificates(last)
		if err == nil {
			for _, c := range rootCAs {
				log.Println("Adding root CA from cert chain: ", c.Subject)
				a.TrustedCertPool.AddCert(c)
			}
		}
	}

	if a.Cert != nil {
		a.initFromCert()
	}
	return nil
}

// Return the SPKI fingerprint of the key
// https://www.rfc-editor.org/rfc/rfc7469#section-2.4
//
// Can be used with "ignore-certificate-errors-spki-list" in chrome
//
//	openssl x509 -pubkey -noout -in <path to PEM cert> | openssl pkey -pubin -outform der \
//	  | openssl dgst -sha256 -binary | openssl enc -base64
//
// sha256/BASE64
func SPKIFingerprint(key crypto.PublicKey) string {
	d := MarshalPublicKey(key)
	sum := sha256.Sum256(d)
	pin := make([]byte, base64.StdEncoding.EncodedLen(len(sum)))
	base64.StdEncoding.Encode(pin, sum[:])
	return string(pin)
}

func MarshalPublicKey(key crypto.PublicKey) []byte {
	if k, ok := key.(ed25519.PublicKey); ok {
		return []byte(k)
	}
	if k, ok := key.(*ecdsa.PublicKey); ok {
		return elliptic.Marshal(elliptic.P256(), k.X, k.Y)
		// starts with 0x04 == uncompressed curve
	}
	if k, ok := key.(*rsa.PublicKey); ok {
		bk := x509.MarshalPKCS1PublicKey(k)
		return bk
	}
	if k, ok := key.([]byte); ok {
		if len(k) == 64 || len(k) == 32 {
			return k
		}
	}

	return nil
}

// MarshalPrivateKey returns the PEM encoding of the key
func MarshalPrivateKey(priv crypto.PrivateKey) []byte {
	switch k := priv.(type) {
	case *rsa.PrivateKey:
		encodedKey := x509.MarshalPKCS1PrivateKey(k)
		return pem.EncodeToMemory(&pem.Block{Type: blockTypeRSAPrivateKey, Bytes: encodedKey})
	case *ecdsa.PrivateKey:
		encodedKey, _ := x509.MarshalECPrivateKey(k)
		return pem.EncodeToMemory(&pem.Block{Type: blockTypeECPrivateKey, Bytes: encodedKey})
	case *ed25519.PrivateKey:
	}
	// TODO: ed25529

	return nil
}

// SaveCerts will create certificate files as expected by gRPC and Istio, similar with the auto-created files.
func (a *MeshAuth) SaveCerts(outDir string) error {
	if outDir == "" {
		outDir = WorkloadCertDir
	}
	err := os.MkdirAll(outDir, 0755)
	// TODO: merge other roots as needed - this is Istio XDS server root.
	rootFile := filepath.Join(outDir, WorkloadRootCAs)
	if err != nil {
		return err
	}
	roots := ""
	err = ioutil.WriteFile(rootFile, []byte(roots), 0644)
	if err != nil {
		return err
	}

	keyFile := filepath.Join(outDir, privateKey)
	chainFile := filepath.Join(outDir, cert)
	os.MkdirAll(outDir, 0755)

	p := MarshalPrivateKey(a.Cert.PrivateKey)

	// TODO: full chain
	b := pem.EncodeToMemory(
		&pem.Block{
			Type:  "CERTIFICATE",
			Bytes: a.Cert.Certificate[0],
		},
	)

	err = ioutil.WriteFile(keyFile, p, 0660)
	if err != nil {
		return err
	}
	err = ioutil.WriteFile(chainFile, b, 0660)
	if err != nil {
		return err
	}
	if os.Getuid() == 0 {
		os.Chown(outDir, 1337, 1337)
		os.Chown(keyFile, 1337, 1337)
		os.Chown(chainFile, 1337, 1337)
	}

	return nil
}

// Common setup for cert management.
// After the 'mesh-env' is loaded (from env, k8s, URL) the next step is to init the workload identity.
// This must happen before connecting to XDS - since certs is one of the possible auth methods.
//
// The logic is:
//   - (best case) certificates already provisioned by platform. Detects GKE paths (CAS), old Istio, CertManager style
//     If workload certs are platform-provisioned: extract trust domain, namespace, name, pod id from cert.
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

// Extract the trustDomain, namespace and Name from a spiffee certificate
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

func (a *MeshAuth) AddRootCert(c *x509.Certificate) error {
	pubDER, err := x509.MarshalPKIXPublicKey(c.PublicKey.(*rsa.PublicKey))
	if err != nil {
		return err
	}
	sum := sha256.Sum256(pubDER)
	pin := make([]byte, base64.StdEncoding.EncodedLen(len(sum)))
	base64.StdEncoding.Encode(pin, sum[:])
	a.RootCertificates[string(pin)] = c
	a.TrustedCertPool.AddCert(c)
	return nil
}

// AddRoots will process a PEM file containing multiple concatenated certificates.
func (a *MeshAuth) AddRoots(rootCertPEM []byte) error {
	block, rest := pem.Decode(rootCertPEM)
	//var blockBytes []byte
	for block != nil {
		rootCAs, err := x509.ParseCertificates(block.Bytes)
		if err != nil {
			return err
		}
		for _, c := range rootCAs {
			err = a.AddRootCert(c)
			if err != nil {
				return err
			}
		}

		//blockBytes = append(blockBytes, block.Bytes...)
		block, rest = pem.Decode(rest)
	}

	//rootCAs, err := x509.ParseCertificates(blockBytes)
	//if err != nil {
	//	return err
	//}
	//for _, c := range rootCAs {
	//	a.TrustedCertPool.AddCert(c)
	//}
	return nil
}

func (a *MeshAuth) GenerateTLSConfigClient(name string) *tls.Config {
	return a.GenerateTLSConfigClientRoots(name, nil)
}

// GenerateTLSConfigClient will provide mesh client certificate config.
// TODO: SecureNaming or expected SANs
func (a *MeshAuth) GenerateTLSConfigClientRoots(name string, pool *x509.CertPool) *tls.Config {
	return &tls.Config{
		//MinVersion: tls.VersionTLS13,
		//PreferServerCipherSuites: ugate.preferServerCipherSuites(),

		ServerName: name,
		GetCertificate: func(ch *tls.ClientHelloInfo) (*tls.Certificate, error) {
			return a.GetCertificate(ch.Context(), ch.ServerName)
		},

		GetClientCertificate: func(cri *tls.CertificateRequestInfo) (*tls.Certificate, error) {
			return a.GetCertificate(cri.Context(), "")
		},
		NextProtos: []string{"h2"},

		InsecureSkipVerify: true, // This is not insecure here. We will verify the cert chain ourselves.
		VerifyPeerCertificate: func(rawCerts [][]byte, verifiedChains [][]*x509.Certificate) error {
			return a.verifyServerCert(name, rawCerts, verifiedChains, pool)
		},
	}
}
func (a *MeshAuth) VerifyServerCert(rawCerts [][]byte, all [][]*x509.Certificate) error {
	return a.verifyServerCert("", rawCerts, all, nil)
}

// Verify the server certificate. The client TLS context is called with InsecureSkipVerify,
// so 'normal' verification is disabled - only rawCerts are available.
//
// Will check:
// - certificate is valid
// - if it has a Spiffee identity - verify it is in same namespace or istio-system
// - else: verify it matches SNI (unless sni is empty). For DNS only will use provided pool or root certs
func (a *MeshAuth) verifyServerCert(sni string, rawCerts [][]byte, _ [][]*x509.Certificate, pool *x509.CertPool) error {
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

		if pool == nil {
			pool = a.TrustedCertPool
		}
		_, err := peerCert.Verify(x509.VerifyOptions{
			Roots:         pool,
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
		// No DomainName since we verified spiffee
		if pool == nil {
			_, err := peerCert.Verify(x509.VerifyOptions{
				Roots:         nil, // use system
				Intermediates: intCertPool,
			})
			if err != nil {
				_, err = peerCert.Verify(x509.VerifyOptions{
					Roots:         a.TrustedCertPool,
					Intermediates: intCertPool,
				})
			}
			if err != nil {
				return err
			}
		} else {
			_, err := peerCert.Verify(x509.VerifyOptions{
				Roots:         pool,
				Intermediates: intCertPool,
			})
			if err != nil {
				return err
			}
		}
		if sni == "" {
			return nil
		}

		if len(peerCert.DNSNames) > 0 {
			err := peerCert.VerifyHostname(sni)
			if err == nil {
				return nil
			}
		}
		//for _, n := range peerCert.DNSNames {
		//	if n == sni {
		//		return nil
		//	}
		//}
		for _, n := range peerCert.IPAddresses {
			if n.String() == sni {
				return nil
			}
		}
		// DNS certificate - need to verify the name separatedly.
		return errors.New("dns cert not found " + sni)
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

// initFromCert initializes the MeshTLSConfig with the workload certificate
func (a *MeshAuth) initFromCert() {
	_, a.TrustDomain, a.Namespace, a.Name = a.Spiffee()

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
