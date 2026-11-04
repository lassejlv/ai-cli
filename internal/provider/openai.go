package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"os"
	"strconv"
	"strings"

	"github.com/lassejlv/ai-cli/internal/media"
)

func init() {
	Register(&Info{
		Name:         "openai",
		Title:        "OpenAI",
		DefaultModel: "gpt-image-2",
		Auth:         "apikey",
		EnvVars:      []string{"OPENAI_API_KEY"},
		KeysURL:      "https://platform.openai.com/api-keys",
		Docs:         "https://platform.openai.com/docs/guides/image-generation",
		New:          newOpenAI,
		Verify:       verifyOpenAI,
		Models: []ModelInfo{
			{ID: "gpt-image-2"},
			{ID: "gpt-image-1.5"},
			{ID: "gpt-image-1"},
			{ID: "gpt-image-1-mini"},
			{ID: "dall-e-3"},
			{ID: "dall-e-2"},
		},
	})
}

// openAICompat implements the OpenAI Images API, which several providers mirror.
type openAICompat struct {
	cfg      Config
	name     string
	title    string
	base     string
	extraHdr map[string]string
	// aspect sends aspect_ratio natively instead of mapping it to a size.
	aspect bool
	// jsonEdits sends edits as JSON with image URLs (xAI) instead of multipart.
	jsonEdits  bool
	maxPerCall func(model string) int
}

func openAIBase(cfg Config) string {
	if cfg.BaseURL != "" {
		return strings.TrimRight(cfg.BaseURL, "/")
	}
	if v := os.Getenv("OPENAI_BASE_URL"); v != "" {
		return strings.TrimRight(v, "/")
	}
	return "https://api.openai.com/v1"
}

func newOpenAI(cfg Config) (Provider, error) {
	if cfg.APIKey == "" && cfg.BaseURL == "" && os.Getenv("OPENAI_BASE_URL") == "" {
		return nil, NewAuthError("openai", "")
	}
	extra := map[string]string{}
	if org := os.Getenv("OPENAI_ORGANIZATION"); org != "" {
		extra["OpenAI-Organization"] = org
	}
	if project := os.Getenv("OPENAI_PROJECT"); project != "" {
		extra["OpenAI-Project"] = project
	}
	return &openAICompat{
		cfg: cfg, name: "openai", title: "OpenAI", base: openAIBase(cfg), extraHdr: extra,
		maxPerCall: func(model string) int {
			if strings.HasPrefix(model, "dall-e-3") {
				return 1
			}
			return 10
		},
	}, nil
}

func verifyOpenAI(ctx context.Context, cfg Config) error {
	_, err := do(ctx, call{
		Provider: "OpenAI",
		Method:   http.MethodGet,
		URL:      openAIBase(cfg) + "/models",
		Headers:  map[string]string{"Authorization": "Bearer " + cfg.APIKey},
		Client:   cfg.HTTP,
	})
	return err
}

func (o *openAICompat) MaxImagesPerCall(model string) int { return o.maxPerCall(model) }

func (o *openAICompat) headers(extra map[string]string) map[string]string {
	h := map[string]string{}
	if o.cfg.APIKey != "" {
		h["Authorization"] = "Bearer " + o.cfg.APIKey
	}
	return withHeaders(withHeaders(withHeaders(h, o.extraHdr), o.cfg.Headers), extra)
}

func (o *openAICompat) Generate(ctx context.Context, req Request) (*Result, error) {
	var warnings []string
	size := req.Size
	if req.AspectRatio != "" && !o.aspect {
		if size == "" && strings.HasPrefix(req.Model, "gpt-image") {
			size = aspectToOpenAISize(req.AspectRatio)
			warnings = append(warnings, fmt.Sprintf("aspect ratio %s mapped to size %s", req.AspectRatio, size))
		} else {
			warnings = append(warnings, "aspect ratio is not supported by "+o.title+"; use --size")
		}
	}
	if req.Seed != nil {
		warnings = append(warnings, "seed is not supported by "+o.title)
	}
	if req.Prompt == "" {
		return nil, fmt.Errorf("%s requires a prompt", o.title)
	}

	var (
		res *response
		err error
	)
	if len(req.Images) > 0 && !o.jsonEdits {
		res, err = o.edit(ctx, req, size)
	} else {
		body := map[string]any{"model": req.Model, "prompt": req.Prompt}
		if req.N > 1 {
			body["n"] = req.N
		}
		if size != "" {
			body["size"] = size
		}
		if o.aspect && req.AspectRatio != "" {
			body["aspect_ratio"] = req.AspectRatio
		}
		if !strings.HasPrefix(req.Model, "gpt-image") {
			body["response_format"] = "b64_json"
		}
		endpoint := "/images/generations"
		if len(req.Images) > 0 {
			endpoint = "/images/edits"
			refs := make([]any, 0, len(req.Images))
			for _, img := range req.Images {
				u := img.URL
				if len(img.Data) > 0 {
					u, err = media.DataURL(ctx, o.cfg.HTTP, img)
					if err != nil {
						return nil, err
					}
				}
				refs = append(refs, map[string]any{"url": u, "type": "image_url"})
			}
			if len(refs) == 1 {
				body["image"] = refs[0]
			} else {
				body["images"] = refs
			}
		}
		body = merge(body, req.Options)
		res, err = do(ctx, call{
			Provider: o.title, URL: o.base + endpoint, Headers: o.headers(req.Headers),
			JSON: body, MaxRetries: o.cfg.MaxRetries, Client: o.cfg.HTTP,
		})
	}
	if err != nil {
		return nil, authCheck(o.name, o.cfg.Source, err)
	}
	result, err := o.parse(ctx, res)
	if err != nil {
		return nil, err
	}
	result.Warnings = append(warnings, result.Warnings...)
	return result, nil
}

func (o *openAICompat) edit(ctx context.Context, req Request, size string) (*response, error) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	fields := map[string]any{"model": req.Model, "prompt": req.Prompt}
	if req.N > 1 {
		fields["n"] = req.N
	}
	if size != "" {
		fields["size"] = size
	}
	if !strings.HasPrefix(req.Model, "gpt-image") {
		fields["response_format"] = "b64_json"
	}
	for k, v := range req.Options {
		fields[k] = v
	}
	for k, v := range fields {
		switch t := v.(type) {
		case string:
			_ = w.WriteField(k, t)
		case float64:
			_ = w.WriteField(k, strconv.FormatFloat(t, 'f', -1, 64))
		case int:
			_ = w.WriteField(k, strconv.Itoa(t))
		case bool:
			_ = w.WriteField(k, strconv.FormatBool(t))
		default:
			b, _ := json.Marshal(t)
			_ = w.WriteField(k, string(b))
		}
	}
	field := "image[]"
	if strings.HasPrefix(req.Model, "dall-e") {
		field = "image"
	}
	for i, ref := range req.Images {
		img, err := media.Bytes(ctx, o.cfg.HTTP, ref)
		if err != nil {
			return nil, err
		}
		if err := writeFilePart(w, field, fmt.Sprintf("image-%d%s", i+1, media.Extension(img.MediaType)), img.MediaType, img.Data); err != nil {
			return nil, err
		}
	}
	if req.Mask != nil {
		mask, err := media.Bytes(ctx, o.cfg.HTTP, *req.Mask)
		if err != nil {
			return nil, err
		}
		if err := writeFilePart(w, "mask", "mask"+media.Extension(mask.MediaType), mask.MediaType, mask.Data); err != nil {
			return nil, err
		}
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return do(ctx, call{
		Provider: o.title, URL: o.base + "/images/edits", Headers: o.headers(req.Headers),
		Body: buf.Bytes(), ContentType: w.FormDataContentType(), MaxRetries: o.cfg.MaxRetries, Client: o.cfg.HTTP,
	})
}

func writeFilePart(w *multipart.Writer, field, filename, mediaType string, data []byte) error {
	h := make(textproto.MIMEHeader)
	h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="%s"; filename="%s"`, field, filename))
	h.Set("Content-Type", mediaType)
	part, err := w.CreatePart(h)
	if err != nil {
		return err
	}
	_, err = part.Write(data)
	return err
}

func (o *openAICompat) parse(ctx context.Context, res *response) (*Result, error) {
	var parsed struct {
		Data []struct {
			B64JSON       string `json:"b64_json"`
			URL           string `json:"url"`
			RevisedPrompt string `json:"revised_prompt"`
		} `json:"data"`
		OutputFormat string `json:"output_format"`
		Usage        *struct {
			InputTokens  *int `json:"input_tokens"`
			OutputTokens *int `json:"output_tokens"`
			TotalTokens  *int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := res.JSONInto(&parsed); err != nil {
		return nil, err
	}
	out := &Result{ID: headerID(res.Headers)}
	if parsed.Usage != nil {
		out.Usage = &Usage{InputTokens: parsed.Usage.InputTokens, OutputTokens: parsed.Usage.OutputTokens, TotalTokens: parsed.Usage.TotalTokens}
	}
	fallback := ""
	if parsed.OutputFormat != "" {
		fallback = "image/" + parsed.OutputFormat
	}
	for _, d := range parsed.Data {
		if d.RevisedPrompt != "" && out.RevisedPrompt == "" {
			out.RevisedPrompt = d.RevisedPrompt
		}
		switch {
		case d.B64JSON != "":
			img, err := decodeB64Image(d.B64JSON, fallback)
			if err != nil {
				return nil, err
			}
			out.Images = append(out.Images, img)
		case d.URL != "":
			imgs, err := downloadImages(ctx, o.cfg.HTTP, []string{d.URL}, nil)
			if err != nil {
				return nil, err
			}
			out.Images = append(out.Images, imgs...)
		}
	}
	return out, nil
}
