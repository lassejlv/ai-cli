package auth

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Codex (ChatGPT subscription) OAuth constants, matching the official Codex CLI.
const (
	CodexIssuer      = "https://auth.openai.com"
	CodexClientID    = "app_EMoamEEZ73f0CkXaXp7hrann"
	CodexOriginator  = "codex_cli_rs"
	codexRedirectURI = "http://localhost:1455/auth/callback"
	codexCallbackAdr = "127.0.0.1:1455"
	codexScope       = "openid profile email offline_access"
	codexDeviceURL   = CodexIssuer + "/codex/device"
)

func codexIssuer() string {
	if v := os.Getenv("AI_CLI_CODEX_ISSUER"); v != "" {
		return strings.TrimRight(v, "/")
	}
	return CodexIssuer
}

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token"`
}

// CodexLoginOptions configure the browser flow.
type CodexLoginOptions struct {
	OpenBrowser func(string)
	Log         func(string)
}

// CodexBrowserLogin runs the OAuth PKCE flow with a local callback server on
// port 1455, the redirect URI registered for the Codex client.
func CodexBrowserLogin(ctx context.Context, opts CodexLoginOptions) (Credential, error) {
	verifier := randomString(64)
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])
	state := randomString(32)

	q := url.Values{}
	q.Set("response_type", "code")
	q.Set("client_id", CodexClientID)
	q.Set("redirect_uri", codexRedirectURI)
	q.Set("scope", codexScope)
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "S256")
	q.Set("id_token_add_organizations", "true")
	q.Set("codex_cli_simplified_flow", "true")
	q.Set("state", state)
	q.Set("originator", CodexOriginator)
	authURL := codexIssuer() + "/oauth/authorize?" + q.Encode()

	ln, err := net.Listen("tcp", codexCallbackAdr)
	if err != nil {
		return Credential{}, fmt.Errorf("could not listen on %s for the OAuth callback (is Codex CLI login running?): %w; try `ai login codex --device`", codexCallbackAdr, err)
	}
	type result struct {
		cred Credential
		err  error
	}
	done := make(chan result, 1)
	mux := http.NewServeMux()
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	mux.HandleFunc("/auth/callback", func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		if e := query.Get("error"); e != "" {
			msg := e
			if d := query.Get("error_description"); d != "" {
				msg += ": " + d
			}
			writeCallbackPage(w, false, msg)
			done <- result{err: fmt.Errorf("sign-in failed: %s", msg)}
			return
		}
		if query.Get("state") != state {
			writeCallbackPage(w, false, "state mismatch")
			done <- result{err: errors.New("sign-in failed: OAuth state mismatch")}
			return
		}
		code := query.Get("code")
		if code == "" {
			writeCallbackPage(w, false, "missing authorization code")
			done <- result{err: errors.New("sign-in failed: missing authorization code")}
			return
		}
		cred, err := exchangeCode(r.Context(), code, verifier, codexRedirectURI)
		if err != nil {
			writeCallbackPage(w, false, err.Error())
			done <- result{err: err}
			return
		}
		writeCallbackPage(w, true, "")
		done <- result{cred: cred}
	})
	go func() { _ = srv.Serve(ln) }()
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	if opts.Log != nil {
		opts.Log("Open this URL to sign in with your ChatGPT account:\n\n  " + authURL + "\n")
	}
	if opts.OpenBrowser != nil {
		opts.OpenBrowser(authURL)
	}
	select {
	case <-ctx.Done():
		return Credential{}, ctx.Err()
	case res := <-done:
		return res.cred, res.err
	}
}

// CodexDeviceLogin runs the device-code flow for headless machines.
func CodexDeviceLogin(ctx context.Context, log func(string)) (Credential, error) {
	issuer := codexIssuer()
	body, _ := json.Marshal(map[string]string{"client_id": CodexClientID})
	var start struct {
		DeviceAuthID string          `json:"device_auth_id"`
		UserCode     string          `json:"user_code"`
		UserCodeAlt  string          `json:"usercode"`
		Interval     json.RawMessage `json:"interval"`
	}
	if err := postJSON(ctx, issuer+"/api/accounts/deviceauth/usercode", body, &start); err != nil {
		return Credential{}, fmt.Errorf("device login is unavailable: %w", err)
	}
	userCode := start.UserCode
	if userCode == "" {
		userCode = start.UserCodeAlt
	}
	if start.DeviceAuthID == "" || userCode == "" {
		return Credential{}, errors.New("device login returned no code")
	}
	interval := 5 * time.Second
	var secs float64
	if err := json.Unmarshal(start.Interval, &secs); err == nil && secs > 0 {
		interval = time.Duration(secs * float64(time.Second))
	} else {
		var s string
		if json.Unmarshal(start.Interval, &s) == nil {
			if d, err := time.ParseDuration(s + "s"); err == nil && d > 0 {
				interval = d
			}
		}
	}
	if log != nil {
		log(fmt.Sprintf("Open %s and enter this one-time code:\n\n  %s\n", issuer+"/codex/device", userCode))
	}
	deadline := time.Now().Add(15 * time.Minute)
	pollBody, _ := json.Marshal(map[string]string{"device_auth_id": start.DeviceAuthID, "user_code": userCode})
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return Credential{}, ctx.Err()
		case <-time.After(interval):
		}
		var tok struct {
			AuthorizationCode string `json:"authorization_code"`
			CodeVerifier      string `json:"code_verifier"`
		}
		err := postJSON(ctx, issuer+"/api/accounts/deviceauth/token", pollBody, &tok)
		var se *statusError
		if errors.As(err, &se) && (se.status == http.StatusForbidden || se.status == http.StatusNotFound) {
			continue // still pending
		}
		if err != nil {
			return Credential{}, err
		}
		if tok.AuthorizationCode == "" {
			continue
		}
		return exchangeCode(ctx, tok.AuthorizationCode, tok.CodeVerifier, issuer+"/deviceauth/callback")
	}
	return Credential{}, errors.New("device login timed out after 15 minutes")
}

func exchangeCode(ctx context.Context, code, verifier, redirectURI string) (Credential, error) {
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", redirectURI)
	form.Set("client_id", CodexClientID)
	form.Set("code_verifier", verifier)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, codexIssuer()+"/oauth/token", strings.NewReader(form.Encode()))
	if err != nil {
		return Credential{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	var tok tokenResponse
	if err := doJSON(req, &tok); err != nil {
		return Credential{}, fmt.Errorf("token exchange failed: %w", err)
	}
	if tok.AccessToken == "" {
		return Credential{}, errors.New("token exchange returned no access token")
	}
	return credentialFromTokens(tok, Credential{}), nil
}

func credentialFromTokens(tok tokenResponse, prev Credential) Credential {
	c := prev
	c.AccessToken = tok.AccessToken
	if tok.RefreshToken != "" {
		c.RefreshToken = tok.RefreshToken
	}
	if tok.IDToken != "" {
		c.IDToken = tok.IDToken
	}
	if id := AccountIDFromToken(c.IDToken); id != "" {
		c.AccountID = id
	} else if id := AccountIDFromToken(c.AccessToken); id != "" {
		c.AccountID = id
	}
	c.LastRefresh = time.Now().UTC().Format(time.RFC3339)
	return c
}

// RefreshCodex exchanges the refresh token for a new access token.
func RefreshCodex(ctx context.Context, c Credential) (Credential, error) {
	if c.RefreshToken == "" {
		return c, errors.New("no Codex refresh token; run `ai login codex` again")
	}
	body, _ := json.Marshal(map[string]string{
		"client_id":     CodexClientID,
		"grant_type":    "refresh_token",
		"refresh_token": c.RefreshToken,
		"scope":         "openid profile email",
	})
	var tok tokenResponse
	if err := postJSON(ctx, codexIssuer()+"/oauth/token", body, &tok); err != nil {
		// OAuth error bodies can contain credentials; report the status only.
		var se *statusError
		if errors.As(err, &se) {
			return c, fmt.Errorf("Codex token refresh failed (HTTP %d); run `ai login codex` again", se.status)
		}
		return c, fmt.Errorf("Codex token refresh failed: %w", err)
	}
	if tok.AccessToken == "" {
		return c, errors.New("Codex token refresh returned no access token")
	}
	return credentialFromTokens(tok, c), nil
}

// JWTClaims decodes a JWT payload without verifying it.
func JWTClaims(token string) map[string]any {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return nil
	}
	var claims map[string]any
	if json.Unmarshal(raw, &claims) != nil {
		return nil
	}
	return claims
}

// AccountIDFromToken reads the ChatGPT account id claim.
func AccountIDFromToken(token string) string {
	claims := JWTClaims(token)
	if claims == nil {
		return ""
	}
	if authClaim, ok := claims["https://api.openai.com/auth"].(map[string]any); ok {
		if id, ok := authClaim["chatgpt_account_id"].(string); ok {
			return id
		}
	}
	return ""
}

// CodexPlan returns the ChatGPT plan type claim, when present.
func CodexPlan(token string) string {
	claims := JWTClaims(token)
	if authClaim, ok := claims["https://api.openai.com/auth"].(map[string]any); ok {
		if plan, ok := authClaim["chatgpt_plan_type"].(string); ok {
			return plan
		}
	}
	return ""
}

// CodexEmail returns the email claim, when present.
func CodexEmail(token string) string {
	claims := JWTClaims(token)
	if email, ok := claims["email"].(string); ok {
		return email
	}
	if email, ok := claims["https://api.openai.com/profile.email"].(string); ok {
		return email
	}
	return ""
}

// TokenExpiring reports whether a JWT expires within leeway.
func TokenExpiring(token string, leeway time.Duration) bool {
	claims := JWTClaims(token)
	exp, ok := claims["exp"].(float64)
	if !ok {
		return false
	}
	return time.Unix(int64(exp), 0).Before(time.Now().Add(leeway))
}

// CodexCLIAuthPath returns $CODEX_HOME/auth.json or ~/.codex/auth.json.
func CodexCLIAuthPath() string {
	if home := os.Getenv("CODEX_HOME"); home != "" {
		return filepath.Join(home, "auth.json")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".codex", "auth.json")
}

type codexCLIFile struct {
	Tokens *struct {
		IDToken      json.RawMessage `json:"id_token"`
		AccessToken  string          `json:"access_token"`
		RefreshToken string          `json:"refresh_token"`
		AccountID    string          `json:"account_id"`
	} `json:"tokens"`
}

// ReadCodexCLIAuth loads ChatGPT tokens written by `codex login`.
func ReadCodexCLIAuth() (Credential, error) {
	path := CodexCLIAuthPath()
	data, err := os.ReadFile(path)
	if err != nil {
		return Credential{}, err
	}
	var f codexCLIFile
	if err := json.Unmarshal(data, &f); err != nil {
		return Credential{}, fmt.Errorf("invalid %s: %w", path, err)
	}
	if f.Tokens == nil || f.Tokens.AccessToken == "" {
		return Credential{}, fmt.Errorf("%s has no ChatGPT tokens; run `codex login` with ChatGPT", path)
	}
	var idToken string
	if err := json.Unmarshal(f.Tokens.IDToken, &idToken); err != nil {
		var obj struct {
			RawJWT string `json:"raw_jwt"`
		}
		_ = json.Unmarshal(f.Tokens.IDToken, &obj)
		idToken = obj.RawJWT
	}
	c := Credential{
		AccessToken:  f.Tokens.AccessToken,
		RefreshToken: f.Tokens.RefreshToken,
		IDToken:      idToken,
		AccountID:    f.Tokens.AccountID,
	}
	if c.AccountID == "" {
		c.AccountID = AccountIDFromToken(idToken)
	}
	return c, nil
}

type statusError struct {
	status int
	body   string
}

func (e *statusError) Error() string {
	msg := strings.TrimSpace(e.body)
	var parsed struct {
		Error any `json:"error"`
		Desc  any `json:"error_description"`
	}
	if json.Unmarshal([]byte(msg), &parsed) == nil && parsed.Error != nil {
		msg = fmt.Sprint(parsed.Error)
		if parsed.Desc != nil {
			msg += ": " + fmt.Sprint(parsed.Desc)
		}
	}
	if len(msg) > 300 {
		msg = msg[:300] + "…"
	}
	return fmt.Sprintf("HTTP %d: %s", e.status, msg)
}

func postJSON(ctx context.Context, u string, body []byte, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	return doJSON(req, out)
}

func doJSON(req *http.Request, out any) error {
	req.Header.Set("Accept", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if res.StatusCode < 200 || res.StatusCode > 299 {
		return &statusError{status: res.StatusCode, body: string(data)}
	}
	return json.Unmarshal(data, out)
}

func randomString(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)[:n]
}

func writeCallbackPage(w http.ResponseWriter, ok bool, msg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	title, body := "Signed in to ai-cli", "You can close this tab and return to your terminal."
	if !ok {
		w.WriteHeader(http.StatusBadRequest)
		title, body = "Sign-in failed", html.EscapeString(msg)
	}
	fmt.Fprintf(w, `<!doctype html><html><head><meta charset="utf-8"><title>%s</title>
<style>body{font-family:system-ui,sans-serif;background:#0a0a0a;color:#ededed;display:grid;place-items:center;height:100vh;margin:0}main{text-align:center}p{color:#a1a1a1}</style>
</head><body><main><h1>%s</h1><p>%s</p></main></body></html>`, title, title, body)
}

// WriteCodexCLIAuth updates the tokens in Codex CLI's auth.json in place,
// preserving every other field, so refreshed tokens stay valid for Codex too.
func WriteCodexCLIAuth(c Credential) error {
	path := CodexCLIAuthPath()
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		return err
	}
	tokens, _ := doc["tokens"].(map[string]any)
	if tokens == nil {
		tokens = map[string]any{}
	}
	tokens["access_token"] = c.AccessToken
	if c.RefreshToken != "" {
		tokens["refresh_token"] = c.RefreshToken
	}
	if c.IDToken != "" {
		if _, isString := tokens["id_token"].(string); isString || tokens["id_token"] == nil {
			tokens["id_token"] = c.IDToken
		}
	}
	if c.AccountID != "" {
		tokens["account_id"] = c.AccountID
	}
	doc["tokens"] = tokens
	doc["last_refresh"] = time.Now().UTC().Format(time.RFC3339Nano)
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".ai-cli.tmp"
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
