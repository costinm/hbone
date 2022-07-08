package auth

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/url"
	"strings"
	"time"
)

const (
	blockTypeECPrivateKey    = "EC PRIVATE KEY"
	blockTypeRSAPrivateKey   = "RSA PRIVATE KEY" // PKCS#1 private key
	blockTypePKCS8PrivateKey = "PRIVATE KEY"     // PKCS#8 plain private key
)

// CA is used as an internal CA, mainly for testing.
// Roughly equivalent with a simplified Istio Citadel.
type CA struct {
	Private     *rsa.PrivateKey
	CACert      *x509.Certificate
	TrustDomain string
	prefix      string
}

func NewCA(trust string) *CA {
	ca, _ := rsa.GenerateKey(rand.Reader, 2048)
	caCert, _ := rootCert(trust, "rootCA", ca, ca)
	return &CA{Private: ca, CACert: caCert, TrustDomain: trust,
		prefix: "spiffe://" + trust + "/ns/",
	}
}

func (ca *CA) NewID(ns, sa string) *MeshAuth {
	caCert := ca.CACert
	crt := ca.NewTLSCert(ns, sa)

	nodeID := NewMeshAuth()
	nodeID.TrustedCertPool.AddCert(caCert)
	nodeID.SetTLSCertificate(crt)

	return nodeID
}

func (ca *CA) NewTLSCert(ns, sa string) *tls.Certificate {
	nodeKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	csr := CertTemplate(ca.TrustDomain, ca.prefix+ns+"/sa/"+sa)
	cert, _, _ := newTLSCertAndKey(csr, nodeKey, ca.Private, ca.CACert)
	return cert
}

func (a *MeshAuth) NewCSR(san string) (privPEM []byte, csrPEM []byte, err error) {
	var priv crypto.PrivateKey

	rsaKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	priv = rsaKey

	csr := GenCSRTemplate(a.TrustDomain, san)
	csrBytes, err := x509.CreateCertificateRequest(rand.Reader, csr, priv)

	encodeMsg := "CERTIFICATE REQUEST"

	csrPEM = pem.EncodeToMemory(&pem.Block{Type: encodeMsg, Bytes: csrBytes})

	var encodedKey []byte
	//if pkcs8 {
	//	if encodedKey, err = x509.MarshalPKCS8PrivateKey(priv); err != nil {
	//		return nil, nil, err
	//	}
	//	privPem = pem.EncodeToMemory(&pem.Block{Type: blockTypePKCS8PrivateKey, Bytes: encodedKey})
	//} else {
	switch k := priv.(type) {
	case *rsa.PrivateKey:
		encodedKey = x509.MarshalPKCS1PrivateKey(k)
		privPEM = pem.EncodeToMemory(&pem.Block{Type: blockTypeRSAPrivateKey, Bytes: encodedKey})
	case *ecdsa.PrivateKey:
		encodedKey, err = x509.MarshalECPrivateKey(k)
		if err != nil {
			return nil, nil, err
		}
		privPEM = pem.EncodeToMemory(&pem.Block{Type: blockTypeECPrivateKey, Bytes: encodedKey})
	}
	//}

	return
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

func PublicKey(key crypto.PrivateKey) crypto.PublicKey {
	if k, ok := key.(ed25519.PrivateKey); ok {
		return k.Public()
	}
	if k, ok := key.(*ecdsa.PrivateKey); ok {
		return k.Public()
	}
	if k, ok := key.(*rsa.PrivateKey); ok {
		return k.Public()
	}

	return nil
}

func GenCSRTemplate(trustDomain, san string) *x509.CertificateRequest {
	template := &x509.CertificateRequest{
		Subject: pkix.Name{
			Organization: []string{trustDomain},
		},
	}

	// TODO: add the SAN, it is not required, server will fill up

	return template
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
