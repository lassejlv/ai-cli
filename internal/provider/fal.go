package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/lassejlv/ai-cli/internal/media"
)

func init() {
	Register(&Info{
		Name:         "fal",
		Title:        "fal",
		DefaultModel: "fal-ai/flux/dev",
		Auth:         "apikey",
		EnvVars:      []string{"FAL_KEY", "FAL_API_KEY"},
		KeysURL:      "https://fal.ai/dashboard/keys",
		Docs:         "https://docs.fal.ai/model-apis/model-endpoints/queue",
		New:          newFal,
		Aliases: map[string]string{
			"flux":         "fal-ai/flux/dev",
			"flux-schnell": "fal-ai/flux/schnell",
			"flux-pro":     "fal-ai/flux-pro/v1.1",
			"flux-2-pro":   "fal-ai/flux-2-pro",
			"flux-kontext": "fal-ai/flux-pro/kontext",
			"nano-banana":  "fal-ai/nano-banana",
			"seedream":     "fal-ai/bytedance/seedream/v4/text-to-image",
			"recraft":      "fal-ai/recraft/v3/text-to-image",
			"ideogram":     "fal-ai/ideogram/v3",
			"qwen-image":   "fal-ai/qwen-image",
			"imagen4":      "fal-ai/imagen4/preview",
		},
		Models: []ModelInfo{
			{ID: "fal-ai/flux/dev", Description: "alias: flux"},
			{ID: "fal-ai/flux/schnell", Description: "alias: flux-schnell"},
			{ID: "fal-ai/flux-pro/v1.1", Description: "alias: flux-pro"},
			{ID: "fal-ai/flux-2-pro", Description: "alias: flux-2-pro"},
			{ID: "fal-ai/flux-pro/kontext", Description: "edits with -i (alias: flux-kontext)"},
			{ID: "fal-ai/nano-banana", Description: "Gemini image (alias: nano-banana)"},
			{ID: "fal-ai/bytedance/seedream/v4/text-to-image", Description: "alias: seedream"},
			{ID: "fal-ai/recraft/v3/text-to-image", Description: "alias: recraft"},
			{ID: "fal-ai/ideogram/v3", Description: "alias: ideogram"},
			{ID: "fal-ai/qwen-image", Description: "alias: qwen-image"},
			{ID: "fal-ai/imagen4/preview", Description: "alias: imagen4"},
		},
	})
}

type fal struct{ cfg Config }

func newFal(cfg Config) (Provider, error) {
	if cfg.APIKey == "" {
		return nil, NewAuthError("fal", "")
	}
	return &fal{cfg: cfg}, nil
}

func (f *fal) base() string {
	if f.cfg.BaseURL != "" {
		return strings.TrimRight(f.cfg.BaseURL, "/")
	}
	if v := os.Getenv("FAL_QUEUE_URL"); v != "" {
		return strings.TrimRight(v, "/")
	}
	return "https://queue.fal.run"
}

func (f *fal) headers(extra map[string]string) map[string]string {
	return withHeaders(withHeaders(map[string]string{"Authorization": "Key " + f.cfg.APIKey}, f.cfg.Headers), extra)
}

func (f *fal) MaxImagesPerCall(string) int { return 4 }

func (f *fal) Generate(ctx context.Context, req Request) (*Result, error) {
	body := map[string]any{"prompt": req.Prompt}
	if req.N > 1 {
		body["num_images"] = req.N
	}
	if w, h, ok := parseSize(req.Size); ok {
		body["image_size"] = map[string]any{"width": w, "height": h}
	}
	if req.AspectRatio != "" {
		body["aspect_ratio"] = req.AspectRatio
	}
	if req.Seed != nil {
		body["seed"] = *req.Seed
	}
	if len(req.Images) > 0 {
		// fal accepts data URIs anywhere it accepts image URLs.
		urls := make([]string, 0, len(req.Images))
		for _, img := range req.Images {
			u := img.URL
			if len(img.Data) > 0 {
				var err error
				if u, err = media.DataURL(ctx, f.cfg.HTTP, img); err != nil {
					return nil, err
				}
			}
			urls = append(urls, u)
		}
		body["image_urls"] = urls
		body["image_url"] = urls[0]
	}
	if req.Mask != nil {
		u, err := media.DataURL(ctx, f.cfg.HTTP, *req.Mask)
		if err != nil {
			return nil, err
		}
		body["mask_url"] = u
	}
	body = merge(body, req.Options)

	res, err := do(ctx, call{Provider: "fal", URL: f.base() + "/" + strings.TrimLeft(req.Model, "/"),
		Headers: f.headers(req.Headers), JSON: body, MaxRetries: f.cfg.MaxRetries, Client: f.cfg.HTTP})
	if err != nil {
		return nil, authCheck("fal", f.cfg.Source, err)
	}
	var queued struct {
		RequestID   string `json:"request_id"`
		StatusURL   string `json:"status_url"`
		ResponseURL string `json:"response_url"`
	}
	if err := res.JSONInto(&queued); err != nil {
		return nil, err
	}
	if queued.RequestID == "" || queued.StatusURL == "" {
		return nil, errors.New("fal returned no request_id")
	}
	err = poll(ctx, 1500*time.Millisecond, func() (bool, error) {
		res, err := do(ctx, call{Provider: "fal", Method: http.MethodGet, URL: queued.StatusURL, Headers: f.headers(nil), MaxRetries: 3, Client: f.cfg.HTTP})
		if err != nil {
			return false, authCheck("fal", f.cfg.Source, err)
		}
		var st struct {
			Status string `json:"status"`
			Error  string `json:"error"`
		}
		if err := res.JSONInto(&st); err != nil {
			return false, err
		}
		switch st.Status {
		case "COMPLETED":
			if st.Error != "" {
				return false, fmt.Errorf("fal: %s", st.Error)
			}
			return true, nil
		case "IN_QUEUE", "IN_PROGRESS", "":
			return false, nil
		default:
			return false, fmt.Errorf("fal request %s", strings.ToLower(st.Status))
		}
	})
	if err != nil {
		return nil, err
	}
	res, err = do(ctx, call{Provider: "fal", Method: http.MethodGet, URL: queued.ResponseURL, Headers: f.headers(nil), MaxRetries: 3, Client: f.cfg.HTTP})
	if err != nil {
		return nil, err
	}
	var out struct {
		Images []falFile `json:"images"`
		Image  *falFile  `json:"image"`
		Seed   any       `json:"seed"`
		Prompt string    `json:"prompt"`
		Desc   string    `json:"description"`
	}
	if err := json.Unmarshal(res.Body, &out); err != nil {
		return nil, fmt.Errorf("invalid fal response: %w", err)
	}
	if out.Image != nil {
		out.Images = append(out.Images, *out.Image)
	}
	var urls []string
	for _, img := range out.Images {
		if img.URL != "" {
			urls = append(urls, img.URL)
		}
	}
	images, err := downloadImages(ctx, f.cfg.HTTP, urls, nil)
	if err != nil {
		return nil, err
	}
	var meta map[string]any
	if out.Seed != nil {
		meta = map[string]any{"fal": map[string]any{"seed": out.Seed}}
	}
	return &Result{ID: queued.RequestID, Images: images, Text: out.Desc, ProviderMetadata: meta}, nil
}

type falFile struct {
	URL         string `json:"url"`
	ContentType string `json:"content_type"`
}
