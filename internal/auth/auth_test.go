package auth

import (
	"crypto/tls"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
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
	m.Run()
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
