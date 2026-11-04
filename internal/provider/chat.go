package provider

import (
	"context"
	"net/http"

	"github.com/lassejlv/ai-cli/internal/media"
)

type chatImageConfig struct {
	Provider   string
	Name       string
	Source     string
	URL        string
	Headers    map[string]string
	MaxRetries int
	Client     *http.Client
}

// chatImageGenerate calls an OpenAI-compatible chat completions endpoint with
// image output modalities (AI Gateway, OpenRouter).
func chatImageGenerate(ctx context.Context, cfg chatImageConfig, req Request) (*Result, error) {
	prompt := req.Prompt
	if prompt == "" {
		prompt = "Generate an image"
	}
	content := []any{}
	for _, img := range req.Images {
		u := img.URL
		if len(img.Data) > 0 {
			var err error
			u, err = media.DataURL(ctx, cfg.Client, img)
			if err != nil {
				return nil, err
			}
		}
		content = append(content, map[string]any{"type": "image_url", "image_url": map[string]any{"url": u}})
	}
	content = append(content, map[string]any{"type": "text", "text": prompt})
	body := map[string]any{
		"model":      req.Model,
		"messages":   []any{map[string]any{"role": "user", "content": content}},
		"modalities": []string{"image", "text"},
		"stream":     false,
	}
	if req.Seed != nil {
		body["seed"] = *req.Seed
	}
	if req.AspectRatio != "" {
		body["image_config"] = map[string]any{"aspect_ratio": req.AspectRatio}
	}
	body = merge(body, req.Options)
	res, err := do(ctx, call{
		Provider:   cfg.Provider,
		URL:        cfg.URL,
		Headers:    cfg.Headers,
		JSON:       body,
		MaxRetries: cfg.MaxRetries,
		Client:     cfg.Client,
	})
	if err != nil {
		return nil, authCheck(cfg.Name, cfg.Source, err)
	}
	var parsed struct {
		ID      string `json:"id"`
		Choices []struct {
			Message struct {
				Content any `json:"content"`
				Images  []struct {
					ImageURL struct {
						URL string `json:"url"`
					} `json:"image_url"`
				} `json:"images"`
			} `json:"message"`
		} `json:"choices"`
		Usage *struct {
			PromptTokens     *int `json:"prompt_tokens"`
			CompletionTokens *int `json:"completion_tokens"`
			TotalTokens      *int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := res.JSONInto(&parsed); err != nil {
		return nil, err
	}
	out := &Result{ID: parsed.ID}
	if parsed.Usage != nil {
		out.Usage = &Usage{InputTokens: parsed.Usage.PromptTokens, OutputTokens: parsed.Usage.CompletionTokens, TotalTokens: parsed.Usage.TotalTokens}
	}
	var urls []string
	for _, choice := range parsed.Choices {
		if s, ok := choice.Message.Content.(string); ok && s != "" {
			out.Text += s
		}
		for _, img := range choice.Message.Images {
			if img.ImageURL.URL != "" {
				urls = append(urls, img.ImageURL.URL)
			}
		}
	}
	images, err := downloadImages(ctx, cfg.Client, urls, nil)
	if err != nil {
		return nil, err
	}
	out.Images = images
	return out, nil
}
