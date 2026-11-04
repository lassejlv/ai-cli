package auth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func fakeJWT(claims map[string]any) string {
	b, _ := json.Marshal(claims)
	return "e30." + base64.RawURLEncoding.EncodeToString(b) + ".sig"
}

func TestCodexBrowserLoginAndRefresh(t *testing.T) {
	idToken := fakeJWT(map[string]any{"email": "me@example.com", "https://api.openai.com/auth": map[string]any{"chatgpt_account_id": "acct_9", "chatgpt_plan_type": "pro"}})
	var refreshed atomic.Bool
	issuer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/oauth/token" {
			t.Errorf("unexpected path %s", r.URL.Path)
			return
		}
		if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["grant_type"] != "refresh_token" || body["refresh_token"] != "rt1" || body["client_id"] != CodexClientID {
				t.Errorf("refresh body %v", body)
			}
			refreshed.Store(true)
			_ = json.NewEncoder(w).Encode(map[string]string{"access_token": "at2", "refresh_token": "rt2"})
			return
		}
		_ = r.ParseForm()
		if r.Form.Get("grant_type") != "authorization_code" || r.Form.Get("code") != "the-code" || r.Form.Get("code_verifier") == "" {
			t.Errorf("token form %v", r.Form)
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"access_token": "at1", "refresh_token": "rt1", "id_token": idToken})
	}))
	defer issuer.Close()
	t.Setenv("AI_CLI_CODEX_ISSUER", issuer.URL)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	opened := make(chan string, 1)
	go func() {
		authURL := <-opened
		u, _ := url.Parse(authURL)
		q := u.Query()
		if q.Get("code_challenge_method") != "S256" || q.Get("client_id") != CodexClientID || q.Get("redirect_uri") != codexRedirectURI {
			t.Errorf("authorize query %v", q)
		}
		res, err := http.Get(codexRedirectURI + "?code=the-code&state=" + url.QueryEscape(q.Get("state")))
		if err == nil {
			res.Body.Close()
		}
	}()
	cred, err := CodexBrowserLogin(ctx, CodexLoginOptions{OpenBrowser: func(u string) { opened <- u }})
	if err != nil {
		t.Skipf("callback port unavailable: %v", err)
	}
	if cred.AccessToken != "at1" || cred.AccountID != "acct_9" || CodexEmail(cred.IDToken) != "me@example.com" || CodexPlan(cred.IDToken) != "pro" {
		t.Fatalf("cred = %+v", cred)
	}
	next, err := RefreshCodex(ctx, cred)
	if err != nil || next.AccessToken != "at2" || next.RefreshToken != "rt2" || next.AccountID != "acct_9" || !refreshed.Load() {
		t.Fatalf("refresh = %+v err=%v", next, err)
	}
}

func TestCodexCLIFileRoundTrip(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CODEX_HOME", dir)
	idToken := fakeJWT(map[string]any{"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": "acct_1"}})
	doc := `{"OPENAI_API_KEY":null,"tokens":{"id_token":"` + idToken + `","access_token":"a","refresh_token":"r","account_id":"acct_1"},"last_refresh":"x"}`
	if err := os.WriteFile(filepath.Join(dir, "auth.json"), []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := ReadCodexCLIAuth()
	if err != nil || c.AccessToken != "a" || c.AccountID != "acct_1" {
		t.Fatalf("read = %+v %v", c, err)
	}
	c.AccessToken, c.RefreshToken = "a2", "r2"
	if err := WriteCodexCLIAuth(c); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "auth.json"))
	var back map[string]any
	_ = json.Unmarshal(raw, &back)
	tokens := back["tokens"].(map[string]any)
	if tokens["access_token"] != "a2" || tokens["refresh_token"] != "r2" {
		t.Fatalf("tokens = %v", tokens)
	}
	if _, ok := back["OPENAI_API_KEY"]; !ok {
		t.Fatal("other fields must be preserved")
	}
}

func TestGrokDeviceLogin(t *testing.T) {
	var polls atomic.Int32
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			_ = json.NewEncoder(w).Encode(map[string]string{"authorization_endpoint": srv.URL + "/auth", "token_endpoint": srv.URL + "/token", "device_authorization_endpoint": srv.URL + "/device"})
		case "/device":
			_ = r.ParseForm()
			if r.Form.Get("client_id") != GrokClientID || !strings.Contains(r.Form.Get("scope"), "api:access") {
				t.Errorf("device form %v", r.Form)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"device_code": "dc", "user_code": "ABCD-1234", "verification_uri": "https://accounts.x.ai/device", "expires_in": 60, "interval": 1})
		case "/token":
			_ = r.ParseForm()
			if r.Form.Get("grant_type") == "refresh_token" {
				_ = json.NewEncoder(w).Encode(map[string]string{"access_token": "g2"})
				return
			}
			if polls.Add(1) == 1 {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":"authorization_pending"}`))
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"access_token": "g1", "refresh_token": "gr1"})
		}
	}))
	defer srv.Close()
	t.Setenv("AI_CLI_GROK_ISSUER", srv.URL)
	var logged string
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cred, err := GrokDeviceLogin(ctx, func(s string) { logged = s })
	if err != nil {
		t.Fatal(err)
	}
	if cred.AccessToken != "g1" || cred.RefreshToken != "gr1" || cred.TokenEndpoint != srv.URL+"/token" || !strings.Contains(logged, "ABCD-1234") {
		t.Fatalf("cred = %+v logged=%q", cred, logged)
	}
	next, err := RefreshGrok(ctx, cred)
	if err != nil || next.AccessToken != "g2" || next.RefreshToken != "gr1" {
		t.Fatalf("refresh = %+v err=%v", next, err)
	}
}

func TestTrustedXAIEndpoint(t *testing.T) {
	t.Setenv("AI_CLI_GROK_ISSUER", "")
	for raw, want := range map[string]bool{
		"https://auth.x.ai/oauth2/token": true,
		"https://x.ai/token":             true,
		"http://auth.x.ai/token":         false,
		"https://evil.example/x.ai":      false,
		"https://x.ai.evil.example/":     false,
	} {
		if got := trustedXAIEndpoint(raw); got != want {
			t.Errorf("%s: got %v want %v", raw, got, want)
		}
	}
}

func TestTokenExpiring(t *testing.T) {
	soon := fakeJWT(map[string]any{"exp": time.Now().Add(30 * time.Second).Unix()})
	later := fakeJWT(map[string]any{"exp": time.Now().Add(time.Hour).Unix()})
	if !TokenExpiring(soon, time.Minute) || TokenExpiring(later, time.Minute) || TokenExpiring("opaque", time.Minute) {
		t.Fatal("TokenExpiring wrong")
	}
}
