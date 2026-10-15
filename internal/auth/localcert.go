package auth

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha1"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"fiotransfer/internal/config"
)

// Adobe requires an https redirect for public (PKCE) clients, so the sign-in
// callback is served on https://localhost. Like mkcert, FioTransfer creates a
// small local certificate authority that macOS trusts once; browsers (including
// Firefox, which imports OS root CAs but not individually trusted leaf certs)
// then accept the localhost certificate it signs.

func certDir() string { return config.Dir() }

func pemFile(name string) string { return filepath.Join(certDir(), name) }

func writePEM(path, typ string, der []byte, mode os.FileMode) error {
	return os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der}), mode)
}

func serial() *big.Int {
	n, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 62))
	return n
}

// localCA loads or creates the local certificate authority.
func localCA() (*x509.Certificate, *ecdsa.PrivateKey, error) {
	certPath, keyPath := pemFile("local-ca.pem"), pemFile("local-ca-key.pem")
	if c, err := tls.LoadX509KeyPair(certPath, keyPath); err == nil {
		if cert, err := x509.ParseCertificate(c.Certificate[0]); err == nil && time.Until(cert.NotAfter) > 60*24*time.Hour {
			if key, ok := c.PrivateKey.(*ecdsa.PrivateKey); ok {
				return cert, key, nil
			}
		}
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	host, _ := os.Hostname()
	tmpl := &x509.Certificate{
		SerialNumber:          serial(),
		Subject:               pkix.Name{CommonName: "FioTransfer Local CA (" + host + ")", Organization: []string{"FioTransfer"}},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().AddDate(10, 0, 0),
		IsCA:                  true,
		BasicConstraintsValid: true,
		MaxPathLenZero:        true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, err
	}
	kb, _ := x509.MarshalECPrivateKey(key)
	if err := writePEM(keyPath, "EC PRIVATE KEY", kb, 0o600); err != nil {
		return nil, nil, err
	}
	if err := writePEM(certPath, "CERTIFICATE", der, 0o644); err != nil {
		return nil, nil, err
	}
	cert, err := x509.ParseCertificate(der)
	return cert, key, err
}

// localCert returns the localhost certificate (signed by the local CA) and the CA's PEM path.
func localCert() (tls.Certificate, string, error) {
	ca, caKey, err := localCA()
	if err != nil {
		return tls.Certificate{}, "", err
	}
	caPath := pemFile("local-ca.pem")
	certPath, keyPath := pemFile("localhost.pem"), pemFile("localhost-key.pem")
	if c, err := tls.LoadX509KeyPair(certPath, keyPath); err == nil {
		if leaf, err := x509.ParseCertificate(c.Certificate[0]); err == nil &&
			time.Until(leaf.NotAfter) > 30*24*time.Hour && leaf.CheckSignatureFrom(ca) == nil {
			return c, caPath, nil
		}
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, "", err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial(),
		Subject:      pkix.Name{CommonName: "localhost", Organization: []string{"FioTransfer"}},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().AddDate(0, 0, 397), // within Apple's 398-day limit for TLS certs
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &key.PublicKey, caKey)
	if err != nil {
		return tls.Certificate{}, "", err
	}
	kb, _ := x509.MarshalECPrivateKey(key)
	if err := writePEM(keyPath, "EC PRIVATE KEY", kb, 0o600); err != nil {
		return tls.Certificate{}, "", err
	}
	if err := writePEM(certPath, "CERTIFICATE", der, 0o644); err != nil {
		return tls.Certificate{}, "", err
	}
	c, err := tls.LoadX509KeyPair(certPath, keyPath)
	return c, caPath, err
}

var trustLocalCert = trustCert

// trustCert makes macOS trust the local CA (one password prompt, first time only).
func trustCert(caPath string) {
	removeLegacyCert()
	if exec.Command("/usr/bin/security", "verify-cert", "-c", pemFile("localhost.pem"), "-p", "ssl", "-s", "localhost", "-q").Run() == nil &&
		caTrusted(caPath) {
		return
	}
	home, _ := os.UserHomeDir()
	kc := filepath.Join(home, "Library", "Keychains", "login.keychain-db")
	_ = exec.Command("/usr/bin/security", "add-trusted-cert", "-r", "trustRoot", "-k", kc, caPath).Run()
}

// caTrusted reports whether the CA itself (not just the leaf) is in the user trust settings.
func caTrusted(caPath string) bool {
	return exec.Command("/usr/bin/security", "verify-cert", "-c", caPath, "-p", "basic", "-q").Run() == nil
}

// removeLegacyCert deletes the self-signed localhost cert used by earlier builds.
func removeLegacyCert() {
	old := pemFile("localhost-cert.pem")
	b, err := os.ReadFile(old)
	if err != nil {
		return
	}
	if blk, _ := pem.Decode(b); blk != nil {
		sum := sha1.Sum(blk.Bytes)
		_ = exec.Command("/usr/bin/security", "delete-certificate", "-Z", hex.EncodeToString(sum[:])).Run()
	}
	_ = os.Remove(old)
}
