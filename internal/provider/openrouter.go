package provider

import (
	"context"
	"net/http"
	"strings"
)

func init() {
	Register(&Info{
		Name:         "openrouter",
		Title:        "OpenRouter",
		DefaultModel: "google/gemini-2.5-flash-image",
		Auth:         "apikey",
		EnvVars:      []string{"OPENROUTER_API_KEY"},
		KeysURL:      "https://openrouter.ai/settings/keys",
		Docs:         "https://openrouter.ai/docs/features/multimodal/image-generation",
		New:          newOpenRouter,
		Verify: func(ctx context.Context, cfg Config) error {
			_, err := do(ctx, call{Provider: "OpenRouter", Method: http.MethodGet, URL: "https://openrouter.ai/api/v1/key",
				Headers: map[string]string{"Authorization": "Bearer " + cfg.APIKey}, Client: cfg.HTTP})
			return err
		},
		Models: []ModelInfo{
			{ID: "google/gemini-2.5-flash-image"},
			{ID: "google/gemini-3-pro-image-preview"},
			{ID: "openai/gpt-5-image"},
			{ID: "openai/gpt-5-image-mini"},
		},
	})
}

type openRouter struct{ cfg Config }

func newOpenRouter(cfg Config) (Provider, error) {
	if cfg.APIKey == "" {
		return nil, NewAuthError("openrouter", "")
	}
	return &openRouter{cfg: cfg}, nil
}

func (o *openRouter) MaxImagesPerCall(string) int { return 1 }

func (o *openRouter) Generate(ctx context.Context, req Request) (*Result, error) {
	base := "https://openrouter.ai/api/v1"
	if o.cfg.BaseURL != "" {
		base = strings.TrimRight(o.cfg.BaseURL, "/")
	}
	headers := withHeaders(map[string]string{
		"Authorization": "Bearer " + o.cfg.APIKey,
		"HTTP-Referer":  "https://github.com/lassejlv/ai-cli",
		"X-Title":       "ai-cli",
	}, withHeaders(o.cfg.Headers, req.Headers))
	return chatImageGenerate(ctx, chatImageConfig{
		Provider: "OpenRouter", Name: "openrouter", Source: o.cfg.Source,
		URL: base + "/chat/completions", Headers: headers, MaxRetries: o.cfg.MaxRetries, Client: o.cfg.HTTP,
	}, req)
}
