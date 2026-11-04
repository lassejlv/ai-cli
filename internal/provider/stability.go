package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"

	"github.com/lassejlv/ai-cli/internal/media"
)

func init() {
	Register(&Info{
		Name:         "stability",
		Title:        "Stability AI",
		DefaultModel: "core",
		Auth:         "apikey",
		EnvVars:      []string{"STABILITY_API_KEY"},
		KeysURL:      "https://platform.stability.ai/account/keys",
		Docs:         "https://platform.stability.ai/docs/api-reference",
		New:          newStability,
		Verify: func(ctx context.Context, cfg Config) error {
			_, err := do(ctx, call{Provider: "Stability AI", Method: http.MethodGet, URL: "https://api.stability.ai/v1/user/account",
				Headers: map[string]string{"Authorization": "Bearer " + cfg.APIKey}, Client: cfg.HTTP})
			return err
		},
		Models: []ModelInfo{
			{ID: "ultra", Description: "Stable Image Ultra"},
			{ID: "core", Description: "Stable Image Core"},
			{ID: "sd3.5-large"},
			{ID: "sd3.5-large-turbo"},
			{ID: "sd3.5-medium"},
			{ID: "sd3.5-flash"},
		},
	})
}

type stability struct{ cfg Config }

func newStability(cfg Config) (Provider, error) {
	if cfg.APIKey == "" {
		return nil, NewAuthError("stability", "")
	}
	return &stability{cfg: cfg}, nil
}

func (s *stability) base() string {
	if s.cfg.BaseURL != "" {
		return strings.TrimRight(s.cfg.BaseURL, "/")
	}
	return "https://api.stability.ai/v2beta"
}

func (s *stability) MaxImagesPerCall(string) int { return 1 }

func (s *stability) Generate(ctx context.Context, req Request) (*Result, error) {
	var warnings []string
	endpoint := "/stable-image/generate/" + req.Model
	fields := map[string]any{"prompt": req.Prompt, "output_format": "png"}
	if strings.HasPrefix(req.Model, "sd3") {
		endpoint = "/stable-image/generate/sd3"
		fields["model"] = req.Model
	}
	if req.AspectRatio != "" {
		fields["aspect_ratio"] = req.AspectRatio
	}
	if req.Seed != nil {
		fields["seed"] = *req.Seed
	}
	if req.Size != "" {
		warnings = append(warnings, "--size is not supported by Stability AI; use --aspect-ratio")
	}
	for k, v := range req.Options {
		fields[k] = v
	}
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	for k, v := range fields {
		switch t := v.(type) {
		case string:
			_ = w.WriteField(k, t)
		case int64:
			_ = w.WriteField(k, strconv.FormatInt(t, 10))
		case float64:
			_ = w.WriteField(k, strconv.FormatFloat(t, 'f', -1, 64))
		default:
			b, _ := json.Marshal(t)
			_ = w.WriteField(k, strings.Trim(string(b), `"`))
		}
	}
	if len(req.Images) > 0 {
		img, err := media.Bytes(ctx, s.cfg.HTTP, req.Images[0])
		if err != nil {
			return nil, err
		}
		if err := writeFilePart(w, "image", "image"+media.Extension(img.MediaType), img.MediaType, img.Data); err != nil {
			return nil, err
		}
		if _, ok := fields["strength"]; !ok {
			_ = w.WriteField("strength", "0.7")
		}
		if strings.HasPrefix(req.Model, "sd3") {
			_ = w.WriteField("mode", "image-to-image")
		}
		if len(req.Images) > 1 {
			warnings = append(warnings, "Stability AI accepts one reference image; extra images were ignored")
		}
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	res, err := do(ctx, call{
		Provider: "Stability AI", URL: s.base() + endpoint,
		Headers: withHeaders(withHeaders(map[string]string{"Authorization": "Bearer " + s.cfg.APIKey, "Accept": "image/*"}, s.cfg.Headers), req.Headers),
		Body:    buf.Bytes(), ContentType: w.FormDataContentType(), MaxRetries: s.cfg.MaxRetries, Client: s.cfg.HTTP,
	})
	if err != nil {
		return nil, authCheck("stability", s.cfg.Source, err)
	}
	if !media.IsImage(res.Body) {
		return nil, fmt.Errorf("Stability AI returned %s instead of an image", res.Headers.Get("Content-Type"))
	}
	out := &Result{ID: headerID(res.Headers), Images: []Image{{Data: res.Body, MediaType: media.Detect(res.Body)}}, Warnings: warnings}
	if seed := res.Headers.Get("seed"); seed != "" {
		out.ProviderMetadata = map[string]any{"stability": map[string]any{"seed": seed, "finishReason": res.Headers.Get("finish-reason")}}
	}
	return out, nil
}
