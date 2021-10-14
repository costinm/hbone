package hbone

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"io/ioutil"
	"log"
	"math/big"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type Auth struct {
	CertDir   string
	Cert      *tls.Certificate
	TLSConfig *tls.Config

	// Namespace and SA are extracted from the certificate.
	Namespace   string
	SA          string
	TrustDomain string

	// Trusted roots
	// TODO: copy Istiod multiple trust domains code. This will be a map[trustDomain]roots and a
	// list of TrustDomains. XDS will return the info via ProxyConfig.
	// This can also be done by krun - loading a config map with same info.
	TrustedCertPool *x509.CertPool
}

// TODO: ./etc/certs support: krun should copy the files, for consistency (simper code for frameworks).
// TODO: periodic reload
func LoadAuth(dir string) (*Auth, error) {
	a := &Auth{
		CertDir: dir,
	}
	err := a.InitKeys()
	if err != nil {
		return nil, err
	}
	return a, nil
}

func (hb *Auth) InitKeys() error {
	if hb.CertDir == "" {
		hb.CertDir = "./var/run/secrets/istio.io/"
	}
	if hb.Cert == nil {
		keyFile := filepath.Join(hb.CertDir, "key.pem")
		err := WaitFile(keyFile, 5*time.Second)
		if err != nil {
			return err
		}

		keyBytes, err := ioutil.ReadFile(keyFile)
		if err != nil {
			return err
		}
		certBytes, err := ioutil.ReadFile(filepath.Join(hb.CertDir, "cert-chain.pem"))
		if err != nil {
			return err
		}
		tlsCert, err := tls.X509KeyPair(certBytes, keyBytes)
		if err != nil {
			return err
		}
		hb.Cert = &tlsCert
		if tlsCert.Certificate == nil || len(tlsCert.Certificate) == 0 {
			return errors.New("missing certificate")
		}
	}

	if hb.TrustedCertPool == nil {
		hb.TrustedCertPool = x509.NewCertPool()
	}
	// TODO: multiple roots
	rootCert, _ := ioutil.ReadFile(filepath.Join(hb.CertDir, "root-cert.pem"))
	if rootCert != nil {
		err2 := hb.AddRoots(rootCert)
		if err2 != nil {
			return err2
		}
	}

	istioCert, _ := ioutil.ReadFile("./var/run/secrets/istio/root-cert.pem")
	if istioCert != nil {
		err2 := hb.AddRoots(istioCert)
		if err2 != nil {
			return err2
		}
	}

	// Similar with /etc/ssl/certs/ca-certificates.crt - the concatenated list of PEM certs.
	rootCertExtra, _ := ioutil.ReadFile(filepath.Join(hb.CertDir, "ca-certificates.crt"))
	if rootCertExtra != nil {
		err2 := hb.AddRoots(rootCertExtra)
		if err2 != nil {
			return err2
		}
	}
	// If the certificate has a chain, use the last cert - similar with Istio
	if len(hb.Cert.Certificate) > 1 {
		last := hb.Cert.Certificate[len(hb.Cert.Certificate)-1]

		rootCAs, err := x509.ParseCertificates(last)
		if err == nil {
			for _, c := range rootCAs {
				log.Println("Adding root CA from cert chain: ", c.Subject)
				hb.TrustedCertPool.AddCert(c)
			}
		}
	}

	cert, err := x509.ParseCertificate(hb.Cert.Certificate[0])
	if err != nil {
		return err
	}
	if len(cert.URIs) > 0 {
		c0 := cert.URIs[0]
		pathComponetns := strings.Split(c0.Path, "/")
		if c0.Scheme == "spiffe" && pathComponetns[1] == "ns" && pathComponetns[3] == "sa" {
			hb.Namespace = pathComponetns[2]
			hb.SA = pathComponetns[4]
			hb.TrustDomain = cert.URIs[0].Host
		} else {
			//log.Println("Cert: ", cert)
			// TODO: extract domain, ns, name
			log.Println("Unexpected ID ", c0, cert.Issuer, cert.NotAfter)
		}
		//log.Println("Cert: ", cert)
		// TODO: extract domain, ns, name
		log.Println("ID ", c0, cert.Issuer, cert.NotAfter)
	} else {
		// org and name are set
		log.Println("Cert: ", cert.Subject.Organization, cert.NotAfter)
	}
	hb.initTLS()
	return nil
}

func (hb *Auth) AddRoots(rootCertPEM []byte) error {
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
		log.Println("Adding root CA: ", c.Subject)
		hb.TrustedCertPool.AddCert(c)
	}
	return nil
}

func (hb *Auth) initTLS() {
	hb.TLSConfig = &tls.Config{
		//MinVersion: tls.VersionTLS13,
		//PreferServerCipherSuites: ugate.preferServerCipherSuites(),
		InsecureSkipVerify: true,                  // This is not insecure here. We will verify the cert chain ourselves.
		ClientAuth:         tls.RequestClientCert, // not require - we'll fallback to JWT

		Certificates: []tls.Certificate{*hb.Cert}, // a.TlsCerts,

		VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
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
			trustDomain := peerCert.URIs[0].Host
			if trustDomain != hb.TrustDomain {
				log.Println("MTLS: invalid trust domain", trustDomain, peerCert.URIs)
				return errors.New("invalid trust domain " + trustDomain + " " + hb.TrustDomain)
			}

			// TODO: also validate namespace is same with this workload or in list of namespaces ?

			_, err := peerCert.Verify(x509.VerifyOptions{
				Roots:         hb.TrustedCertPool,
				Intermediates: intCertPool,
			})
			return err
		},
		NextProtos: []string{"istio", "h2"},
		GetCertificate: func(ch *tls.ClientHelloInfo) (*tls.Certificate, error) {
			return hb.Cert, nil
		},
	}

}

// WaitFile will check for the file to show up - the agent is running in a separate process.
func WaitFile(keyFile string, d time.Duration) error {
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

func PublicKey(key crypto.PrivateKey) crypto.PublicKey {
	//if k, ok := key.(ed25519.PrivateKey); ok {
	//	return k.Public()
	//}
	if k, ok := key.(*ecdsa.PrivateKey); ok {
		return k.Public()
	}
	if k, ok := key.(*rsa.PrivateKey); ok {
		return k.Public()
	}

	return nil
}

// SignCertDER uses caPrivate to sign a cert, returns the DER format.
// Used primarily for tests with self-signed cert.
func SignCertDER(template *x509.Certificate, pub crypto.PublicKey, caPrivate crypto.PrivateKey, parent *x509.Certificate) ([]byte, error) {
	certDER, err := x509.CreateCertificate(rand.Reader, template, parent, pub, caPrivate)
	if err != nil {
		return nil, err
	}
	return certDER, nil
}

func CertTemplate(org string, sans ...string) *x509.Certificate {
	var notBefore time.Time
	notBefore = time.Now().Add(-1 * time.Hour)

	notAfter := notBefore.Add(365 * 24 * time.Hour)

	serialNumberLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	serialNumber, _ := rand.Int(rand.Reader, serialNumberLimit)

	template := x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			CommonName:   sans[0],
			Organization: []string{org},
		},
		NotBefore: notBefore,
		NotAfter:  notAfter,

		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
		DNSNames:              sans,
		//IPAddresses:           []net.IP{auth.VIP6},
	}
	for _, k := range sans {
		if strings.Contains(k, "://") {
			u, _ := url.Parse(k)
			template.URIs = append(template.URIs, u)
		} else {
			template.DNSNames = append(template.DNSNames, k)
		}
	}
	// IPFS:
	//certKeyPub, err := x509.MarshalPKIXPublicKey(certKey.Public())
	//signature, err := sk.Sign(append([]byte(certificatePrefix), certKeyPub...))
	//value, err := asn1.Marshal(signedKey{
	//	PubKey:    keyBytes,
	//	Signature: signature,
	//})
	return &template
}

type CA struct {
	ca          *rsa.PrivateKey
	CACert      *x509.Certificate
	TrustDomain string
	prefix      string
}

func NewCA(trust string) *CA {
	ca, _ := rsa.GenerateKey(rand.Reader, 2048)
	caCert, _ := rootCert(trust, "rootCA", ca, ca)
	return &CA{ca: ca, CACert: caCert, TrustDomain: trust,
		prefix: "spiffe://" + trust + "/ns/",
	}
}

func (ca *CA) NewID(ns, sa string) *Auth {
	nodeID := &Auth{
		TrustDomain: ca.TrustDomain,
		Namespace:   ns,
		SA:          sa,
	}
	caCert := ca.CACert
	nodeID.Cert = ca.NewTLSCert(ns, sa)

	nodeID.TrustedCertPool = x509.NewCertPool()
	nodeID.TrustedCertPool.AddCert(caCert)
	nodeID.initTLS()

	return nodeID
}

func (ca *CA) NewTLSCert(ns, sa string) *tls.Certificate {
	nodeKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	csr := CertTemplate(ca.TrustDomain, ca.prefix+ns+"/sa/"+sa)
	cert, _, _ := newTLSCertAndKey(csr, nodeKey, ca.ca, ca.CACert)
	return cert
}

func newTLSCertAndKey(template *x509.Certificate, priv crypto.PrivateKey, ca crypto.PrivateKey, parent *x509.Certificate) (*tls.Certificate, []byte, []byte) {
	pub := PublicKey(priv)
	certDER, err := SignCertDER(template, pub, ca, parent)
	if err != nil {
		return nil, nil, nil
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})

	ecb, _ := x509.MarshalPKCS8PrivateKey(priv)
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: ecb})

	tlsCert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, nil, nil
	}
	return &tlsCert, keyPEM, certPEM
}

func rootCert(org, cn string, priv crypto.PrivateKey, ca crypto.PrivateKey) (*x509.Certificate, []byte) {
	pub := PublicKey(priv)
	var notBefore time.Time
	notBefore = time.Now().Add(-1 * time.Hour)

	notAfter := notBefore.Add(365 * 24 * time.Hour)

	serialNumberLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	serialNumber, _ := rand.Int(rand.Reader, serialNumberLimit)

	template := x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			CommonName:   cn,
			Organization: []string{org},
		},
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}
	certDER, err := x509.CreateCertificate(rand.Reader, &template, &template, pub, ca)
	if err != nil {
		panic(err)
	}
	rootCA, _ := x509.ParseCertificates(certDER)
	return rootCA[0], certDER
}
