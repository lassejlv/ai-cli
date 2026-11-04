package provider

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/lassejlv/ai-cli/internal/media"
)

func init() {
	Register(&Info{
		Name:         "bfl",
		Title:        "Black Forest Labs",
		DefaultModel: "flux-2-pro",
		Auth:         "apikey",
		EnvVars:      []string{"BFL_API_KEY"},
		KeysURL:      "https://dashboard.bfl.ai/keys",
		Docs:         "https://docs.bfl.ai",
		New:          newBFL,
		Models: []ModelInfo{
			{ID: "flux-2-pro"},
			{ID: "flux-2-flex"},
			{ID: "flux-kontext-pro", Description: "edits with -i"},
			{ID: "flux-kontext-max", Description: "edits with -i"},
			{ID: "flux-pro-1.1-ultra"},
			{ID: "flux-pro-1.1"},
			{ID: "flux-dev"},
		},
	})
}

type bfl struct{ cfg Config }

func newBFL(cfg Config) (Provider, error) {
	if cfg.APIKey == "" {
		return nil, NewAuthError("bfl", "")
	}
	return &bfl{cfg: cfg}, nil
}

func (b *bfl) base() string {
	if b.cfg.BaseURL != "" {
		return strings.TrimRight(b.cfg.BaseURL, "/")
	}
	return "https://api.bfl.ai/v1"
}

func (b *bfl) headers(extra map[string]string) map[string]string {
	return withHeaders(withHeaders(map[string]string{"x-key": b.cfg.APIKey, "accept": "application/json"}, b.cfg.Headers), extra)
}

func (b *bfl) MaxImagesPerCall(string) int { return 1 }

func (b *bfl) Generate(ctx context.Context, req Request) (*Result, error) {
	body := map[string]any{"prompt": req.Prompt}
	if req.AspectRatio != "" {
		body["aspect_ratio"] = req.AspectRatio
	}
	if w, h, ok := parseSize(req.Size); ok {
		body["width"], body["height"] = w, h
	}
	if req.Seed != nil {
		body["seed"] = *req.Seed
	}
	for i, ref := range req.Images {
		img, err := media.Bytes(ctx, b.cfg.HTTP, ref)
		if err != nil {
			return nil, err
		}
		key := "input_image"
		if i > 0 {
			key = fmt.Sprintf("input_image_%d", i+1)
		}
		body[key] = base64.StdEncoding.EncodeToString(img.Data)
	}
	if req.Mask != nil {
		mask, err := media.Bytes(ctx, b.cfg.HTTP, *req.Mask)
		if err != nil {
			return nil, err
		}
		body["mask"] = base64.StdEncoding.EncodeToString(mask.Data)
	}
	body = merge(body, req.Options)
	res, err := do(ctx, call{Provider: "BFL", URL: b.base() + "/" + req.Model, Headers: b.headers(req.Headers),
		JSON: body, MaxRetries: b.cfg.MaxRetries, Client: b.cfg.HTTP})
	if err != nil {
		return nil, authCheck("bfl", b.cfg.Source, err)
	}
	var job struct {
		ID         string `json:"id"`
		PollingURL string `json:"polling_url"`
	}
	if err := res.JSONInto(&job); err != nil {
		return nil, err
	}
	if job.PollingURL == "" {
		if job.ID == "" {
			return nil, errors.New("BFL returned no task id")
		}
		job.PollingURL = b.base() + "/get_result?id=" + job.ID
	}
	var sample string
	err = poll(ctx, 1500*time.Millisecond, func() (bool, error) {
		res, err := do(ctx, call{Provider: "BFL", Method: http.MethodGet, URL: job.PollingURL, Headers: b.headers(nil), MaxRetries: 3, Client: b.cfg.HTTP})
		if err != nil {
			return false, err
		}
		var st struct {
			Status string         `json:"status"`
			Result map[string]any `json:"result"`
		}
		if err := res.JSONInto(&st); err != nil {
			return false, err
		}
		switch st.Status {
		case "Ready":
			sample, _ = st.Result["sample"].(string)
			return true, nil
		case "Pending", "Queued", "Processing", "":
			return false, nil
		default:
			return false, fmt.Errorf("BFL generation failed: %s", st.Status)
		}
	})
	if err != nil {
		return nil, err
	}
	if sample == "" {
		return nil, errors.New("BFL returned no image")
	}
	images, err := downloadImages(ctx, b.cfg.HTTP, []string{sample}, nil)
	if err != nil {
		return nil, err
	}
	return &Result{ID: job.ID, Images: images}, nil
}
