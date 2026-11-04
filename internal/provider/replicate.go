package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/lassejlv/ai-cli/internal/media"
)

func init() {
	Register(&Info{
		Name:         "replicate",
		Title:        "Replicate",
		DefaultModel: "black-forest-labs/flux-schnell",
		Auth:         "apikey",
		EnvVars:      []string{"REPLICATE_API_TOKEN"},
		KeysURL:      "https://replicate.com/account/api-tokens",
		Docs:         "https://replicate.com/docs/reference/http",
		New:          newReplicate,
		Verify: func(ctx context.Context, cfg Config) error {
			_, err := do(ctx, call{Provider: "Replicate", Method: http.MethodGet, URL: "https://api.replicate.com/v1/account",
				Headers: map[string]string{"Authorization": "Bearer " + cfg.APIKey}, Client: cfg.HTTP})
			return err
		},
		Models: []ModelInfo{
			{ID: "black-forest-labs/flux-schnell"},
			{ID: "black-forest-labs/flux-1.1-pro"},
			{ID: "black-forest-labs/flux-2-pro"},
			{ID: "black-forest-labs/flux-kontext-pro", Description: "edits with -i"},
			{ID: "google/imagen-4"},
			{ID: "google/nano-banana"},
			{ID: "bytedance/seedream-4"},
			{ID: "ideogram-ai/ideogram-v3-turbo"},
			{ID: "recraft-ai/recraft-v3"},
			{ID: "owner/model:version", Description: "any model version"},
		},
	})
}

type replicate struct{ cfg Config }

func newReplicate(cfg Config) (Provider, error) {
	if cfg.APIKey == "" {
		return nil, NewAuthError("replicate", "")
	}
	return &replicate{cfg: cfg}, nil
}

func (r *replicate) base() string {
	if r.cfg.BaseURL != "" {
		return strings.TrimRight(r.cfg.BaseURL, "/")
	}
	return "https://api.replicate.com/v1"
}

func (r *replicate) headers(extra map[string]string) map[string]string {
	return withHeaders(withHeaders(map[string]string{"Authorization": "Bearer " + r.cfg.APIKey}, r.cfg.Headers), extra)
}

func (r *replicate) MaxImagesPerCall(string) int { return 1 }

func (r *replicate) Generate(ctx context.Context, req Request) (*Result, error) {
	input := map[string]any{"prompt": req.Prompt}
	if req.AspectRatio != "" {
		input["aspect_ratio"] = req.AspectRatio
	}
	if w, h, ok := parseSize(req.Size); ok {
		input["width"], input["height"] = w, h
	}
	if req.Seed != nil {
		input["seed"] = *req.Seed
	}
	if len(req.Images) > 0 {
		urls := make([]string, 0, len(req.Images))
		for _, img := range req.Images {
			u := img.URL
			if len(img.Data) > 0 {
				var err error
				if u, err = media.DataURL(ctx, r.cfg.HTTP, img); err != nil {
					return nil, err
				}
			}
			urls = append(urls, u)
		}
		// Popular image models disagree on the parameter name.
		input["input_image"] = urls[0]
		input["image_input"] = urls
	}
	if req.Mask != nil {
		u, err := media.DataURL(ctx, r.cfg.HTTP, *req.Mask)
		if err != nil {
			return nil, err
		}
		input["mask"] = u
	}
	input = merge(input, req.Options)

	url := r.base() + "/predictions"
	body := map[string]any{"input": input}
	if owner, version, ok := strings.Cut(req.Model, ":"); ok {
		_ = owner
		body["version"] = version
	} else {
		url = r.base() + "/models/" + req.Model + "/predictions"
	}
	res, err := do(ctx, call{Provider: "Replicate", URL: url, Headers: r.headers(withHeaders(map[string]string{"Prefer": "wait=60"}, req.Headers)),
		JSON: body, MaxRetries: r.cfg.MaxRetries, Client: r.cfg.HTTP})
	if err != nil {
		return nil, authCheck("replicate", r.cfg.Source, err)
	}
	var pred replicatePrediction
	if err := res.JSONInto(&pred); err != nil {
		return nil, err
	}
	err = poll(ctx, 1500*time.Millisecond, func() (bool, error) {
		switch pred.Status {
		case "succeeded":
			return true, nil
		case "failed", "canceled", "aborted":
			msg := fmt.Sprint(pred.Error)
			if pred.Error == nil {
				msg = pred.Status
			}
			return false, fmt.Errorf("Replicate prediction %s: %s", pred.Status, msg)
		}
		if pred.URLs.Get == "" {
			return false, errors.New("Replicate returned no polling URL")
		}
		res, err := do(ctx, call{Provider: "Replicate", Method: http.MethodGet, URL: pred.URLs.Get, Headers: r.headers(nil), MaxRetries: 3, Client: r.cfg.HTTP})
		if err != nil {
			return false, err
		}
		pred = replicatePrediction{}
		if err := res.JSONInto(&pred); err != nil {
			return false, err
		}
		return false, nil
	})
	if err != nil {
		return nil, err
	}
	urls := outputURLs(pred.Output)
	images, err := downloadImages(ctx, r.cfg.HTTP, urls, nil)
	if err != nil {
		return nil, err
	}
	return &Result{ID: pred.ID, Images: images}, nil
}

type replicatePrediction struct {
	ID     string          `json:"id"`
	Status string          `json:"status"`
	Error  any             `json:"error"`
	Output json.RawMessage `json:"output"`
	URLs   struct {
		Get string `json:"get"`
	} `json:"urls"`
}

func outputURLs(raw json.RawMessage) []string {
	var one string
	if json.Unmarshal(raw, &one) == nil && one != "" {
		return []string{one}
	}
	var many []string
	if json.Unmarshal(raw, &many) == nil {
		return many
	}
	var objs []map[string]any
	if json.Unmarshal(raw, &objs) == nil {
		var out []string
		for _, o := range objs {
			if u, ok := o["url"].(string); ok {
				out = append(out, u)
			}
		}
		return out
	}
	return nil
}
