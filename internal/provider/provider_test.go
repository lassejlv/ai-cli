package provider

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lassejlv/ai-cli/internal/auth"
	"github.com/lassejlv/ai-cli/internal/media"
)

func testPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 4, 3))
	img.Set(1, 1, color.RGBA{255, 0, 0, 255})
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func decodeBody(t *testing.T, r *http.Request) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	return body
}

func mustNew(t *testing.T, name string, cfg Config) Provider {
	t.Helper()
	info, ok := Lookup(name)
	if !ok {
		t.Fatalf("provider %s not registered", name)
	}
	p, err := info.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestOpenAIGenerate(t *testing.T) {
	pngData := testPNG(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/images/generations" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer sk-test" {
			t.Errorf("auth = %q", got)
		}
		body := decodeBody(t, r)
		if body["model"] != "gpt-image-2" || body["prompt"] != "a cat" || body["quality"] != "high" || body["n"] != float64(2) {
			t.Errorf("unexpected body %v", body)
		}
		if body["size"] != "1536x1024" {
			t.Errorf("aspect ratio should map to 1536x1024, got %v", body["size"])
		}
		if _, ok := body["response_format"]; ok {
			t.Error("gpt-image must not send response_format")
		}
		w.Header().Set("x-request-id", "req_123")
		b64 := base64.StdEncoding.EncodeToString(pngData)
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{map[string]any{"b64_json": b64}, map[string]any{"b64_json": b64}}, "usage": map[string]any{"input_tokens": 5, "output_tokens": 10, "total_tokens": 15}})
	}))
	defer srv.Close()
	p := mustNew(t, "openai", Config{APIKey: "sk-test", BaseURL: srv.URL})
	res, err := p.Generate(context.Background(), Request{Model: "gpt-image-2", Prompt: "a cat", N: 2, AspectRatio: "16:9", Options: map[string]any{"quality": "high"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Images) != 2 || res.Images[0].MediaType != "image/png" || res.ID != "req_123" {
		t.Fatalf("unexpected result %+v", res)
	}
	if res.Usage == nil || *res.Usage.TotalTokens != 15 {
		t.Errorf("usage not parsed: %+v", res.Usage)
	}
}

func TestOpenAIEditMultipart(t *testing.T) {
	pngData := testPNG(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/images/edits" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if err := r.ParseMultipartForm(10 << 20); err != nil {
			t.Fatal(err)
		}
		if r.FormValue("prompt") != "make it blue" || len(r.MultipartForm.File["image[]"]) != 2 || len(r.MultipartForm.File["mask"]) != 1 {
			t.Errorf("unexpected form: %v files=%v", r.MultipartForm.Value, r.MultipartForm.File)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{map[string]any{"b64_json": base64.StdEncoding.EncodeToString(pngData)}}})
	}))
	defer srv.Close()
	p := mustNew(t, "openai", Config{APIKey: "sk", BaseURL: srv.URL})
	img := media.FromBytes(pngData)
	res, err := p.Generate(context.Background(), Request{Model: "gpt-image-1", Prompt: "make it blue", N: 1, Images: []media.Image{img, img}, Mask: &img})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Images) != 1 {
		t.Fatalf("got %d images", len(res.Images))
	}
}

func TestAuthErrorOn401(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"Incorrect API key"}}`))
	}))
	defer srv.Close()
	p := mustNew(t, "openai", Config{APIKey: "bad", BaseURL: srv.URL, Source: "env"})
	_, err := p.Generate(context.Background(), Request{Model: "gpt-image-1", Prompt: "x", N: 1})
	authErr, ok := err.(*AuthError)
	if !ok || !strings.Contains(authErr.Message, "OPENAI_API_KEY") {
		t.Fatalf("expected auth error naming the env var, got %v", err)
	}
}

func TestRetriesTransientErrors(t *testing.T) {
	var calls atomic.Int32
	pngData := testPNG(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{map[string]any{"b64_json": base64.StdEncoding.EncodeToString(pngData)}}})
	}))
	defer srv.Close()
	p := mustNew(t, "openai", Config{APIKey: "sk", BaseURL: srv.URL, MaxRetries: 2})
	if _, err := p.Generate(context.Background(), Request{Model: "gpt-image-1", Prompt: "x", N: 1}); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Fatalf("expected 2 calls, got %d", calls.Load())
	}
}

func TestGatewayImageModel(t *testing.T) {
	pngData := testPNG(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/image-model" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if r.Header.Get("ai-model-id") != "bfl/flux-2-pro" || r.Header.Get("ai-image-model-specification-version") != "4" || r.Header.Get("x-vercel-ai-gateway-team") != "my-team" {
			t.Errorf("headers: %v", r.Header)
		}
		body := decodeBody(t, r)
		if body["seed"] != float64(0) || body["aspectRatio"] != "1:1" {
			t.Errorf("body: %v", body)
		}
		files := body["files"].([]any)
		if files[0].(map[string]any)["type"] != "file" || files[1].(map[string]any)["type"] != "url" {
			t.Errorf("files: %v", files)
		}
		po := body["providerOptions"].(map[string]any)
		if po["bfl"].(map[string]any)["raw"] != true {
			t.Errorf("providerOptions: %v", po)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"images": []string{base64.StdEncoding.EncodeToString(pngData)}, "warnings": []any{map[string]any{"type": "unsupported", "feature": "size"}}})
	}))
	defer srv.Close()
	p := mustNew(t, "gateway", Config{APIKey: "gw", BaseURL: srv.URL, Scope: "my-team"})
	zero := int64(0)
	res, err := p.Generate(context.Background(), Request{
		Model: "bfl/flux-2-pro", Prompt: "x", N: 1, AspectRatio: "1:1", Seed: &zero,
		Images:         []media.Image{media.FromBytes(pngData), {URL: "https://example.com/a.png"}},
		GatewayOptions: map[string]any{"bfl": map[string]any{"raw": true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Images) != 1 || len(res.Warnings) != 1 || res.Warnings[0] != "unsupported: size" {
		t.Fatalf("result: %+v", res)
	}
}

func TestHiggsfieldQueueAndUpload(t *testing.T) {
	pngData := testPNG(t)
	var srv *httptest.Server
	var statusCalls atomic.Int32
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/files/generate-upload-url":
			if r.Header.Get("Authorization") != "Key kid:ksecret" {
				t.Errorf("auth = %q", r.Header.Get("Authorization"))
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"public_url": srv.URL + "/cdn/input.png", "upload_url": srv.URL + "/upload", "upload_headers": map[string]string{"Content-Type": "image/png"}})
		case r.URL.Path == "/upload":
			if r.Method != http.MethodPut || r.Header.Get("Authorization") != "" {
				t.Errorf("upload must be a credential-free PUT, got %s auth=%q", r.Method, r.Header.Get("Authorization"))
			}
			b, _ := io.ReadAll(r.Body)
			if !bytes.Equal(b, pngData) {
				t.Error("upload body mismatch")
			}
		case r.URL.Path == "/xai/grok-imagine-image-2.0":
			body := decodeBody(t, r)
			urls, _ := body["image_urls"].([]any)
			if len(urls) != 1 || urls[0] != srv.URL+"/cdn/input.png" || body["aspect_ratio"] != "16:9" || body["quality"] != "high" {
				t.Errorf("body: %v", body)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "queued", "request_id": "r1", "status_url": srv.URL + "/requests/r1/status"})
		case r.URL.Path == "/requests/r1/status":
			if statusCalls.Add(1) < 2 {
				_ = json.NewEncoder(w).Encode(map[string]any{"status": "in_progress", "request_id": "r1"})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "completed", "request_id": "r1", "images": []any{map[string]any{"url": srv.URL + "/cdn/out.png"}}})
		case r.URL.Path == "/cdn/out.png":
			_, _ = w.Write(pngData)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()
	p := mustNew(t, "higgsfield", Config{APIKey: "kid", APISecret: "ksecret", BaseURL: srv.URL})
	res, err := p.Generate(context.Background(), Request{Model: "xai/grok-imagine-image-2.0", Prompt: "x", N: 1, AspectRatio: "16:9",
		Images: []media.Image{media.FromBytes(pngData)}, Options: map[string]any{"quality": "high"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Images) != 1 || res.ID != "r1" {
		t.Fatalf("result: %+v", res)
	}
}

func TestHiggsfieldNSFW(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "nsfw", "request_id": "r2"})
	}))
	defer srv.Close()
	p := mustNew(t, "higgsfield", Config{APIKey: "a", APISecret: "b", BaseURL: srv.URL})
	_, err := p.Generate(context.Background(), Request{Model: "higgsfield-ai/soul/standard", Prompt: "x", N: 1})
	if err == nil || !strings.Contains(err.Error(), "NSFW") {
		t.Fatalf("expected NSFW error, got %v", err)
	}
}

func TestFalQueue(t *testing.T) {
	pngData := testPNG(t)
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Key fk" && !strings.HasPrefix(r.URL.Path, "/files") {
			t.Errorf("auth = %q", r.Header.Get("Authorization"))
		}
		switch r.URL.Path {
		case "/fal-ai/flux/dev":
			body := decodeBody(t, r)
			if body["num_images"] != float64(2) || body["image_size"].(map[string]any)["width"] != float64(512) {
				t.Errorf("body: %v", body)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"request_id": "f1", "status_url": srv.URL + "/status", "response_url": srv.URL + "/response"})
		case "/status":
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "COMPLETED"})
		case "/response":
			_ = json.NewEncoder(w).Encode(map[string]any{"images": []any{map[string]any{"url": srv.URL + "/files/a.png"}, map[string]any{"url": "data:image/png;base64," + base64.StdEncoding.EncodeToString(pngData)}}, "seed": 42})
		case "/files/a.png":
			_, _ = w.Write(pngData)
		}
	}))
	defer srv.Close()
	p := mustNew(t, "fal", Config{APIKey: "fk", BaseURL: srv.URL})
	res, err := p.Generate(context.Background(), Request{Model: "fal-ai/flux/dev", Prompt: "x", N: 2, Size: "512x768"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Images) != 2 {
		t.Fatalf("got %d images", len(res.Images))
	}
}

func TestBFLPolling(t *testing.T) {
	pngData := testPNG(t)
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/flux-kontext-pro":
			if r.Header.Get("x-key") != "bk" {
				t.Errorf("x-key = %q", r.Header.Get("x-key"))
			}
			body := decodeBody(t, r)
			if body["input_image"] == nil {
				t.Error("missing input_image")
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "b1", "polling_url": srv.URL + "/poll"})
		case "/poll":
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "Ready", "result": map[string]any{"sample": srv.URL + "/img"}})
		case "/img":
			_, _ = w.Write(pngData)
		}
	}))
	defer srv.Close()
	p := mustNew(t, "bfl", Config{APIKey: "bk", BaseURL: srv.URL})
	res, err := p.Generate(context.Background(), Request{Model: "flux-kontext-pro", Prompt: "x", N: 1, Images: []media.Image{media.FromBytes(pngData)}})
	if err != nil || len(res.Images) != 1 {
		t.Fatalf("res=%+v err=%v", res, err)
	}
}

func TestReplicateVersionAndModelRoutes(t *testing.T) {
	pngData := testPNG(t)
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/predictions":
			body := decodeBody(t, r)
			if body["version"] != "abc" {
				t.Errorf("version body: %v", body)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "p1", "status": "starting", "urls": map[string]any{"get": srv.URL + "/predictions/p1"}})
		case "/predictions/p1":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "p1", "status": "succeeded", "output": []string{srv.URL + "/out.png"}})
		case "/models/black-forest-labs/flux-schnell/predictions":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "p2", "status": "succeeded", "output": srv.URL + "/out.png"})
		case "/out.png":
			_, _ = w.Write(pngData)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()
	p := mustNew(t, "replicate", Config{APIKey: "rk", BaseURL: srv.URL})
	for _, model := range []string{"owner/model:abc", "black-forest-labs/flux-schnell"} {
		res, err := p.Generate(context.Background(), Request{Model: model, Prompt: "x", N: 1})
		if err != nil || len(res.Images) != 1 {
			t.Fatalf("%s: res=%+v err=%v", model, res, err)
		}
	}
}

func TestGoogleGemini(t *testing.T) {
	pngData := testPNG(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models/gemini-2.5-flash-image:generateContent" || r.Header.Get("x-goog-api-key") != "gk" {
			t.Errorf("path=%s key=%s", r.URL.Path, r.Header.Get("x-goog-api-key"))
		}
		body := decodeBody(t, r)
		gc := body["generationConfig"].(map[string]any)
		if gc["imageConfig"].(map[string]any)["aspectRatio"] != "3:4" || gc["imageConfig"].(map[string]any)["imageSize"] != "2K" {
			t.Errorf("generationConfig: %v", gc)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"candidates": []any{map[string]any{"content": map[string]any{"parts": []any{
			map[string]any{"text": "Here you go"},
			map[string]any{"inlineData": map[string]any{"mimeType": "image/png", "data": base64.StdEncoding.EncodeToString(pngData)}},
		}}}}})
	}))
	defer srv.Close()
	p := mustNew(t, "google", Config{APIKey: "gk", BaseURL: srv.URL})
	res, err := p.Generate(context.Background(), Request{Model: "gemini-2.5-flash-image", Prompt: "x", N: 1, AspectRatio: "3:4",
		Options: map[string]any{"generationConfig": map[string]any{"imageConfig": map[string]any{"imageSize": "2K"}}}})
	if err != nil || len(res.Images) != 1 || res.Text != "Here you go" {
		t.Fatalf("res=%+v err=%v", res, err)
	}
}

func TestStabilityMultipart(t *testing.T) {
	pngData := testPNG(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/stable-image/generate/sd3" {
			t.Errorf("path %s", r.URL.Path)
		}
		_ = r.ParseMultipartForm(1 << 20)
		if r.FormValue("model") != "sd3.5-large" || r.FormValue("seed") != "7" {
			t.Errorf("form %v", r.MultipartForm.Value)
		}
		w.Header().Set("seed", "7")
		_, _ = w.Write(pngData)
	}))
	defer srv.Close()
	p := mustNew(t, "stability", Config{APIKey: "sk", BaseURL: srv.URL})
	seed := int64(7)
	res, err := p.Generate(context.Background(), Request{Model: "sd3.5-large", Prompt: "x", N: 1, Seed: &seed})
	if err != nil || len(res.Images) != 1 {
		t.Fatalf("res=%+v err=%v", res, err)
	}
}

func TestChatImageModalities(t *testing.T) {
	pngData := testPNG(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := decodeBody(t, r)
		if mods := body["modalities"].([]any); len(mods) != 2 {
			t.Errorf("modalities %v", mods)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "c1", "choices": []any{map[string]any{"message": map[string]any{
			"content": "done", "images": []any{map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64," + base64.StdEncoding.EncodeToString(pngData)}}},
		}}}})
	}))
	defer srv.Close()
	p := mustNew(t, "openrouter", Config{APIKey: "ok", BaseURL: srv.URL})
	res, err := p.Generate(context.Background(), Request{Model: "google/gemini-2.5-flash-image", Prompt: "x", N: 1})
	if err != nil || len(res.Images) != 1 || res.ID != "c1" {
		t.Fatalf("res=%+v err=%v", res, err)
	}
}

func sse(events ...map[string]any) string {
	var b strings.Builder
	for _, e := range events {
		data, _ := json.Marshal(e)
		b.WriteString("event: " + e["type"].(string) + "\ndata: " + string(data) + "\n\n")
	}
	return b.String()
}

func TestCodexResponsesStream(t *testing.T) {
	pngData := testPNG(t)
	b64 := base64.StdEncoding.EncodeToString(pngData)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses" {
			t.Errorf("path %s", r.URL.Path)
		}
		if r.Header.Get("ChatGPT-Account-ID") != "acct_1" || r.Header.Get("originator") != "codex_cli_rs" || r.Header.Get("Authorization") != "Bearer tok" {
			t.Errorf("headers %v", r.Header)
		}
		body := decodeBody(t, r)
		if body["stream"] != true || body["store"] != false || body["instructions"] == "" {
			t.Errorf("body %v", body)
		}
		tool := body["tools"].([]any)[0].(map[string]any)
		if tool["type"] != "image_generation" || tool["quality"] != "high" {
			t.Errorf("tool %v", tool)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		item := map[string]any{"type": "image_generation_call", "id": "ig_1", "status": "completed", "result": b64}
		_, _ = io.WriteString(w, sse(
			map[string]any{"type": "response.created", "response": map[string]any{"id": "resp_1"}},
			map[string]any{"type": "response.output_item.done", "item": item},
			map[string]any{"type": "response.completed", "response": map[string]any{"id": "resp_1", "output": []any{item}, "usage": map[string]any{"total_tokens": 99}}},
		))
	}))
	defer srv.Close()
	p := mustNew(t, "codex", Config{BaseURL: srv.URL, OAuth: &auth.Credential{AccessToken: "tok", AccountID: "acct_1"}})
	res, err := p.Generate(context.Background(), Request{Model: "gpt-5.5", Prompt: "x", N: 1, Options: map[string]any{"quality": "high"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Images) != 1 || res.ID != "resp_1" || res.Usage == nil || *res.Usage.TotalTokens != 99 {
		t.Fatalf("res=%+v", res)
	}
}

func TestCodexStreamFailure(t *testing.T) {
	_, err := parseResponsesStream([]byte(sse(map[string]any{"type": "response.failed", "response": map[string]any{"error": map[string]any{"message": "usage limit reached"}}})))
	if err == nil || !strings.Contains(err.Error(), "usage limit reached") {
		t.Fatalf("err = %v", err)
	}
}

func TestCodexNativeFallsBackToResponses(t *testing.T) {
	pngData := testPNG(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/images/generations":
			w.WriteHeader(http.StatusNotFound)
		case "/responses":
			item := map[string]any{"type": "image_generation_call", "id": "ig", "status": "completed", "result": base64.StdEncoding.EncodeToString(pngData)}
			_, _ = io.WriteString(w, sse(map[string]any{"type": "response.output_item.done", "item": item}))
		}
	}))
	defer srv.Close()
	p := mustNew(t, "codex", Config{BaseURL: srv.URL, OAuth: &auth.Credential{AccessToken: "tok"}})
	res, err := p.Generate(context.Background(), Request{Model: "gpt-image-2", Prompt: "x", N: 1})
	if err != nil || len(res.Images) != 1 || len(res.Warnings) == 0 {
		t.Fatalf("res=%+v err=%v", res, err)
	}
}

func TestGrokUsesOAuthBearer(t *testing.T) {
	pngData := testPNG(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer grok-token" {
			t.Errorf("auth = %q", r.Header.Get("Authorization"))
		}
		body := decodeBody(t, r)
		if body["aspect_ratio"] != "2:3" || body["response_format"] != "b64_json" {
			t.Errorf("body %v", body)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{map[string]any{"b64_json": base64.StdEncoding.EncodeToString(pngData)}}})
	}))
	defer srv.Close()
	p := mustNew(t, "grok", Config{BaseURL: srv.URL, OAuth: &auth.Credential{AccessToken: "grok-token"}})
	res, err := p.Generate(context.Background(), Request{Model: "grok-imagine-image", Prompt: "x", N: 1, AspectRatio: "2:3"})
	if err != nil || len(res.Images) != 1 {
		t.Fatalf("res=%+v err=%v", res, err)
	}
}

func TestMergeDeep(t *testing.T) {
	got := Merge(map[string]any{"a": map[string]any{"x": 1, "y": 2}, "b": 1}, map[string]any{"a": map[string]any{"y": 3}, "c": 4})
	a := got["a"].(map[string]any)
	if a["x"] != 1 || a["y"] != 3 || got["b"] != 1 || got["c"] != 4 {
		t.Fatalf("merge = %v", got)
	}
}

func TestExtractSVG(t *testing.T) {
	text := "Here: <svg width=\"2\"><g><svg/></g><text>a > b</text></svg> done <svg></svg>"
	got := ExtractSVG(text)
	if got != `<svg width="2"><g><svg/></g><text>a > b</text></svg>` {
		t.Fatalf("got %q", got)
	}
	if ExtractSVG("no markup") != "" {
		t.Fatal("expected empty")
	}
}

func TestRetryPolicyProtectsPaidSubmits(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()
	ctx := context.Background()
	if _, err := do(ctx, call{Provider: "t", URL: srv.URL, JSON: map[string]any{}, MaxRetries: 2}); err == nil {
		t.Fatal("expected error")
	}
	if calls.Load() != 1 {
		t.Fatalf("a POST that may have started paid work must not be retried on 502, got %d calls", calls.Load())
	}
	calls.Store(0)
	_, _ = do(ctx, call{Provider: "t", Method: http.MethodGet, URL: srv.URL, MaxRetries: 1})
	if calls.Load() != 2 {
		t.Fatalf("GET polls should retry, got %d calls", calls.Load())
	}
}

func TestParseRetryAfter(t *testing.T) {
	if parseRetryAfter("") != -1 || parseRetryAfter("bogus") != -1 {
		t.Fatal("absent or invalid values are -1")
	}
	if parseRetryAfter("120") != 120*time.Second {
		t.Fatal("seconds above 60 must be honored")
	}
	if d := parseRetryAfter(time.Now().Add(30 * time.Second).UTC().Format(http.TimeFormat)); d < 25*time.Second || d > 31*time.Second {
		t.Fatalf("http-date gave %s", d)
	}
}
