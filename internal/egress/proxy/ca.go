package proxy

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"sync"
	"time"

	"crypto/tls"

	"github.com/cockroachdb/errors"
)

// CA is a self-signed certificate authority used to sign on-the-fly leaf
// certificates for hosts the proxy intercepts. The CA's private key never
// leaves the orchestrator process; only the public certificate (PEM bytes)
// is published to the VM.
type CA struct {
	cert    *x509.Certificate
	key     *ecdsa.PrivateKey
	pemCert []byte

	mu    sync.Mutex
	cache map[string]*tls.Certificate
}

// GenerateCA creates a fresh ephemeral CA. The returned PEM bytes are safe
// to write into the cloud-init seed; the private key stays on the CA.
func GenerateCA() (*CA, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, errors.Wrap(err, "generate CA key")
	}

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, errors.Wrap(err, "generate CA serial")
	}

	template := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "kvarn ephemeral CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true,
	}

	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return nil, errors.Wrap(err, "self-sign CA cert")
	}

	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, errors.Wrap(err, "parse CA cert")
	}

	pemCert := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})

	return &CA{
		cert:    cert,
		key:     key,
		pemCert: pemCert,
		cache:   make(map[string]*tls.Certificate),
	}, nil
}

// CertPEM returns the CA certificate in PEM form.
func (c *CA) CertPEM() []byte { return c.pemCert }

// LeafCert returns a TLS certificate valid for the given host, generating
// (and caching) one if necessary. The certificate is signed by this CA.
func (c *CA) LeafCert(host string) (*tls.Certificate, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if cert, ok := c.cache[host]; ok {
		return cert, nil
	}

	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, errors.Wrap(err, "generate leaf key")
	}

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, errors.Wrap(err, "generate leaf serial")
	}

	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: host},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{host},
	}

	der, err := x509.CreateCertificate(rand.Reader, template, c.cert, &leafKey.PublicKey, c.key)
	if err != nil {
		return nil, errors.Wrap(err, "sign leaf cert")
	}

	cert := &tls.Certificate{
		Certificate: [][]byte{der, c.cert.Raw},
		PrivateKey:  leafKey,
	}
	c.cache[host] = cert
	return cert, nil
}
