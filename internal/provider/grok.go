package provider

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/lassejlv/ai-cli/internal/auth"
)

func init() {
	Register(&Info{
		Name:         "grok",
		Title:        "xAI (Grok subscription)",
		DefaultModel: "grok-imagine-image",
		Auth:         "oauth",
		Docs:         "https://docs.x.ai/docs/guides/image-generations",
		New:          newGrok,
		Models: []ModelInfo{
			{ID: "grok-imagine-image", Description: "fast"},
			{ID: "grok-imagine-image-2.0", Description: "typography and layout aware"},
			{ID: "grok-imagine-image-quality", Description: "higher fidelity"},
		},
	})
}

// grok generates images with a SuperGrok / X Premium OAuth token against the
// xAI Images API.
type grok struct {
	cfg  Config
	mu   sync.Mutex
	cred auth.Credential
}

func newGrok(cfg Config) (Provider, error) {
	if cfg.OAuth == nil || cfg.OAuth.AccessToken == "" {
		return nil, NewAuthError("grok", "")
	}
	return &grok{cfg: cfg, cred: *cfg.OAuth}, nil
}

func (g *grok) MaxImagesPerCall(string) int { return 10 }

func (g *grok) credential(ctx context.Context, force bool) (auth.Credential, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	expiring := func(c auth.Credential) bool { return auth.TokenExpiring(c.AccessToken, 5*time.Minute) }
	if !force && !expiring(g.cred) {
		return g.cred, nil
	}
	refresh := func(c auth.Credential) (auth.Credential, error) { return auth.RefreshGrok(ctx, c) }
	var (
		next auth.Credential
		err  error
	)
	if g.cfg.RefreshOAuth != nil {
		next, err = g.cfg.RefreshOAuth(g.cred, force, expiring, refresh)
	} else {
		next, err = refresh(g.cred)
	}
	if err != nil {
		return g.cred, err
	}
	g.cred = next
	return next, nil
}

func (g *grok) client(cred auth.Credential) *openAICompat {
	base := "https://api.x.ai/v1"
	if g.cfg.BaseURL != "" {
		base = strings.TrimRight(g.cfg.BaseURL, "/")
	} else if v := os.Getenv("XAI_BASE_URL"); v != "" {
		base = strings.TrimRight(v, "/")
	}
	cfg := g.cfg
	cfg.APIKey = cred.AccessToken
	return &openAICompat{
		cfg: cfg, name: "grok", title: "xAI (Grok subscription)", base: base,
		aspect: true, jsonEdits: true,
		maxPerCall: func(string) int { return 10 },
	}
}

func (g *grok) Generate(ctx context.Context, req Request) (*Result, error) {
	cred, err := g.credential(ctx, false)
	if err != nil {
		return nil, err
	}
	res, err := g.client(cred).Generate(ctx, req)
	var authErr *AuthError
	if errors.As(err, &authErr) {
		if cred, err = g.credential(ctx, true); err != nil {
			return nil, err
		}
		res, err = g.client(cred).Generate(ctx, req)
	}
	if errors.As(err, &authErr) {
		return nil, &AuthError{Provider: "grok", Message: "xAI rejected the Grok subscription token. Run `ai login grok` again with the account that has SuperGrok or X Premium+; some tiers do not include API image generation."}
	}
	return res, err
}
