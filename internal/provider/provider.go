// Package provider implements image generation backends.
package provider

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/lassejlv/ai-cli/internal/auth"
	"github.com/lassejlv/ai-cli/internal/media"
)

// Request is a provider-neutral image generation request.
type Request struct {
	Model       string
	Prompt      string
	Images      []media.Image
	Mask        *media.Image
	N           int
	Size        string // WxH
	AspectRatio string // W:H
	Seed        *int64
	// Options are the provider's entry from --provider-options. They are
	// merged into the provider request body unchanged.
	Options map[string]any
	// GatewayOptions is the full providerOptions object for the AI Gateway,
	// which routes provider-keyed options itself.
	GatewayOptions map[string]any
	Headers        map[string]string
}

// Image is one generated artifact.
type Image struct {
	Data      []byte `json:"-"`
	MediaType string `json:"mediaType"`
	URL       string `json:"url,omitempty"`
}

// Usage reports token usage when a provider returns it.
type Usage struct {
	InputTokens  *int `json:"inputTokens,omitempty"`
	OutputTokens *int `json:"outputTokens,omitempty"`
	TotalTokens  *int `json:"totalTokens,omitempty"`
}

// Result is the output of one provider call.
type Result struct {
	Images           []Image  `json:"-"`
	ID               string   `json:"id,omitempty"`
	Text             string   `json:"text,omitempty"`
	RevisedPrompt    string   `json:"revisedPrompt,omitempty"`
	Warnings         []string `json:"warnings,omitempty"`
	Usage            *Usage   `json:"usage,omitempty"`
	ProviderMetadata any      `json:"providerMetadata,omitempty"`
}

// Provider generates images.
type Provider interface {
	// Generate performs one call that returns up to req.N images.
	Generate(ctx context.Context, req Request) (*Result, error)
	// MaxImagesPerCall is the default batching limit for a model.
	MaxImagesPerCall(model string) int
}

// Info describes a provider for help, login and model listings.
type Info struct {
	Name         string
	Title        string
	DefaultModel string
	Models       []ModelInfo
	// Auth is how `ai login` collects credentials: "apikey", "keypair" or "oauth".
	Auth     string
	EnvVars  []string
	KeysURL  string
	Docs     string
	New      func(Config) (Provider, error)
	Verify   func(ctx context.Context, cfg Config) error
	Aliases  map[string]string
	Optional bool
}

// ModelInfo is a known model for listings and short-name resolution.
type ModelInfo struct {
	ID          string
	Description string
}

// Config is the resolved connection configuration for a provider.
type Config struct {
	APIKey     string
	APISecret  string
	BaseURL    string
	Scope      string
	Headers    map[string]string
	MaxRetries int
	HTTP       *http.Client
	// Source describes where credentials came from (env, login, codex-cli).
	Source string
	// OAuth holds subscription tokens (Codex, Grok). RefreshOAuth refreshes
	// them in the backing store under a cross-process lock; when nil, tokens
	// are refreshed in memory only.
	OAuth        *auth.Credential
	RefreshOAuth OAuthRefresher
}

// OAuthRefresher refreshes stored OAuth tokens, reusing tokens another
// process refreshed meanwhile.
type OAuthRefresher func(current auth.Credential, force bool, expiring func(auth.Credential) bool, refresh auth.RefreshFunc) (auth.Credential, error)

var registry = map[string]*Info{}

// Register adds a provider to the registry.
func Register(info *Info) { registry[info.Name] = info }

// Lookup returns a registered provider.
func Lookup(name string) (*Info, bool) {
	info, ok := registry[strings.ToLower(name)]
	return info, ok
}

// All returns registered providers sorted by name.
func All() []*Info {
	out := make([]*Info, 0, len(registry))
	for _, info := range registry {
		out = append(out, info)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Names returns registered provider names.
func Names() []string {
	var names []string
	for _, info := range All() {
		names = append(names, info.Name)
	}
	return names
}

// ResolveModel expands provider aliases and short names.
func (info *Info) ResolveModel(model string) string {
	if model == "" {
		return info.DefaultModel
	}
	if alias, ok := info.Aliases[model]; ok {
		return alias
	}
	return model
}

// AuthError marks a rejected or missing credential.
type AuthError struct {
	Provider string
	Message  string
}

func (e *AuthError) Error() string { return e.Message }

// NewAuthError builds a login hint for a provider.
func NewAuthError(provider, source string) *AuthError {
	info, _ := Lookup(provider)
	title := provider
	env := ""
	if info != nil {
		title = info.Title
		if len(info.EnvVars) > 0 {
			env = info.EnvVars[0]
		}
	}
	switch source {
	case "env":
		return &AuthError{provider, fmt.Sprintf("%s rejected %s. Check the key, or unset it and run `ai login %s`.", title, env, provider)}
	case "login", "codex-cli":
		return &AuthError{provider, fmt.Sprintf("%s rejected the saved credentials. Run `ai login %s` again.", title, provider)}
	}
	if env != "" {
		return &AuthError{provider, fmt.Sprintf("Not signed in to %s. Run `ai login %s`, or set %s.", title, provider, env)}
	}
	return &AuthError{provider, fmt.Sprintf("Not signed in to %s. Run `ai login %s`.", title, provider)}
}
