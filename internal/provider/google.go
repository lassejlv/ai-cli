package provider

import (
	"context"
	"encoding/base64"
	"strings"

	"github.com/lassejlv/ai-cli/internal/media"
)

func init() {
	Register(&Info{
		Name:         "google",
		Title:        "Google Gemini",
		DefaultModel: "gemini-2.5-flash-image",
		Auth:         "apikey",
		EnvVars:      []string{"GEMINI_API_KEY", "GOOGLE_GENERATIVE_AI_API_KEY", "GOOGLE_API_KEY"},
		KeysURL:      "https://aistudio.google.com/apikey",
		Docs:         "https://ai.google.dev/gemini-api/docs/image-generation",
		New:          newGoogle,
		Aliases: map[string]string{
			"nano-banana":     "gemini-2.5-flash-image",
			"nano-banana-2":   "gemini-3.1-flash-image",
			"nano-banana-pro": "gemini-3-pro-image-preview",
		},
		// Imagen models (imagen-*) still route to :predict, but Google shut
		// Imagen 4 down on the Gemini API, so they are not listed.
		Models: []ModelInfo{
			{ID: "gemini-2.5-flash-image", Description: "alias: nano-banana"},
			{ID: "gemini-3.1-flash-image", Description: "alias: nano-banana-2"},
			{ID: "gemini-3-pro-image-preview", Description: "alias: nano-banana-pro"},
		},
	})
}

type google struct{ cfg Config }

func newGoogle(cfg Config) (Provider, error) {
	if cfg.APIKey == "" {
		return nil, NewAuthError("google", "")
	}
	return &google{cfg: cfg}, nil
}

func (g *google) base() string {
	if g.cfg.BaseURL != "" {
		return strings.TrimRight(g.cfg.BaseURL, "/")
	}
	return "https://generativelanguage.googleapis.com/v1beta"
}

func (g *google) headers(extra map[string]string) map[string]string {
	return withHeaders(withHeaders(map[string]string{"x-goog-api-key": g.cfg.APIKey}, g.cfg.Headers), extra)
}

func isImagen(model string) bool { return strings.HasPrefix(model, "imagen") }

func (g *google) MaxImagesPerCall(model string) int {
	if isImagen(model) {
		return 4
	}
	return 1
}

func (g *google) Generate(ctx context.Context, req Request) (*Result, error) {
	if isImagen(req.Model) {
		return g.imagen(ctx, req)
	}
	var warnings []string
	parts := []any{}
	for _, ref := range req.Images {
		img, err := media.Bytes(ctx, g.cfg.HTTP, ref)
		if err != nil {
			return nil, err
		}
		parts = append(parts, map[string]any{"inline_data": map[string]any{"mime_type": img.MediaType, "data": base64.StdEncoding.EncodeToString(img.Data)}})
	}
	prompt := req.Prompt
	if prompt == "" {
		prompt = "Generate an image"
	}
	parts = append(parts, map[string]any{"text": prompt})
	genConfig := map[string]any{"responseModalities": []string{"TEXT", "IMAGE"}}
	imageConfig := map[string]any{}
	if req.AspectRatio != "" {
		imageConfig["aspectRatio"] = req.AspectRatio
	}
	if len(imageConfig) > 0 {
		genConfig["imageConfig"] = imageConfig
	}
	if req.Seed != nil {
		genConfig["seed"] = *req.Seed
	}
	if req.Size != "" {
		warnings = append(warnings, `--size is not supported by Gemini; use --aspect-ratio or {"google":{"generationConfig":{"imageConfig":{"imageSize":"4K"}}}}`)
	}
	body := map[string]any{
		"contents":         []any{map[string]any{"role": "user", "parts": parts}},
		"generationConfig": genConfig,
	}
	body = merge(body, req.Options)
	res, err := do(ctx, call{Provider: "Google", URL: g.base() + "/models/" + req.Model + ":generateContent",
		Headers: g.headers(req.Headers), JSON: body, MaxRetries: g.cfg.MaxRetries, Client: g.cfg.HTTP})
	if err != nil {
		return nil, authCheck("google", g.cfg.Source, googleAuth(err))
	}
	var parsed struct {
		ResponseID string `json:"responseId"`
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text       string `json:"text"`
					InlineData *struct {
						MimeType string `json:"mimeType"`
						Data     string `json:"data"`
					} `json:"inlineData"`
				} `json:"parts"`
			} `json:"content"`
			FinishReason string `json:"finishReason"`
		} `json:"candidates"`
		UsageMetadata *struct {
			PromptTokenCount     *int `json:"promptTokenCount"`
			CandidatesTokenCount *int `json:"candidatesTokenCount"`
			TotalTokenCount      *int `json:"totalTokenCount"`
		} `json:"usageMetadata"`
	}
	if err := res.JSONInto(&parsed); err != nil {
		return nil, err
	}
	out := &Result{ID: parsed.ResponseID, Warnings: warnings}
	if u := parsed.UsageMetadata; u != nil {
		out.Usage = &Usage{InputTokens: u.PromptTokenCount, OutputTokens: u.CandidatesTokenCount, TotalTokens: u.TotalTokenCount}
	}
	for _, c := range parsed.Candidates {
		for _, p := range c.Content.Parts {
			if p.InlineData != nil && strings.HasPrefix(p.InlineData.MimeType, "image/") {
				img, err := decodeB64Image(p.InlineData.Data, p.InlineData.MimeType)
				if err != nil {
					return nil, err
				}
				out.Images = append(out.Images, img)
			} else if p.Text != "" {
				out.Text += p.Text
			}
		}
		if len(out.Images) == 0 && c.FinishReason != "" && c.FinishReason != "STOP" {
			out.Warnings = append(out.Warnings, "finish reason: "+c.FinishReason)
		}
	}
	return out, nil
}

func (g *google) imagen(ctx context.Context, req Request) (*Result, error) {
	var warnings []string
	params := map[string]any{"sampleCount": max(req.N, 1)}
	if req.AspectRatio != "" {
		params["aspectRatio"] = req.AspectRatio
	}
	if req.Seed != nil {
		params["seed"] = *req.Seed
		params["addWatermark"] = false
	}
	if len(req.Images) > 0 {
		warnings = append(warnings, "Imagen does not accept reference images here; they were ignored")
	}
	body := map[string]any{"instances": []any{map[string]any{"prompt": req.Prompt}}, "parameters": params}
	body = merge(body, req.Options)
	res, err := do(ctx, call{Provider: "Google", URL: g.base() + "/models/" + req.Model + ":predict",
		Headers: g.headers(req.Headers), JSON: body, MaxRetries: g.cfg.MaxRetries, Client: g.cfg.HTTP})
	if err != nil {
		return nil, authCheck("google", g.cfg.Source, googleAuth(err))
	}
	var parsed struct {
		Predictions []struct {
			BytesBase64Encoded string `json:"bytesBase64Encoded"`
			MimeType           string `json:"mimeType"`
		} `json:"predictions"`
	}
	if err := res.JSONInto(&parsed); err != nil {
		return nil, err
	}
	out := &Result{Warnings: warnings}
	for _, p := range parsed.Predictions {
		img, err := decodeB64Image(p.BytesBase64Encoded, p.MimeType)
		if err != nil {
			return nil, err
		}
		out.Images = append(out.Images, img)
	}
	return out, nil
}

// googleAuth maps "API key not valid" 400s to an auth error.
func googleAuth(err error) error {
	if apiErr, ok := err.(*APIError); ok && apiErr.Status == 400 && strings.Contains(apiErr.Message, "API key not valid") {
		apiErr.Status = 401
	}
	return err
}
