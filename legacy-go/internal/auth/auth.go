// Package auth implements Adobe IMS OAuth 2.0 (PKCE, public "Single Page App"
// client) with an https://localhost redirect, and stores tokens in the macOS Keychain.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/zalando/go-keyring"

	"fiotransfer/internal/config"
)

// imsBase is Adobe IMS (overridable for tests).
var imsBase = "https://ims-na1.adobelogin.com"

const (
	scopes        = "openid email profile offline_access additional_info.roles" // as in Frame.io's SDK
	keyringSvc    = "FioTransfer"
	keyringUser   = "adobe-ims"
	refreshMargin = 5 * time.Minute
)

type Token struct {
	Access  string    `json:"access_token"`
	Refresh string    `json:"refresh_token"`
	Expiry  time.Time `json:"expiry"`
}

var (
	ErrNotLoggedIn   = errors.New("not signed in to Frame.io")
	ErrLoginCanceled = errors.New("sign-in canceled")
)

// Manager hands out valid access tokens, refreshing as needed.
type Manager struct {
	mu       sync.Mutex
	tok      *Token
	clientID func() string
	http     *http.Client
	static   bool
}

func NewManager(clientID func() string) *Manager {
	m := &Manager{clientID: clientID, http: &http.Client{Timeout: 30 * time.Second}}
	m.tok = store.load()
	return m
}

// NewStatic returns a Manager with a fixed token that never refreshes (tests, debugging).
func NewStatic(token string) *Manager {
	return &Manager{clientID: func() string { return "" }, http: http.DefaultClient,
		tok: &Token{Access: token, Refresh: "static", Expiry: time.Now().Add(100 * 365 * 24 * time.Hour)}, static: true}
}

func (m *Manager) LoggedIn() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.tok != nil && m.tok.Refresh != ""
}

// Token returns a valid access token, refreshing if it is about to expire.
func (m *Manager) Token(ctx context.Context) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.tok == nil {
		return "", ErrNotLoggedIn
	}
	if time.Until(m.tok.Expiry) > refreshMargin {
		return m.tok.Access, nil
	}
	if err := m.refreshLocked(ctx); err != nil {
		return "", err
	}
	return m.tok.Access, nil
}

// ForceRefresh is used after a 401 from the API.
func (m *Manager) ForceRefresh(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.tok == nil {
		return ErrNotLoggedIn
	}
	return m.refreshLocked(ctx)
}

func (m *Manager) refreshLocked(ctx context.Context) error {
	if m.static {
		return nil
	}
	if m.tok.Refresh == "" {
		return ErrNotLoggedIn
	}
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"client_id":     {m.clientID()},
		"refresh_token": {m.tok.Refresh},
	}
	t, err := m.tokenRequest(ctx, form)
	if err != nil {
		var re *rejectedError
		if errors.As(err, &re) {
			// The session is gone (revoked or expired): drop it so the app asks to sign in.
			m.tok = nil
			store.clear()
			return ErrNotLoggedIn
		}
		return fmt.Errorf("refreshing Adobe session: %w", err)
	}
	if t.Refresh == "" {
		t.Refresh = m.tok.Refresh
	}
	m.tok = t
	store.save(t)
	return nil
}

func (m *Manager) Logout() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tok = nil
	store.clear()
}

// RedirectURI is what must be registered on the Adobe "OAuth Single Page App" credential.
func RedirectURI(port int) string { return fmt.Sprintf("https://localhost:%d/callback", port) }

// Login runs the browser-based PKCE flow. openURL opens the system browser.
func (m *Manager) Login(ctx context.Context, port int, openURL func(string)) error {
	clientID := m.clientID()
	if clientID == "" {
		return config.ErrNoClientID
	}
	verifier := randString(64)
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])
	state := randString(24)
	redirect := RedirectURI(port)

	cert, certFile, err := localCert()
	if err != nil {
		return fmt.Errorf("creating local sign-in certificate: %w", err)
	}
	trustLocalCert(certFile)
	// localhost may resolve to IPv4 or IPv6; listen on both.
	var lns []net.Listener
	for _, addr := range []string{"127.0.0.1", "[::1]"} {
		if l, err := net.Listen("tcp", fmt.Sprintf("%s:%d", addr, port)); err == nil {
			lns = append(lns, l)
		}
	}
	if len(lns) == 0 {
		return fmt.Errorf("port %d is in use", port)
	}
	type result struct {
		code string
		err  error
	}
	resCh := make(chan result, 1)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/callback" {
			http.NotFound(w, r)
			return
		}
		q := r.URL.Query()
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		var res result
		switch {
		case q.Get("error") != "":
			res.err = fmt.Errorf("Adobe sign-in failed: %s %s", q.Get("error"), q.Get("error_description"))
		case q.Get("state") != state:
			res.err = errors.New("sign-in state mismatch; please try again")
		case q.Get("code") == "":
			res.err = errors.New("sign-in returned no authorization code")
		default:
			res.code = q.Get("code")
		}
		if res.err != nil {
			fmt.Fprintf(w, page, "Sign-in failed", htmlEscape(res.err.Error()))
		} else {
			fmt.Fprintf(w, page, "Signed in", "You can close this tab and return to FioTransfer.")
		}
		select {
		case resCh <- res:
		default:
		}
	})}
	srv.TLSConfig = &tls.Config{Certificates: []tls.Certificate{cert}}
	for _, l := range lns {
		go srv.ServeTLS(l, "", "")
	}
	defer srv.Close()

	q := url.Values{
		"client_id":             {clientID},
		"scope":                 {scopes},
		"response_type":         {"code"},
		"redirect_uri":          {redirect},
		"state":                 {state},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
	}
	openURL(imsBase + "/ims/authorize/v2?" + q.Encode())

	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	var res result
	select {
	case <-ctx.Done():
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return errors.New("timed out waiting for Adobe sign-in")
		}
		return ErrLoginCanceled
	case res = <-resCh:
	}
	if res.err != nil {
		return res.err
	}
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"client_id":     {clientID},
		"code":          {res.code},
		"code_verifier": {verifier},
		"redirect_uri":  {redirect},
	}
	t, err := m.tokenRequest(ctx, form)
	if err != nil {
		return err
	}
	m.mu.Lock()
	m.tok = t
	m.mu.Unlock()
	store.save(t)
	return nil
}

func (m *Manager) tokenRequest(ctx context.Context, form url.Values) (*Token, error) {
	req, _ := http.NewRequestWithContext(ctx, "POST", imsBase+"/ims/token/v3", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := m.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != 200 {
		if resp.StatusCode == 400 || resp.StatusCode == 401 {
			return nil, &rejectedError{fmt.Sprintf("Adobe returned %d: %s", resp.StatusCode, truncate(string(body), 300))}
		}
		return nil, fmt.Errorf("Adobe token endpoint returned %d: %s", resp.StatusCode, truncate(string(body), 300))
	}
	var r struct {
		AccessToken  string  `json:"access_token"`
		RefreshToken string  `json:"refresh_token"`
		ExpiresIn    float64 `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, err
	}
	exp := time.Duration(r.ExpiresIn) * time.Second
	if r.ExpiresIn > 1e6 { // some IMS versions report milliseconds
		exp = time.Duration(r.ExpiresIn) * time.Millisecond
	}
	if exp <= 0 {
		exp = time.Hour
	}
	return &Token{Access: r.AccessToken, Refresh: r.RefreshToken, Expiry: time.Now().Add(exp)}, nil
}

// Tokens live in the Keychain; a 0600 file is the fallback if the Keychain is unavailable.
func fallbackPath() string { return filepath.Join(config.Dir(), "token.json") }

// rejectedError means Adobe refused the grant (bad code, revoked or expired refresh token).
type rejectedError struct{ msg string }

func (e *rejectedError) Error() string { return e.msg }

// tokenStore persists tokens; tests swap in a memory store so they never touch the Keychain.
type tokenStore interface {
	load() *Token
	save(*Token)
	clear()
}

type keychainTokens struct{}

func (keychainTokens) load() *Token  { return loadToken() }
func (keychainTokens) save(t *Token) { saveToken(t) }
func (keychainTokens) clear() {
	_ = keyring.Delete(keyringSvc, keyringUser)
	_ = os.Remove(fallbackPath())
}

var store tokenStore = keychainTokens{}

func saveToken(t *Token) {
	b, _ := json.Marshal(t)
	if err := keyring.Set(keyringSvc, keyringUser, string(b)); err == nil {
		_ = os.Remove(fallbackPath())
		return
	}
	_ = os.WriteFile(fallbackPath(), b, 0o600)
}

func loadToken() *Token {
	var raw []byte
	if s, err := keyring.Get(keyringSvc, keyringUser); err == nil {
		raw = []byte(s)
	} else if b, err := os.ReadFile(fallbackPath()); err == nil {
		raw = b
	}
	if raw == nil {
		return nil
	}
	var t Token
	if json.Unmarshal(raw, &t) != nil || t.Refresh == "" {
		return nil
	}
	return &t
}

func randString(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)[:n]
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

func htmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
	return r.Replace(s)
}

const page = `<!doctype html><meta charset="utf-8"><title>FioTransfer</title>
<body style="font:16px -apple-system,system-ui;background:#111;color:#eee;display:grid;place-items:center;height:100vh;margin:0">
<div style="text-align:center"><h2>%s</h2><p>%s</p></div></body>`
