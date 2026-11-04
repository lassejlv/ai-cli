package provider

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/lassejlv/ai-cli/internal/media"
)

func init() {
	Register(&Info{
		Name:         "higgsfield",
		Title:        "Higgsfield",
		DefaultModel: "higgsfield-ai/soul/standard",
		Auth:         "keypair",
		EnvVars:      []string{"HF_API_KEY", "HF_API_SECRET", "HF_KEY"},
		KeysURL:      "https://cloud.higgsfield.ai/api-keys",
		Docs:         "https://docs.higgsfield.ai",
		New:          newHiggsfield,
		Aliases: map[string]string{
			"soul":            "higgsfield-ai/soul/standard",
			"soul-v2":         "higgsfield-ai/soul/v2/standard",
			"soul-cinema":     "higgsfield-ai/soul/cinema",
			"ideogram":        "ideogram/v4.0",
			"recraft":         "recraft/v4.1/text-to-image",
			"recraft-pro":     "recraft/v4.1/pro/text-to-image",
			"qwen-image":      "alibaba/qwen-image-3/text-to-image",
			"qwen-image-edit": "alibaba/qwen-image-3/edit",
			"z-image":         "z-image/turbo",
			"grok-imagine":    "xai/grok-imagine-image-2.0",
			"seedream":        "bytedance/seedream/v4/text-to-image",
			"marketing":       "marketing-studio/image",
			"ads-studio":      "higgsfield/ads-studio/v1.0",
		},
		Models: []ModelInfo{
			{ID: "higgsfield-ai/soul/standard", Description: "SOUL, styles and character references (alias: soul)"},
			{ID: "higgsfield-ai/soul/v2/standard", Description: "SOUL V2 (alias: soul-v2)"},
			{ID: "higgsfield-ai/soul/cinema", Description: "SOUL Cinema (alias: soul-cinema)"},
			{ID: "ideogram/v4.0", Description: "Ideogram 4 (alias: ideogram)"},
			{ID: "recraft/v4.1/text-to-image", Description: "Recraft 4.1 (alias: recraft)"},
			{ID: "recraft/v4.1/pro/text-to-image", Description: "Recraft 4.1 Pro (alias: recraft-pro)"},
			{ID: "recraft/v4.1/utility/text-to-image"},
			{ID: "recraft/v4.1/utility/pro/text-to-image"},
			{ID: "alibaba/qwen-image-3/text-to-image", Description: "Qwen Image 3 (alias: qwen-image)"},
			{ID: "alibaba/qwen-image-3/edit", Description: "Qwen Image 3 edit, needs -i (alias: qwen-image-edit)"},
			{ID: "z-image/turbo", Description: "Z-Image Turbo (alias: z-image)"},
			{ID: "xai/grok-imagine-image-2.0", Description: "Grok Imagine 2 (alias: grok-imagine)"},
			{ID: "bytedance/seedream/v4/text-to-image", Description: "Seedream 4 (alias: seedream)"},
			{ID: "marketing-studio/image", Description: "Marketing Studio (alias: marketing)"},
			{ID: "marketing-studio/image/flare"},
			{ID: "marketing-studio/image/sunburst"},
			{ID: "higgsfield/ads-studio/v1.0", Description: "Ads Studio (alias: ads-studio)"},
		},
	})
}

type higgsfield struct{ cfg Config }

func newHiggsfield(cfg Config) (Provider, error) {
	if cfg.APIKey == "" {
		return nil, NewAuthError("higgsfield", "")
	}
	return &higgsfield{cfg: cfg}, nil
}

func (h *higgsfield) base() string {
	if h.cfg.BaseURL != "" {
		return strings.TrimRight(h.cfg.BaseURL, "/")
	}
	return "https://api.higgsfield.ai"
}

func (h *higgsfield) authHeaders(extra map[string]string) map[string]string {
	key := h.cfg.APIKey
	if h.cfg.APISecret != "" {
		key += ":" + h.cfg.APISecret
	}
	return withHeaders(withHeaders(map[string]string{"Authorization": "Key " + key}, h.cfg.Headers), extra)
}

func (h *higgsfield) MaxImagesPerCall(string) int { return 1 }

// referenceField returns the parameter that accepts reference image URLs.
func higgsfieldReferenceField(model string) (string, bool) {
	switch {
	case strings.HasPrefix(model, "ideogram/"):
		return "image_url", false
	case model == "higgsfield-ai/soul/standard":
		return "image_reference_url", false
	case strings.HasPrefix(model, "higgsfield-ai/soul/"), strings.HasPrefix(model, "recraft/"), model == "z-image/turbo", model == "alibaba/qwen-image-3/text-to-image":
		return "", false
	}
	return "image_urls", true
}

func (h *higgsfield) Generate(ctx context.Context, req Request) (*Result, error) {
	var warnings []string
	body := map[string]any{"prompt": req.Prompt}
	if req.AspectRatio != "" {
		body["aspect_ratio"] = req.AspectRatio
	}
	if req.Seed != nil {
		body["seed"] = *req.Seed
	}
	if req.Size != "" {
		warnings = append(warnings, "--size is not supported by Higgsfield; use --aspect-ratio or a resolution in --provider-options")
	}
	if req.Mask != nil {
		warnings = append(warnings, "--mask is not supported by Higgsfield; it was ignored")
	}
	if len(req.Images) > 0 {
		field, multi := higgsfieldReferenceField(req.Model)
		if field == "" {
			return nil, fmt.Errorf("Higgsfield model %s does not accept reference images", req.Model)
		}
		urls := make([]string, 0, len(req.Images))
		for _, img := range req.Images {
			u, err := h.publicURL(ctx, img)
			if err != nil {
				return nil, err
			}
			urls = append(urls, u)
		}
		if multi {
			body[field] = urls
		} else {
			body[field] = urls[0]
			if len(urls) > 1 {
				warnings = append(warnings, fmt.Sprintf("%s accepts one reference image; extra images were ignored", req.Model))
			}
		}
	}
	body = merge(body, req.Options)

	res, err := do(ctx, call{
		Provider: "Higgsfield", URL: h.base() + "/" + strings.TrimLeft(req.Model, "/"),
		Headers: h.authHeaders(req.Headers), JSON: body, MaxRetries: h.cfg.MaxRetries, Client: h.cfg.HTTP,
	})
	if err != nil {
		return nil, authCheck("higgsfield", h.cfg.Source, err)
	}
	var job higgsfieldStatus
	if err := res.JSONInto(&job); err != nil {
		return nil, err
	}
	if job.RequestID == "" {
		return nil, errors.New("Higgsfield returned no request_id")
	}
	statusURL := job.StatusURL
	if statusURL == "" {
		statusURL = h.base() + "/requests/" + job.RequestID + "/status"
	}
	err = poll(ctx, 2*time.Second, func() (bool, error) {
		if job.done() {
			return true, nil
		}
		res, err := do(ctx, call{Provider: "Higgsfield", Method: http.MethodGet, URL: statusURL, Headers: h.authHeaders(nil), MaxRetries: 3, Client: h.cfg.HTTP})
		if err != nil {
			return false, authCheck("higgsfield", h.cfg.Source, err)
		}
		job = higgsfieldStatus{RequestID: job.RequestID}
		if err := res.JSONInto(&job); err != nil {
			return false, err
		}
		return job.done(), nil
	})
	if err != nil {
		return nil, err
	}
	switch job.Status {
	case "completed":
	case "nsfw":
		return nil, errors.New("Higgsfield rejected the request as NSFW")
	case "failed", "canceled":
		msg := job.Error
		if msg == "" {
			msg = job.Status
		}
		return nil, fmt.Errorf("Higgsfield generation %s", msg)
	}
	var urls []string
	for _, img := range job.Images {
		if img.URL != "" {
			urls = append(urls, img.URL)
		}
	}
	images, err := downloadImages(ctx, h.cfg.HTTP, urls, nil)
	if err != nil {
		return nil, err
	}
	return &Result{ID: job.RequestID, Images: images, Warnings: warnings}, nil
}

type higgsfieldStatus struct {
	Status    string `json:"status"`
	RequestID string `json:"request_id"`
	StatusURL string `json:"status_url"`
	Error     string `json:"error"`
	Images    []struct {
		URL string `json:"url"`
	} `json:"images"`
}

func (s higgsfieldStatus) done() bool {
	switch s.Status {
	case "completed", "failed", "nsfw", "canceled", "cancelled":
		return true
	}
	return false
}

// publicURL uploads local reference bytes and returns a public HTTPS URL.
func (h *higgsfield) publicURL(ctx context.Context, img media.Image) (string, error) {
	if img.IsRemote() {
		return img.URL, nil
	}
	mediaType := img.MediaType
	switch mediaType {
	case "image/jpeg", "image/png", "image/webp", "image/gif":
	default:
		return "", fmt.Errorf("Higgsfield uploads support JPEG, PNG, WebP and GIF (got %s)", mediaType)
	}
	res, err := do(ctx, call{
		Provider: "Higgsfield", URL: h.base() + "/files/generate-upload-url", Headers: h.authHeaders(nil),
		JSON: map[string]any{"content_type": mediaType}, MaxRetries: h.cfg.MaxRetries, Client: h.cfg.HTTP,
	})
	if err != nil {
		return "", authCheck("higgsfield", h.cfg.Source, err)
	}
	var upload struct {
		PublicURL     string            `json:"public_url"`
		UploadURL     string            `json:"upload_url"`
		UploadHeaders map[string]string `json:"upload_headers"`
	}
	if err := res.JSONInto(&upload); err != nil {
		return "", err
	}
	if upload.UploadURL == "" || upload.PublicURL == "" {
		return "", errors.New("Higgsfield returned no upload URL")
	}
	headers := upload.UploadHeaders
	if headers == nil {
		headers = map[string]string{}
	}
	if _, ok := headers["Content-Type"]; !ok {
		headers["Content-Type"] = mediaType
	}
	// Never send Higgsfield credentials to the presigned storage URL.
	if _, err := do(ctx, call{
		Provider: "Higgsfield upload", Method: http.MethodPut, URL: upload.UploadURL,
		Headers: headers, Body: img.Data, MaxRetries: h.cfg.MaxRetries, Client: h.cfg.HTTP,
	}); err != nil {
		return "", err
	}
	return upload.PublicURL, nil
}
