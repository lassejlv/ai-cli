package auth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// Grok (SuperGrok / X Premium) OAuth constants, matching xAI's Grok CLI client.
const (
	GrokIssuer       = "https://auth.x.ai"
	GrokClientID     = "b1a00492-073a-47ea-816f-4c329264a828"
	grokScope        = "openid profile email offline_access grok-cli:access api:access"
	grokCallbackAddr = "127.0.0.1:56121"
	grokCallbackPath = "/callback"
	grokDeviceGrant  = "urn:ietf:params:oauth:grant-type:device_code"
)

func grokIssuer() string {
	if v := os.Getenv("AI_CLI_GROK_ISSUER"); v != "" {
		return strings.TrimRight(v, "/")
	}
	return GrokIssuer
}

type grokDiscovery struct {
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
	DeviceEndpoint        string `json:"device_authorization_endpoint"`
}

func discoverGrok(ctx context.Context) (grokDiscovery, error) {
	issuer := grokIssuer()
	d := grokDiscovery{
		AuthorizationEndpoint: issuer + "/oauth2/authorize",
		TokenEndpoint:         issuer + "/oauth2/token",
		DeviceEndpoint:        issuer + "/oauth2/device/code",
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, issuer+"/.well-known/openid-configuration", nil)
	if err != nil {
		return d, err
	}
	var found grokDiscovery
	if err := doJSON(req, &found); err != nil {
		return d, fmt.Errorf("xAI OAuth discovery failed: %w", err)
	}
	for _, ep := range []*string{&found.AuthorizationEndpoint, &found.TokenEndpoint, &found.DeviceEndpoint} {
		if *ep != "" && !trustedXAIEndpoint(*ep) {
			return d, fmt.Errorf("xAI OAuth discovery returned an untrusted endpoint: %s", *ep)
		}
	}
	if found.AuthorizationEndpoint != "" {
		d.AuthorizationEndpoint = found.AuthorizationEndpoint
	}
	if found.TokenEndpoint != "" {
		d.TokenEndpoint = found.TokenEndpoint
	}
	if found.DeviceEndpoint != "" {
		d.DeviceEndpoint = found.DeviceEndpoint
	}
	return d, nil
}

// trustedXAIEndpoint pins OAuth endpoints to HTTPS on x.ai so a tampered
// discovery document cannot redirect refresh tokens elsewhere.
func trustedXAIEndpoint(raw string) bool {
	if os.Getenv("AI_CLI_GROK_ISSUER") != "" {
		return true
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" {
		return false
	}
	host := strings.ToLower(u.Hostname())
	return host == "x.ai" || strings.HasSuffix(host, ".x.ai")
}

// GrokBrowserLogin runs the OAuth PKCE flow with a loopback callback.
func GrokBrowserLogin(ctx context.Context, opts CodexLoginOptions) (Credential, error) {
	disc, err := discoverGrok(ctx)
	if err != nil {
		return Credential{}, err
	}
	verifier := randomString(64)
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])
	state := randomString(32)
	nonce := randomString(32)

	ln, err := net.Listen("tcp", grokCallbackAddr)
	if err != nil {
		ln, err = net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return Credential{}, fmt.Errorf("could not start the OAuth callback server: %w; try `ai login grok --device`", err)
		}
	}
	redirectURI := fmt.Sprintf("http://127.0.0.1:%d%s", ln.Addr().(*net.TCPAddr).Port, grokCallbackPath)

	authURL, err := url.Parse(disc.AuthorizationEndpoint)
	if err != nil {
		return Credential{}, err
	}
	q := authURL.Query()
	q.Set("response_type", "code")
	q.Set("client_id", GrokClientID)
	q.Set("redirect_uri", redirectURI)
	q.Set("scope", grokScope)
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "S256")
	q.Set("state", state)
	q.Set("nonce", nonce)
	q.Set("plan", "generic")
	q.Set("referrer", "ai-cli")
	authURL.RawQuery = q.Encode()

	type result struct {
		cred Credential
		err  error
	}
	done := make(chan result, 1)
	mux := http.NewServeMux()
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	mux.HandleFunc(grokCallbackPath, func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		if e := query.Get("error"); e != "" {
			writeCallbackPage(w, false, e)
			done <- result{err: fmt.Errorf("sign-in failed: %s", e)}
			return
		}
		if query.Get("state") != state || query.Get("code") == "" {
			writeCallbackPage(w, false, "invalid callback")
			done <- result{err: errors.New("sign-in failed: invalid OAuth callback")}
			return
		}
		cred, err := grokToken(r.Context(), disc.TokenEndpoint, url.Values{
			"grant_type":    {"authorization_code"},
			"client_id":     {GrokClientID},
			"code":          {query.Get("code")},
			"redirect_uri":  {redirectURI},
			"code_verifier": {verifier},
		}, Credential{})
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
		opts.Log("Open this URL to sign in with the account that has your SuperGrok or X Premium subscription:\n\n  " + authURL.String() + "\n")
	}
	if opts.OpenBrowser != nil {
		opts.OpenBrowser(authURL.String())
	}
	select {
	case <-ctx.Done():
		return Credential{}, ctx.Err()
	case res := <-done:
		return res.cred, res.err
	}
}

// GrokDeviceLogin runs the OAuth device-authorization grant.
func GrokDeviceLogin(ctx context.Context, log func(string)) (Credential, error) {
	disc, err := discoverGrok(ctx)
	if err != nil {
		return Credential{}, err
	}
	form := url.Values{"client_id": {GrokClientID}, "scope": {grokScope}, "referrer": {"ai-cli"}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, disc.DeviceEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return Credential{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	var device struct {
		DeviceCode              string `json:"device_code"`
		UserCode                string `json:"user_code"`
		VerificationURI         string `json:"verification_uri"`
		VerificationURIComplete string `json:"verification_uri_complete"`
		ExpiresIn               int    `json:"expires_in"`
		Interval                int    `json:"interval"`
	}
	if err := doJSON(req, &device); err != nil {
		return Credential{}, fmt.Errorf("xAI device-code request failed: %w", err)
	}
	display := device.VerificationURIComplete
	if display == "" {
		display = device.VerificationURI
	}
	if log != nil {
		log(fmt.Sprintf("Open %s and confirm this code:\n\n  %s\n", display, device.UserCode))
	}
	interval := 5 * time.Second
	if device.Interval > 0 {
		interval = time.Duration(device.Interval) * time.Second
	}
	deadline := time.Now().Add(time.Duration(max(device.ExpiresIn, 60)) * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return Credential{}, ctx.Err()
		case <-time.After(interval):
		}
		cred, err := grokToken(ctx, disc.TokenEndpoint, url.Values{
			"grant_type":  {grokDeviceGrant},
			"device_code": {device.DeviceCode},
			"client_id":   {GrokClientID},
		}, Credential{})
		if err == nil {
			return cred, nil
		}
		var se *statusError
		if errors.As(err, &se) {
			var body struct {
				Error string `json:"error"`
			}
			_ = json.Unmarshal([]byte(se.body), &body)
			switch body.Error {
			case "authorization_pending":
				continue
			case "slow_down":
				interval += 5 * time.Second
				continue
			case "access_denied":
				return Credential{}, errors.New("xAI device login was denied")
			case "expired_token":
				return Credential{}, errors.New("xAI device code expired; run `ai login grok --device` again")
			}
		}
		return Credential{}, err
	}
	return Credential{}, errors.New("xAI device code expired; run `ai login grok --device` again")
}

// RefreshGrok exchanges the refresh token for a new access token.
func RefreshGrok(ctx context.Context, c Credential) (Credential, error) {
	if c.RefreshToken == "" {
		return c, errors.New("no Grok refresh token; run `ai login grok` again")
	}
	endpoint := c.TokenEndpoint
	if endpoint == "" || !trustedXAIEndpoint(endpoint) {
		disc, err := discoverGrok(ctx)
		if err != nil {
			return c, err
		}
		endpoint = disc.TokenEndpoint
	}
	refreshed, err := grokToken(ctx, endpoint, url.Values{
		"grant_type":    {"refresh_token"},
		"client_id":     {GrokClientID},
		"refresh_token": {c.RefreshToken},
	}, c)
	if err != nil {
		var se *statusError
		if errors.As(err, &se) {
			return c, fmt.Errorf("Grok token refresh failed (HTTP %d); run `ai login grok` again", se.status)
		}
		return c, err
	}
	return refreshed, nil
}

func grokToken(ctx context.Context, endpoint string, form url.Values, prev Credential) (Credential, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return Credential{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	var tok tokenResponse
	if err := doJSON(req, &tok); err != nil {
		return Credential{}, err
	}
	if tok.AccessToken == "" {
		return Credential{}, errors.New("xAI returned no access token")
	}
	c := prev
	c.AccessToken = tok.AccessToken
	if tok.RefreshToken != "" {
		c.RefreshToken = tok.RefreshToken
	}
	if tok.IDToken != "" {
		c.IDToken = tok.IDToken
	}
	c.TokenEndpoint = endpoint
	c.LastRefresh = time.Now().UTC().Format(time.RFC3339)
	return c, nil
}
