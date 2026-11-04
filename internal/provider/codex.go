package provider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/lassejlv/ai-cli/internal/auth"
	"github.com/lassejlv/ai-cli/internal/media"
)

const codexDefaultBase = "https://chatgpt.com/backend-api/codex"

func init() {
	Register(&Info{
		Name:         "codex",
		Title:        "ChatGPT (Codex subscription)",
		DefaultModel: "gpt-image-2",
		Auth:         "oauth",
		Docs:         "https://developers.openai.com/codex/auth",
		New:          newCodex,
		Models: []ModelInfo{
			{ID: "gpt-image-2", Description: "native Codex image endpoint"},
			{ID: "gpt-5.5", Description: "Responses API with the image_generation tool"},
			{ID: "gpt-5.4", Description: "Responses API with the image_generation tool"},
		},
	})
}

type codex struct {
	cfg  Config
	mu   sync.Mutex
	cred auth.Credential
}

func newCodex(cfg Config) (Provider, error) {
	if cfg.OAuth == nil || cfg.OAuth.AccessToken == "" {
		return nil, NewAuthError("codex", "")
	}
	return &codex{cfg: cfg, cred: *cfg.OAuth}, nil
}

func codexBase(cfg Config) string {
	if cfg.BaseURL != "" {
		return strings.TrimRight(cfg.BaseURL, "/")
	}
	if v := os.Getenv("AI_CLI_CODEX_BASE_URL"); v != "" {
		return strings.TrimRight(v, "/")
	}
	return codexDefaultBase
}

func (c *codex) MaxImagesPerCall(string) int { return 1 }

// credential returns a fresh access token, refreshing it when it expires soon.
func (c *codex) credential(ctx context.Context, force bool) (auth.Credential, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !force && !auth.TokenExpiring(c.cred.AccessToken, 2*time.Minute) {
		return c.cred, nil
	}
	refreshed, err := auth.RefreshCodex(ctx, c.cred)
	if err != nil {
		return c.cred, err
	}
	c.cred = refreshed
	if c.cfg.SaveOAuth != nil {
		if err := c.cfg.SaveOAuth(refreshed); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: could not save refreshed Codex tokens: %v\n", err)
		}
	}
	return refreshed, nil
}

func (c *codex) headers(cred auth.Credential, extra map[string]string) map[string]string {
	h := map[string]string{
		"Authorization": "Bearer " + cred.AccessToken,
		"originator":    auth.CodexOriginator,
		"User-Agent":    fmt.Sprintf("%s/0.130.0 (%s; %s) ai-cli", auth.CodexOriginator, runtime.GOOS, runtime.GOARCH),
		"version":       "0.130.0",
	}
	if id := cred.AccountID; id != "" {
		h["ChatGPT-Account-ID"] = id
	}
	return withHeaders(withHeaders(h, c.cfg.Headers), extra)
}

// send performs a request, refreshing the token once on 401.
func (c *codex) send(ctx context.Context, build func(auth.Credential) call) (*response, error) {
	cred, err := c.credential(ctx, false)
	if err != nil {
		return nil, err
	}
	res, err := do(ctx, build(cred))
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.Status == http.StatusUnauthorized {
		if cred, err = c.credential(ctx, true); err != nil {
			return nil, err
		}
		res, err = do(ctx, build(cred))
	}
	if err != nil {
		return nil, authCheck("codex", c.cfg.Source, err)
	}
	return res, nil
}

func (c *codex) Generate(ctx context.Context, req Request) (*Result, error) {
	if req.Prompt == "" {
		return nil, errors.New("Codex image generation requires a prompt")
	}
	if strings.HasPrefix(req.Model, "gpt-image") {
		res, err := c.native(ctx, req)
		var apiErr *APIError
		if errors.As(err, &apiErr) && (apiErr.Status == http.StatusNotFound || apiErr.Status == http.StatusMethodNotAllowed) {
			// Older backends only expose image generation through Responses.
			fallback := req
			fallback.Model = "gpt-5.5"
			res, err = c.responses(ctx, fallback)
			if res != nil {
				res.Warnings = append(res.Warnings, "native image endpoint unavailable; used the Responses image_generation tool")
			}
		}
		return res, err
	}
	return c.responses(ctx, req)
}

func (c *codex) toolSettings(req Request) (map[string]any, []string) {
	var warnings []string
	settings := map[string]any{}
	size := req.Size
	if size == "" && req.AspectRatio != "" {
		size = aspectToOpenAISize(req.AspectRatio)
		warnings = append(warnings, fmt.Sprintf("aspect ratio %s mapped to size %s", req.AspectRatio, size))
	}
	if size != "" {
		settings["size"] = size
	}
	if req.Seed != nil {
		warnings = append(warnings, "seed is not supported by Codex image generation")
	}
	if req.Mask != nil {
		warnings = append(warnings, "mask is not supported by Codex image generation; it was ignored")
	}
	return settings, warnings
}

func (c *codex) native(ctx context.Context, req Request) (*Result, error) {
	settings, warnings := c.toolSettings(req)
	body := map[string]any{"model": req.Model, "prompt": req.Prompt}
	for k, v := range settings {
		body[k] = v
	}
	endpoint := "/images/generations"
	if len(req.Images) > 0 {
		endpoint = "/images/edits"
		var images []any
		for _, img := range req.Images {
			u, err := media.DataURL(ctx, c.cfg.HTTP, img)
			if err != nil {
				return nil, err
			}
			images = append(images, map[string]any{"image_url": u})
		}
		body["images"] = images
	}
	body = merge(body, req.Options)
	res, err := c.send(ctx, func(cred auth.Credential) call {
		return call{
			Provider: "Codex", URL: codexBase(c.cfg) + endpoint,
			Headers: c.headers(cred, withHeaders(req.Headers, map[string]string{"Accept": "application/json"})),
			JSON:    body, MaxRetries: c.cfg.MaxRetries, Client: c.cfg.HTTP,
		}
	})
	if err != nil {
		return nil, err
	}
	o := &openAICompat{cfg: c.cfg}
	out, err := o.parse(ctx, res)
	if err != nil {
		return nil, err
	}
	out.Warnings = append(warnings, out.Warnings...)
	return out, nil
}

func (c *codex) responses(ctx context.Context, req Request) (*Result, error) {
	tool, warnings := c.toolSettings(req)
	tool["type"] = "image_generation"
	tool["output_format"] = "png"
	// Options for the image tool live at the top level of the "codex" entry;
	// Responses request fields can be overridden under "request".
	requestOverrides, _ := req.Options["request"].(map[string]any)
	for k, v := range req.Options {
		if k != "request" {
			tool[k] = v
		}
	}
	content := []any{map[string]any{"type": "input_text", "text": req.Prompt}}
	for _, img := range req.Images {
		u, err := media.DataURL(ctx, c.cfg.HTTP, img)
		if err != nil {
			return nil, err
		}
		content = append(content, map[string]any{"type": "input_image", "image_url": u})
	}
	instructions := "Use the available image generation tool to generate exactly one image for the user request. Do not use any other tool."
	if len(req.Images) > 0 {
		instructions += " Treat the provided input images as edit/reference images for the request."
	}
	body := map[string]any{
		"model":               req.Model,
		"instructions":        instructions,
		"input":               []any{map[string]any{"type": "message", "role": "user", "content": content}},
		"tools":               []any{tool},
		"tool_choice":         map[string]any{"type": "image_generation"},
		"parallel_tool_calls": false,
		"store":               false,
		"stream":              true,
		"include":             []any{},
	}
	body = merge(body, requestOverrides)
	res, err := c.send(ctx, func(cred auth.Credential) call {
		return call{
			Provider: "Codex", URL: codexBase(c.cfg) + "/responses",
			Headers: c.headers(cred, withHeaders(req.Headers, map[string]string{"Accept": "text/event-stream"})),
			JSON:    body, MaxRetries: c.cfg.MaxRetries, Client: c.cfg.HTTP,
		}
	})
	if err != nil {
		return nil, err
	}
	out, err := parseResponsesStream(res.Body)
	if err != nil {
		return nil, err
	}
	out.Warnings = append(warnings, out.Warnings...)
	if out.ID == "" {
		out.ID = headerID(res.Headers)
	}
	return out, nil
}

// parseResponsesStream extracts image_generation_call results from an SSE body.
func parseResponsesStream(body []byte) (*Result, error) {
	out := &Result{}
	seen := map[string]bool{}
	var lastStatus string
	handle := func(data string) error {
		if data == "" || data == "[DONE]" {
			return nil
		}
		var event map[string]any
		if json.Unmarshal([]byte(data), &event) != nil {
			return nil
		}
		typ, _ := event["type"].(string)
		switch typ {
		case "response.failed", "response.incomplete", "error":
			return fmt.Errorf("Codex image generation failed: %s", responsesError(event))
		}
		items := []any{event["item"]}
		if resp, ok := event["response"].(map[string]any); ok {
			if id, ok := resp["id"].(string); ok && out.ID == "" {
				out.ID = id
			}
			if typ == "response.completed" {
				if output, ok := resp["output"].([]any); ok {
					items = append(items, output...)
				}
				if usage, ok := resp["usage"].(map[string]any); ok {
					out.Usage = &Usage{InputTokens: intPtr(usage["input_tokens"]), OutputTokens: intPtr(usage["output_tokens"]), TotalTokens: intPtr(usage["total_tokens"])}
				}
			}
		}
		for _, raw := range items {
			item, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			switch item["type"] {
			case "image_generation_call":
				status, _ := item["status"].(string)
				lastStatus = status
				result, _ := item["result"].(string)
				id, _ := item["id"].(string)
				if result == "" || seen[id+result[:min(len(result), 64)]] {
					continue
				}
				seen[id+result[:min(len(result), 64)]] = true
				img, err := decodeB64Image(result, "image/png")
				if err != nil {
					return err
				}
				out.Images = append(out.Images, img)
				if rp, ok := item["revised_prompt"].(string); ok && out.RevisedPrompt == "" {
					out.RevisedPrompt = rp
				}
			case "message":
				if parts, ok := item["content"].([]any); ok {
					for _, p := range parts {
						if part, ok := p.(map[string]any); ok {
							if text, ok := part["text"].(string); ok && typ == "response.output_item.done" {
								out.Text += text
							}
						}
					}
				}
			}
		}
		return nil
	}

	scanner := bufio.NewScanner(bytes.NewReader(body))
	scanner.Buffer(make([]byte, 1<<20), len(body)+1)
	var data []string
	for scanner.Scan() {
		line := strings.TrimRight(scanner.Text(), "\r")
		if line == "" {
			if err := handle(strings.Join(data, "\n")); err != nil {
				return nil, err
			}
			data = data[:0]
			continue
		}
		if strings.HasPrefix(line, "data:") {
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	if err := handle(strings.Join(data, "\n")); err != nil {
		return nil, err
	}
	if len(out.Images) == 0 {
		if lastStatus != "" {
			return nil, fmt.Errorf("Codex stream ended without an image (last status %q)", lastStatus)
		}
		if out.Text != "" {
			return nil, fmt.Errorf("Codex returned no image: %s", strings.TrimSpace(out.Text))
		}
		return nil, errors.New("Codex stream ended without an image generation result")
	}
	return out, nil
}

func responsesError(event map[string]any) string {
	if resp, ok := event["response"].(map[string]any); ok {
		if e, ok := resp["error"].(map[string]any); ok {
			if msg, ok := e["message"].(string); ok {
				return msg
			}
		}
		if d, ok := resp["incomplete_details"].(map[string]any); ok {
			if r, ok := d["reason"].(string); ok {
				return "incomplete: " + r
			}
		}
	}
	if msg, ok := event["message"].(string); ok {
		return msg
	}
	if e, ok := event["error"].(map[string]any); ok {
		if msg, ok := e["message"].(string); ok {
			return msg
		}
	}
	b, _ := json.Marshal(event)
	if len(b) > 300 {
		b = append(b[:300], "…"...)
	}
	return string(b)
}

func intPtr(v any) *int {
	if f, ok := v.(float64); ok {
		i := int(f)
		return &i
	}
	return nil
}
