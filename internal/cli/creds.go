package cli

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/lassejlv/ai-cli/internal/auth"
	"github.com/lassejlv/ai-cli/internal/provider"
)

// providerPriority orders providers for automatic default selection.
var providerPriority = []string{"gateway", "openai", "codex", "grok", "xai", "google", "fal", "higgsfield", "replicate", "bfl", "openrouter", "stability"}

// Overrides are connection flags that apply to every provider in a run.
type Overrides struct {
	BaseURL    string
	Scope      string
	Headers    map[string]string
	MaxRetries int
}

// envKey returns the first set environment variable for a provider.
func envKey(info *provider.Info) (string, string) {
	if info.Name == "higgsfield" {
		if k := os.Getenv("HF_KEY"); k != "" {
			return k, "HF_KEY"
		}
		if k := os.Getenv("HF_CREDENTIALS"); k != "" {
			return k, "HF_CREDENTIALS"
		}
		if k, s := os.Getenv("HF_API_KEY"), os.Getenv("HF_API_SECRET"); k != "" && s != "" {
			return k + ":" + s, "HF_API_KEY"
		}
		if k := os.Getenv("HIGGSFIELD_API_KEY"); k != "" {
			return k, "HIGGSFIELD_API_KEY"
		}
		return "", ""
	}
	for _, name := range info.EnvVars {
		if v := strings.TrimSpace(os.Getenv(name)); v != "" {
			return v, name
		}
	}
	return "", ""
}

// ResolvedAccount describes the credential a run uses.
type ResolvedAccount struct {
	Provider string `json:"provider"`
	Account  string `json:"account,omitempty"`
	Source   string `json:"source"` // env, login, codex-cli, none
	EnvVar   string `json:"envVar,omitempty"`
}

// resolveConfig builds a provider configuration from flags, env and saved accounts.
// Environment variables win unless an account is named explicitly.
func resolveConfig(info *provider.Info, account string, ov Overrides) (provider.Config, ResolvedAccount, error) {
	cfg := provider.Config{BaseURL: ov.BaseURL, Scope: ov.Scope, Headers: ov.Headers, MaxRetries: ov.MaxRetries}
	ra := ResolvedAccount{Provider: info.Name, Source: "none"}
	if cfg.Scope == "" && info.Name == "gateway" {
		cfg.Scope = os.Getenv("AI_GATEWAY_TEAM_ID_OR_SLUG")
	}

	if account == "" && info.Auth != "oauth" {
		if key, name := envKey(info); key != "" {
			cfg.APIKey = key
			cfg.Source, ra.Source, ra.EnvVar = "env", "env", name
			splitKeypair(info, &cfg)
			return cfg, ra, nil
		}
	}

	f, err := auth.Load()
	if err != nil {
		return cfg, ra, err
	}
	cred, name, ok := f.Lookup(info.Name, account)
	if !ok && account != "" {
		return cfg, ra, fmt.Errorf("no %s account named %q; saved accounts: %s (run `ai login %s --account %s`)",
			info.Name, account, joinOrNone(f.AccountNames(info.Name)), info.Name, account)
	}
	if ok {
		ra.Account, ra.Source = name, "login"
		cfg.Source = "login"
		cfg.APIKey, cfg.APISecret = cred.APIKey, cred.APISecret
		if cfg.BaseURL == "" {
			cfg.BaseURL = cred.BaseURL
		}
		if info.Auth == "oauth" {
			c := cred
			cfg.OAuth = &c
			providerName, accountName := info.Name, name
			cfg.SaveOAuth = func(updated auth.Credential) error {
				return auth.Replace(providerName, accountName, updated)
			}
		}
		splitKeypair(info, &cfg)
		return cfg, ra, nil
	}

	// Fall back to the tokens written by the official Codex CLI.
	if info.Name == "codex" {
		if cred, err := auth.ReadCodexCLIAuth(); err == nil {
			c := cred
			cfg.OAuth = &c
			cfg.Source, ra.Source, ra.Account = "codex-cli", "codex-cli", "codex-cli"
			cfg.SaveOAuth = auth.WriteCodexCLIAuth
			return cfg, ra, nil
		}
	}
	return cfg, ra, nil
}

func splitKeypair(info *provider.Info, cfg *provider.Config) {
	if info.Auth == "keypair" && cfg.APISecret == "" {
		if id, secret, ok := strings.Cut(cfg.APIKey, ":"); ok {
			cfg.APIKey, cfg.APISecret = id, secret
		}
	}
}

func joinOrNone(names []string) string {
	if len(names) == 0 {
		return "none"
	}
	return strings.Join(names, ", ")
}

// isConfigured reports whether a provider has usable credentials.
func isConfigured(info *provider.Info) bool {
	_, ra, err := resolveConfig(info, "", Overrides{})
	if err != nil {
		return false
	}
	if ra.Source != "none" {
		return true
	}
	// OpenAI-compatible servers may run without a key.
	return info.Name == "openai" && os.Getenv("OPENAI_BASE_URL") != ""
}

// defaultProvider chooses the provider for unprefixed model IDs.
func defaultProvider(flag string) (string, error) {
	if flag != "" {
		if _, ok := provider.Lookup(flag); !ok {
			return "", fmt.Errorf("unknown provider %q (available: %s)", flag, strings.Join(provider.Names(), ", "))
		}
		return strings.ToLower(flag), nil
	}
	if env := os.Getenv("AI_CLI_IMAGE_PROVIDER"); env != "" {
		if _, ok := provider.Lookup(env); !ok {
			return "", fmt.Errorf("AI_CLI_IMAGE_PROVIDER: unknown provider %q", env)
		}
		return strings.ToLower(env), nil
	}
	if f, err := auth.Load(); err == nil && f.DefaultProvider != "" {
		if _, ok := provider.Lookup(f.DefaultProvider); ok {
			return f.DefaultProvider, nil
		}
	}
	for _, name := range providerPriority {
		if info, ok := provider.Lookup(name); ok && isConfigured(info) {
			return name, nil
		}
	}
	return "gateway", nil
}

// ModelSpec is a parsed -m entry: provider[@account]:model.
type ModelSpec struct {
	Provider string `json:"provider"`
	Account  string `json:"account,omitempty"`
	Model    string `json:"model"`
}

// String renders the canonical spec.
func (s ModelSpec) String() string {
	p := s.Provider
	if s.Account != "" {
		p += "@" + s.Account
	}
	return p + ":" + s.Model
}

// ParseModelSpec parses one model entry. A prefix before the first colon is
// only treated as a provider when it names a registered provider, so
// Replicate-style owner/model:version IDs pass through unchanged.
func ParseModelSpec(raw, defProvider, defAccount string) (ModelSpec, error) {
	raw = strings.TrimSpace(raw)
	spec := ModelSpec{Provider: defProvider, Account: defAccount}
	head, rest, hasColon := strings.Cut(raw, ":")
	provName, account, hasAt := strings.Cut(head, "@")
	if _, known := provider.Lookup(provName); known && (hasColon || hasAt || !strings.Contains(raw, "/")) {
		spec.Provider = strings.ToLower(provName)
		if hasAt {
			if account == "" {
				return spec, fmt.Errorf("empty account in model %q", raw)
			}
			spec.Account = account
		}
		if hasColon {
			spec.Model = rest
		}
	} else {
		spec.Model = raw
	}
	info, ok := provider.Lookup(spec.Provider)
	if !ok {
		return spec, fmt.Errorf("unknown provider %q", spec.Provider)
	}
	spec.Model = info.ResolveModel(spec.Model)
	for _, r := range spec.Model {
		if r <= ' ' || r > '~' {
			return spec, fmt.Errorf("model ID must use printable ASCII characters without spaces (got %q)", spec.Model)
		}
	}
	return spec, nil
}

// ResolveModelSpecs parses a comma-separated -m value.
func ResolveModelSpecs(ctx context.Context, models, providerFlag, accountFlag string) ([]ModelSpec, error) {
	defProvider, err := defaultProvider(providerFlag)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(models) == "" {
		models = os.Getenv("AI_CLI_IMAGE_MODEL")
	}
	var entries []string
	for _, part := range strings.Split(models, ",") {
		if p := strings.TrimSpace(part); p != "" {
			entries = append(entries, p)
		}
	}
	if len(entries) == 0 {
		entries = []string{defProvider + ":"}
	}
	specs := make([]ModelSpec, 0, len(entries))
	for _, entry := range entries {
		spec, err := ParseModelSpec(entry, defProvider, accountFlag)
		if err != nil {
			return nil, err
		}
		if spec.Provider == "gateway" {
			spec.Model = expandGatewayModel(ctx, spec.Model)
		}
		specs = append(specs, spec)
	}
	return specs, nil
}

func expandGatewayModel(ctx context.Context, model string) string {
	models, err := provider.GatewayImageModels(ctx)
	if err != nil || strings.Contains(model, "/") {
		return model
	}
	for _, m := range models {
		if strings.SplitN(m.ID, "/", 2)[len(strings.SplitN(m.ID, "/", 2))-1] == model {
			return m.ID
		}
	}
	return model
}
