package provider

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	gatewayDefaultBase = "https://ai-gateway.vercel.sh/v4/ai"
	gatewayOrigin      = "https://ai-gateway.vercel.sh"
)

func init() {
	Register(&Info{
		Name:         "gateway",
		Title:        "Vercel AI Gateway",
		DefaultModel: "openai/gpt-image-2",
		Auth:         "apikey",
		EnvVars:      []string{"AI_GATEWAY_API_KEY"},
		KeysURL:      "https://vercel.com/d?to=%2F%5Bteam%5D%2F%7E%2Fai%2Fapi-keys",
		Docs:         "https://vercel.com/docs/ai-gateway",
		New:          newGateway,
		Verify:       verifyGateway,
		Models: []ModelInfo{
			{ID: "openai/gpt-image-2"},
			{ID: "openai/gpt-image-1"},
			{ID: "bfl/flux-2-pro"},
			{ID: "xai/grok-imagine-image"},
			{ID: "google/gemini-3-pro-image", Description: "language image model (chat completions)"},
		},
	})
}

type gateway struct{ cfg Config }

func newGateway(cfg Config) (Provider, error) {
	if cfg.APIKey == "" {
		return nil, NewAuthError("gateway", "")
	}
	return &gateway{cfg: cfg}, nil
}

func gatewayBase(cfg Config) string {
	if cfg.BaseURL != "" {
		return strings.TrimRight(cfg.BaseURL, "/")
	}
	if v := os.Getenv("AI_GATEWAY_BASE_URL"); v != "" {
		return strings.TrimRight(v, "/")
	}
	return gatewayDefaultBase
}

func gatewayOriginURL(cfg Config) string {
	u, err := url.Parse(gatewayBase(cfg))
	if err != nil || u.Host == "" {
		return gatewayOrigin
	}
	return u.Scheme + "://" + u.Host
}

func (g *gateway) headers(extra map[string]string) map[string]string {
	h := map[string]string{
		"Authorization":               "Bearer " + g.cfg.APIKey,
		"ai-gateway-protocol-version": "0.0.1",
		"ai-gateway-auth-method":      "api-key",
		"http-referer":                "https://github.com/lassejlv/ai-cli",
		"x-title":                     "ai-cli",
	}
	if g.cfg.Scope != "" {
		h["x-vercel-ai-gateway-team"] = g.cfg.Scope
	}
	return withHeaders(withHeaders(h, g.cfg.Headers), extra)
}

func (g *gateway) MaxImagesPerCall(model string) int {
	if IsGatewayLanguageImageModel(model) {
		return 1
	}
	return 1 << 30
}

func (g *gateway) Generate(ctx context.Context, req Request) (*Result, error) {
	if IsGatewayLanguageImageModel(req.Model) {
		return g.generateChat(ctx, req)
	}
	body := map[string]any{"prompt": req.Prompt, "n": req.N}
	if req.Prompt == "" {
		body["prompt"] = nil
	}
	if req.Size != "" {
		body["size"] = req.Size
	}
	if req.AspectRatio != "" {
		body["aspectRatio"] = req.AspectRatio
	}
	if req.Seed != nil {
		body["seed"] = *req.Seed
	}
	if len(req.GatewayOptions) > 0 {
		body["providerOptions"] = req.GatewayOptions
	}
	if len(req.Images) > 0 {
		files := make([]any, 0, len(req.Images))
		for _, img := range req.Images {
			files = append(files, gatewayFile(img.Data, img.MediaType, img.URL))
		}
		body["files"] = files
	}
	if req.Mask != nil {
		body["mask"] = gatewayFile(req.Mask.Data, req.Mask.MediaType, req.Mask.URL)
	}
	res, err := do(ctx, call{
		Provider:   "AI Gateway",
		URL:        gatewayBase(g.cfg) + "/image-model",
		Headers:    g.headers(withHeaders(req.Headers, map[string]string{"ai-image-model-specification-version": "4", "ai-model-id": req.Model})),
		JSON:       body,
		MaxRetries: g.cfg.MaxRetries,
		Client:     g.cfg.HTTP,
	})
	if err != nil {
		return nil, authCheck("gateway", g.cfg.Source, err)
	}
	var parsed struct {
		Images           []string         `json:"images"`
		Warnings         []map[string]any `json:"warnings"`
		ProviderMetadata any              `json:"providerMetadata"`
		Usage            *Usage           `json:"usage"`
	}
	if err := res.JSONInto(&parsed); err != nil {
		return nil, err
	}
	out := &Result{ID: headerID(res.Headers), ProviderMetadata: parsed.ProviderMetadata, Usage: parsed.Usage}
	for _, w := range parsed.Warnings {
		out.Warnings = append(out.Warnings, warningText(w))
	}
	for _, b64 := range parsed.Images {
		img, err := decodeB64Image(b64, "image/png")
		if err != nil {
			return nil, err
		}
		if strings.HasPrefix(req.Model, "quiverai/") {
			img.MediaType = "image/svg+xml"
		}
		out.Images = append(out.Images, img)
	}
	return out, nil
}

func (g *gateway) generateChat(ctx context.Context, req Request) (*Result, error) {
	res, err := chatImageGenerate(ctx, chatImageConfig{
		Provider:   "AI Gateway",
		Name:       "gateway",
		Source:     g.cfg.Source,
		URL:        gatewayOriginURL(g.cfg) + "/v1/chat/completions",
		Headers:    g.headers(req.Headers),
		MaxRetries: g.cfg.MaxRetries,
		Client:     g.cfg.HTTP,
	}, req)
	// Quiver Arrow models answer with SVG markup in the text.
	if err == nil && len(res.Images) == 0 && strings.HasPrefix(req.Model, "quiverai/") {
		if svg := ExtractSVG(res.Text); svg != "" {
			res.Images = append(res.Images, Image{Data: []byte(svg), MediaType: "image/svg+xml"})
			res.Text = ""
		}
	}
	return res, err
}

var svgOpen = regexp.MustCompile(`(?i)<svg[\s/>]`)

// ExtractSVG returns the longest complete <svg>…</svg> element in text.
func ExtractSVG(text string) string {
	best := ""
	for _, loc := range svgOpen.FindAllStringIndex(text, -1) {
		if svg := svgAt(text, loc[0]); len(svg) > len(best) {
			best = svg
		}
	}
	return best
}

func svgAt(text string, start int) string {
	depth := 0
	lower := strings.ToLower(text)
	for i := start; i < len(text); {
		lt := strings.IndexByte(text[i:], '<')
		if lt < 0 {
			return ""
		}
		i += lt
		if strings.HasPrefix(text[i:], "<!--") {
			end := strings.Index(text[i+4:], "-->")
			if end < 0 {
				return ""
			}
			i += 4 + end + 3
			continue
		}
		end := markupEnd(text, i+1)
		if end < 0 {
			return ""
		}
		tag := lower[i : end+1]
		switch {
		case svgOpen.MatchString(tag) && strings.HasPrefix(tag, "<svg"):
			if strings.HasSuffix(strings.TrimSpace(strings.TrimSuffix(tag, ">")), "/") {
				if depth == 0 {
					return text[start : end+1]
				}
			} else {
				depth++
			}
		case strings.HasPrefix(tag, "</svg"):
			depth--
			if depth == 0 {
				return text[start : end+1]
			}
		}
		i = end + 1
	}
	return ""
}

func markupEnd(text string, from int) int {
	var quote byte
	for i := from; i < len(text); i++ {
		c := text[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '"' || c == '\'':
			quote = c
		case c == '>':
			return i
		}
	}
	return -1
}

func gatewayFile(data []byte, mediaType, u string) map[string]any {
	if len(data) == 0 && u != "" {
		return map[string]any{"type": "url", "url": u}
	}
	return map[string]any{"type": "file", "data": base64.StdEncoding.EncodeToString(data), "mediaType": mediaType}
}

func warningText(w map[string]any) string {
	parts := []string{}
	for _, key := range []string{"type", "feature", "setting", "message", "details"} {
		if v, ok := w[key].(string); ok && v != "" {
			parts = append(parts, v)
		}
	}
	return strings.Join(parts, ": ")
}

func verifyGateway(ctx context.Context, cfg Config) error {
	_, err := do(ctx, call{
		Provider: "AI Gateway",
		Method:   http.MethodGet,
		URL:      gatewayOriginURL(cfg) + "/v1/credits",
		Headers:  map[string]string{"Authorization": "Bearer " + cfg.APIKey},
		Client:   cfg.HTTP,
	})
	return err
}

// GatewayModel is one entry in the public AI Gateway catalog.
type GatewayModel struct {
	ID          string         `json:"id"`
	Name        string         `json:"name,omitempty"`
	Description string         `json:"description,omitempty"`
	OwnedBy     string         `json:"owned_by,omitempty"`
	Type        string         `json:"type"`
	Tags        []string       `json:"tags,omitempty"`
	Pricing     map[string]any `json:"pricing,omitempty"`
	Released    int64          `json:"released,omitempty"`
}

var (
	catalogMu   sync.Mutex
	catalog     []GatewayModel
	catalogOK   bool
	languageIDs = map[string]bool{}
)

// GatewayImageModels fetches image-capable models from the AI Gateway catalog.
// Only a successful fetch is cached, so a long-running MCP server recovers
// from a transient network error.
func GatewayImageModels(ctx context.Context) ([]GatewayModel, error) {
	catalogMu.Lock()
	defer catalogMu.Unlock()
	if catalogOK {
		return catalog, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	base := gatewayOrigin
	if v := os.Getenv("AI_GATEWAY_BASE_URL"); v != "" {
		base = gatewayOriginURL(Config{BaseURL: v})
	}
	res, err := do(ctx, call{Provider: "AI Gateway", Method: http.MethodGet, URL: base + "/v1/models"})
	if err != nil {
		return nil, err
	}
	var parsed struct {
		Data []GatewayModel `json:"data"`
	}
	if err := json.Unmarshal(res.Body, &parsed); err != nil {
		return nil, fmt.Errorf("invalid AI Gateway model catalog: %w", err)
	}
	var models []GatewayModel
	ids := map[string]bool{}
	for _, m := range parsed.Data {
		isImageGen := false
		for _, t := range m.Tags {
			if t == "image-generation" {
				isImageGen = true
			}
		}
		if m.Type == "image" || (m.Type == "language" && isImageGen) {
			models = append(models, m)
			if m.Type == "language" {
				ids[m.ID] = true
			}
		}
	}
	catalog, languageIDs, catalogOK = models, ids, true
	return catalog, nil
}

// IsGatewayLanguageImageModel reports whether a gateway model generates images
// through chat completions rather than the image-model endpoint.
func IsGatewayLanguageImageModel(model string) bool {
	catalogMu.Lock()
	known, ok := languageIDs[model], catalogOK
	catalogMu.Unlock()
	if known {
		return true
	}
	if ok {
		return false
	}
	m := strings.ToLower(model)
	return strings.HasPrefix(m, "google/gemini") && strings.Contains(m, "image")
}
