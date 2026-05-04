package cli

import (
	cryptorand "crypto/rand"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

// Token holds OAuth2 credentials cached for a server URL.
// The server validates the Google ID token (IDToken) directly.
type Token struct {
	IDToken      string    `json:"id_token"`      // Google OIDC ID token — what the server validates
	AccessToken  string    `json:"access_token"`  // kept for reference / future use
	RefreshToken string    `json:"refresh_token"`
	Expiry       time.Time `json:"expiry"`
	ClientID     string    `json:"client_id"`
	ClientSecret string    `json:"client_secret"`
}

type credStore map[string]*Token

func credentialsFilePath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("get config dir: %w", err)
	}
	return filepath.Join(dir, "ds", "credentials.json"), nil
}

func readCredStore() (credStore, error) {
	path, err := credentialsFilePath()
	if err != nil {
		return credStore{}, err
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return credStore{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read credentials: %w", err)
	}
	var cs credStore
	if err := json.Unmarshal(data, &cs); err != nil {
		return nil, fmt.Errorf("parse credentials: %w", err)
	}
	return cs, nil
}

func writeCredStore(cs credStore) error {
	path, err := credentialsFilePath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return fmt.Errorf("create credentials directory: %w", err)
	}
	data, err := json.MarshalIndent(cs, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0600)
}

// loadCreds returns cached credentials for serverURL, or nil if none.
func loadCreds(serverURL string) (*Token, error) {
	cs, err := readCredStore()
	if err != nil {
		return nil, err
	}
	return cs[serverURL], nil
}

// saveCreds stores credentials for serverURL.
func saveCreds(serverURL string, tok *Token) error {
	cs, err := readCredStore()
	if err != nil {
		cs = credStore{}
	}
	cs[serverURL] = tok
	return writeCredStore(cs)
}

// deleteCreds removes credentials for serverURL.
func deleteCreds(serverURL string) error {
	cs, err := readCredStore()
	if err != nil {
		cs = credStore{}
	}
	delete(cs, serverURL)
	return writeCredStore(cs)
}

// authTransport is an http.RoundTripper that injects Authorization: Bearer
// headers for servers that have cached credentials. It refreshes expired tokens
// automatically using the stored refresh token.
type authTransport struct {
	base http.RoundTripper
	mu   sync.Mutex
}

// NewAuthTransport returns an http.RoundTripper that injects Bearer tokens
// from the credential store when available. All existing CLI commands get auth
// headers automatically when credentials are cached — no per-command changes needed.
func NewAuthTransport() http.RoundTripper {
	return &authTransport{base: http.DefaultTransport}
}

func (t *authTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	serverURL := req.URL.Scheme + "://" + req.URL.Host

	tok, err := loadCreds(serverURL)
	if err != nil || tok == nil {
		// No credentials — pass through without auth header.
		return t.base.RoundTrip(req)
	}

	// Refresh token if expired or expiring within 30 seconds.
	if !tok.Expiry.IsZero() && time.Now().After(tok.Expiry.Add(-30*time.Second)) && tok.RefreshToken != "" {
		t.mu.Lock()
		// Re-read under lock to avoid redundant refreshes from concurrent requests.
		if tok2, loadErr := loadCreds(serverURL); loadErr == nil && tok2 != nil {
			tok = tok2
		}
		if !tok.Expiry.IsZero() && time.Now().After(tok.Expiry.Add(-30*time.Second)) {
			if refreshed, refreshErr := refreshOAuthToken(tok); refreshErr == nil {
				tok = refreshed
				_ = saveCreds(serverURL, tok) // best effort; failure is non-fatal
			}
		}
		t.mu.Unlock()
	}

	// The server validates the Google OIDC ID token directly.
	bearer := tok.IDToken
	if bearer == "" {
		bearer = tok.AccessToken // fallback when no ID token available
	}
	reqCopy := req.Clone(req.Context())
	reqCopy.Header.Set("Authorization", "Bearer "+bearer)
	return t.base.RoundTrip(reqCopy)
}

// refreshOAuthToken uses the stored refresh token to obtain a new access token.
func refreshOAuthToken(tok *Token) (*Token, error) {
	conf := &oauth2.Config{
		ClientID:     tok.ClientID,
		ClientSecret: tok.ClientSecret,
		Endpoint:     google.Endpoint,
	}
	src := conf.TokenSource(context.Background(), &oauth2.Token{
		AccessToken:  tok.AccessToken,
		RefreshToken: tok.RefreshToken,
		Expiry:       tok.Expiry,
	})
	newTok, err := src.Token()
	if err != nil {
		return nil, err
	}
	idToken, _ := newTok.Extra("id_token").(string)
	return &Token{
		IDToken:      idToken,
		AccessToken:  newTok.AccessToken,
		RefreshToken: newTok.RefreshToken,
		Expiry:       newTok.Expiry,
		ClientID:     tok.ClientID,
		ClientSecret: tok.ClientSecret,
	}, nil
}

// dsAuthConfig is the auth section of the /.well-known/ds-config response.
type dsAuthConfig struct {
	Type         string `json:"type"`
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
}

// dsServerConfig is the full /.well-known/ds-config response.
type dsServerConfig struct {
	Auth dsAuthConfig `json:"auth"`
}

// Login authenticates with the given server and stores the credentials.
// It first tries to discover OAuth client credentials via /.well-known/ds-config.
// If that fails, it falls back to the compiled-in fallbackClientID and
// fallbackClientSecret.
func (a *App) Login(serverURL, fallbackClientID, fallbackClientSecret string) error {
	serverURL = strings.TrimRight(serverURL, "/")

	clientID, clientSecret := fallbackClientID, fallbackClientSecret

	// Attempt discovery via well-known endpoint; failure is non-fatal if we
	// have compiled-in fallback credentials.
	resp, err := a.HTTP.Get(serverURL + "/.well-known/ds-config")
	if err == nil && resp.StatusCode == http.StatusOK {
		defer resp.Body.Close()
		var cfg dsServerConfig
		if jsonErr := json.NewDecoder(resp.Body).Decode(&cfg); jsonErr == nil {
			if cfg.Auth.Type == "none" || cfg.Auth.Type == "" {
				fmt.Fprintln(a.Out, "Server does not require authentication")
				return nil
			}
			// "iap" is kept for backward compatibility with older server configs.
			if (cfg.Auth.Type == "oauth" || cfg.Auth.Type == "iap") && cfg.Auth.ClientID != "" {
				clientID = cfg.Auth.ClientID
				if cfg.Auth.ClientSecret != "" {
					clientSecret = cfg.Auth.ClientSecret
				}
			}
		}
	} else if resp != nil {
		resp.Body.Close()
	}

	if clientID == "" {
		return fmt.Errorf("could not determine OAuth client ID: server did not provide one via /.well-known/ds-config and no default is compiled in")
	}

	oauthTok, err := runOAuthDesktopFlow(clientID, clientSecret)
	if err != nil {
		return fmt.Errorf("oauth flow: %w", err)
	}

	// Extract the Google OIDC ID token — sent as the Bearer token to the server.
	email := ""
	idToken, _ := oauthTok.Extra("id_token").(string)
	if idToken != "" {
		email, _ = jwtEmail(idToken)
	}

	tok := &Token{
		IDToken:      idToken,
		AccessToken:  oauthTok.AccessToken,
		RefreshToken: oauthTok.RefreshToken,
		Expiry:       oauthTok.Expiry,
		ClientID:     clientID,
		ClientSecret: clientSecret,
	}
	if err := saveCreds(serverURL, tok); err != nil {
		return fmt.Errorf("save credentials: %w", err)
	}

	if email != "" {
		fmt.Fprintf(a.Out, "Logged in as %s\n", email)
	} else {
		fmt.Fprintln(a.Out, "Logged in successfully")
	}
	return nil
}

// Logout removes stored credentials for the given server.
func (a *App) Logout(serverURL string) error {
	serverURL = strings.TrimRight(serverURL, "/")
	if err := deleteCreds(serverURL); err != nil {
		return fmt.Errorf("delete credentials: %w", err)
	}
	fmt.Fprintf(a.Out, "Logged out from %s\n", serverURL)
	return nil
}

// runOAuthDesktopFlow performs a browser-based OAuth 2.0 authorization code
// flow and returns the resulting token.
func runOAuthDesktopFlow(clientID, clientSecret string) (*oauth2.Token, error) {
	ln, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		return nil, fmt.Errorf("listen for OAuth callback: %w", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port

	conf := &oauth2.Config{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		RedirectURL:  fmt.Sprintf("http://localhost:%d/callback", port),
		Scopes:       []string{"openid", "email"},
		Endpoint:     google.Endpoint,
	}

	stateBytes := make([]byte, 16)
	if _, err := cryptorand.Read(stateBytes); err != nil {
		return nil, fmt.Errorf("generate state: %w", err)
	}
	state := hex.EncodeToString(stateBytes)

	codeCh := make(chan string, 1)
	errCh := make(chan error, 1)

	mux := http.NewServeMux()
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("state"); got != state {
			errCh <- fmt.Errorf("state mismatch in OAuth callback")
			http.Error(w, "state mismatch", http.StatusBadRequest)
			return
		}
		code := r.URL.Query().Get("code")
		if code == "" {
			errCh <- fmt.Errorf("no authorization code in OAuth callback")
			http.Error(w, "missing code", http.StatusBadRequest)
			return
		}
		fmt.Fprintln(w, "Authentication successful! You can close this tab.")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		codeCh <- code
	})

	// Start the callback server before opening the browser so it is ready
	// to accept the redirect the moment the user completes sign-in.
	cbSrv := &http.Server{Handler: mux}
	go cbSrv.Serve(ln) //nolint:errcheck

	authURL := conf.AuthCodeURL(state, oauth2.AccessTypeOffline)
	fmt.Printf("Opening browser for authentication...\n")
	fmt.Printf("If the browser does not open, visit:\n  %s\n", authURL)
	openBrowser(authURL)

	shutdownSrv := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		cbSrv.Shutdown(ctx) //nolint:errcheck
	}

	var code string
	select {
	case code = <-codeCh:
	case err = <-errCh:
		shutdownSrv()
		return nil, err
	case <-time.After(5 * time.Minute):
		shutdownSrv()
		return nil, fmt.Errorf("timed out waiting for browser authentication")
	}
	shutdownSrv()

	tok, err := conf.Exchange(context.Background(), code)
	if err != nil {
		return nil, fmt.Errorf("exchange authorization code: %w", err)
	}
	return tok, nil
}

// openBrowser opens url in the user's default browser.
func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "linux":
		cmd = exec.Command("xdg-open", url)
	case "windows":
		cmd = exec.Command("cmd", "/c", "start", url)
	default:
		return
	}
	_ = cmd.Start()
}

// jwtEmail decodes a JWT payload (without verifying the signature) and
// returns the "email" claim.
func jwtEmail(token string) (string, error) {
	parts := strings.SplitN(token, ".", 3)
	if len(parts) != 3 {
		return "", fmt.Errorf("invalid JWT format")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", fmt.Errorf("decode JWT payload: %w", err)
	}
	var claims struct {
		Email string `json:"email"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return "", fmt.Errorf("parse JWT claims: %w", err)
	}
	if claims.Email == "" {
		return "", fmt.Errorf("no email claim in token")
	}
	return claims.Email, nil
}
