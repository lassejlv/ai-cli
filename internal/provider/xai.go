package provider

import (
	"context"
	"net/http"
	"strings"
)

func init() {
	Register(&Info{
		Name:         "xai",
		Title:        "xAI",
		DefaultModel: "grok-imagine-image",
		Auth:         "apikey",
		EnvVars:      []string{"XAI_API_KEY"},
		KeysURL:      "https://console.x.ai",
		Docs:         "https://docs.x.ai/docs/guides/image-generations",
		New:          newXAI,
		Verify: func(ctx context.Context, cfg Config) error {
			_, err := do(ctx, call{Provider: "xAI", Method: http.MethodGet, URL: xaiBase(cfg) + "/models",
				Headers: map[string]string{"Authorization": "Bearer " + cfg.APIKey}, Client: cfg.HTTP})
			return err
		},
		Models: []ModelInfo{
			{ID: "grok-imagine-image"},
			{ID: "grok-2-image"},
		},
	})
}

func xaiBase(cfg Config) string {
	if cfg.BaseURL != "" {
		return strings.TrimRight(cfg.BaseURL, "/")
	}
	return "https://api.x.ai/v1"
}

func newXAI(cfg Config) (Provider, error) {
	if cfg.APIKey == "" {
		return nil, NewAuthError("xai", "")
	}
	return &openAICompat{
		cfg: cfg, name: "xai", title: "xAI", base: xaiBase(cfg),
		aspect: true, jsonEdits: true,
		maxPerCall: func(string) int { return 10 },
	}, nil
}
