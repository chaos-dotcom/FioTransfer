package auth

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"testing"
	"time"
)

type memTokens struct{ t *Token }

func (m *memTokens) load() *Token  { return m.t }
func (m *memTokens) save(t *Token) { m.t = t }
func (m *memTokens) clear()        { m.t = nil }

func TestMain(m *testing.M) {
	store = &memTokens{} // never touch the real Keychain from tests
	trustLocalCert = func(string) {}
	home, _ := os.MkdirTemp("", "fio-auth-test") // nor the real certificates in Application Support
	os.Setenv("HOME", home)
	code := m.Run()
	os.RemoveAll(home)
	os.Exit(code)
}

func TestRejectedRefreshSignsOut(t *testing.T) {
	ims := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"access_denied"}`, 400)
	}))
	defer ims.Close()
	imsBase = ims.URL
	m := &Manager{clientID: func() string { return "cid" }, http: http.DefaultClient,
		tok: &Token{Access: "old", Refresh: "dead", Expiry: time.Now().Add(-time.Minute)}}
	if _, err := m.Token(t.Context()); err != ErrNotLoggedIn {
		t.Fatalf("err = %v, want ErrNotLoggedIn", err)
	}
	if m.LoggedIn() {
		t.Error("still logged in after rejected refresh")
	}
}

func TestLoginHTTPSCallback(t *testing.T) {
	ims := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		if r.URL.Path != "/ims/token/v3" || r.Form.Get("code") != "the-code" || r.Form.Get("code_verifier") == "" ||
			r.Form.Get("redirect_uri") != "https://localhost:18765/callback" {
			http.Error(w, "bad token request: "+r.Form.Encode(), 400)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"access_token": "acc", "refresh_token": "ref", "expires_in": 86399})
	}))
	defer ims.Close()
	imsBase = ims.URL

	m := &Manager{clientID: func() string { return "cid" }, http: http.DefaultClient, static: true}
	browser := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}
	err := m.Login(t.Context(), 18765, func(authURL string) {
		u, _ := url.Parse(authURL)
		q := u.Query()
		if q.Get("code_challenge_method") != "S256" || q.Get("client_id") != "cid" {
			t.Errorf("authorize URL: %s", authURL)
		}
		go func() {
			time.Sleep(100 * time.Millisecond)
			resp, err := browser.Get(q.Get("redirect_uri") + "?code=the-code&state=" + url.QueryEscape(q.Get("state")))
			if err != nil {
				t.Errorf("callback: %v", err)
				return
			}
			resp.Body.Close()
		}()
	})
	if err != nil {
		t.Fatal(err)
	}
	if m.tok.Access != "acc" || m.tok.Refresh != "ref" {
		t.Fatalf("token = %+v", m.tok)
	}
}

// The local CA must only be able to vouch for localhost.
func TestLocalCAIsNameConstrained(t *testing.T) {
	// An unconstrained CA from an earlier build gets replaced.
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: serial(), Subject: pkix.Name{CommonName: "old"}, IsCA: true, BasicConstraintsValid: true,
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().AddDate(5, 0, 0), KeyUsage: x509.KeyUsageCertSign}
	der, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	kb, _ := x509.MarshalECPrivateKey(key)
	writePEM(pemFile("local-ca.pem"), "CERTIFICATE", der, 0o644)
	writePEM(pemFile("local-ca-key.pem"), "EC PRIVATE KEY", kb, 0o600)

	cert, caPath, err := localCert()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(pemFile(oldCAFile)); err != nil {
		t.Error("replaced CA was not kept for removal from the keychain")
	}
	b, _ := os.ReadFile(caPath)
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(b)
	leaf, _ := x509.ParseCertificate(cert.Certificate[0])
	for _, host := range []string{"localhost", "127.0.0.1", "::1"} {
		if _, err := leaf.Verify(x509.VerifyOptions{Roots: pool, DNSName: host}); err != nil {
			t.Errorf("localhost cert rejected for %s: %v", host, err)
		}
	}

	// A certificate for another site signed with the CA key is not trusted.
	ca, caKey, err := localCA()
	if err != nil {
		t.Fatal(err)
	}
	evil := &x509.Certificate{SerialNumber: serial(), Subject: pkix.Name{CommonName: "example.com"}, DNSNames: []string{"example.com"},
		IPAddresses: []net.IP{net.ParseIP("93.184.216.34")}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	ek, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	ed, _ := x509.CreateCertificate(rand.Reader, evil, ca, &ek.PublicKey, caKey)
	ec, _ := x509.ParseCertificate(ed)
	if _, err := ec.Verify(x509.VerifyOptions{Roots: pool, DNSName: "example.com"}); err == nil {
		t.Error("CA signed a trusted certificate for example.com")
	}
}
