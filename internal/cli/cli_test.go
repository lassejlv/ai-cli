package cli

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lassejlv/ai-cli/internal/auth"
	"github.com/lassejlv/ai-cli/internal/output"
	"github.com/lassejlv/ai-cli/internal/provider"
)

type providerInfo = provider.Info

var lookupProvider = provider.Lookup

func isolate(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("AI_CLI_CONFIG_DIR", filepath.Join(dir, "config"))
	t.Setenv("HOME", dir)
	t.Setenv("CODEX_HOME", filepath.Join(dir, "codex"))
	t.Setenv("AI_CLI_OUTPUT_DIR", "")
	t.Setenv("AI_CLI_IMAGE_PROVIDER", "")
	t.Setenv("AI_CLI_IMAGE_MODEL", "")
	for _, k := range []string{"AI_GATEWAY_API_KEY", "OPENAI_API_KEY", "OPENAI_BASE_URL", "FAL_KEY", "REPLICATE_API_TOKEN", "HF_KEY", "HF_API_KEY", "XAI_API_KEY", "GEMINI_API_KEY", "GOOGLE_API_KEY", "GOOGLE_GENERATIVE_AI_API_KEY", "BFL_API_KEY", "STABILITY_API_KEY", "OPENROUTER_API_KEY"} {
		t.Setenv(k, "")
	}
}

func TestParseModelSpec(t *testing.T) {
	isolate(t)
	cases := []struct {
		in   string
		want ModelSpec
	}{
		{"openai:gpt-image-1", ModelSpec{Provider: "openai", Model: "gpt-image-1"}},
		{"openai@work:gpt-image-1", ModelSpec{Provider: "openai", Account: "work", Model: "gpt-image-1"}},
		{"higgsfield:soul", ModelSpec{Provider: "higgsfield", Model: "higgsfield-ai/soul/standard"}},
		{"codex", ModelSpec{Provider: "codex", Model: "gpt-image-2"}},
		{"grok@personal", ModelSpec{Provider: "grok", Account: "personal", Model: "grok-imagine-image"}},
		{"replicate:owner/model:abc", ModelSpec{Provider: "replicate", Model: "owner/model:abc"}},
		{"openai/gpt-image-2", ModelSpec{Provider: "gateway", Model: "openai/gpt-image-2"}},
		{"owner/model:version", ModelSpec{Provider: "gateway", Model: "owner/model:version"}},
	}
	for _, c := range cases {
		got, err := ParseModelSpec(c.in, "gateway", "")
		if err != nil {
			t.Fatalf("%s: %v", c.in, err)
		}
		if got != c.want {
			t.Errorf("%s: got %+v want %+v", c.in, got, c.want)
		}
	}
	if _, err := ParseModelSpec("openai:bad model", "gateway", ""); err == nil {
		t.Error("expected error for model with a space")
	}
}

func TestParseParams(t *testing.T) {
	got, err := parseParams([]string{"quality=high", "n=2", "a.b=true", "higgsfield:enhance_prompt=true", `colors=["red"]`})
	if err != nil {
		t.Fatal(err)
	}
	if got["quality"] != "high" || got["n"] != float64(2) || got["a"].(map[string]any)["b"] != true {
		t.Errorf("params = %v", got)
	}
	if got["provider:higgsfield"].(map[string]any)["enhance_prompt"] != true {
		t.Errorf("scoped = %v", got)
	}
	opts := RunOptions{Params: got}
	if o := providerOptionsFor("higgsfield", opts); o["enhance_prompt"] != true || o["quality"] != "high" {
		t.Errorf("higgsfield options = %v", o)
	}
	if o := providerOptionsFor("openai", opts); o["enhance_prompt"] != nil {
		t.Errorf("scoped param leaked to openai: %v", o)
	}
	if _, err := parseParams([]string{"nope"}); err == nil {
		t.Error("expected error")
	}
	if _, err := parseParams([]string{"notaprovider:x=1"}); err == nil {
		t.Error("expected unknown provider error")
	}
}

func TestMultipleAccounts(t *testing.T) {
	isolate(t)
	if err := auth.Set("openai", "personal", auth.Credential{APIKey: "sk-personal"}, false); err != nil {
		t.Fatal(err)
	}
	if err := auth.Set("openai", "work", auth.Credential{APIKey: "sk-work", BaseURL: "https://proxy.example/v1"}, false); err != nil {
		t.Fatal(err)
	}
	info := mustInfo(t, "openai")
	cfg, ra, err := resolveConfig(info, "", Overrides{})
	if err != nil || cfg.APIKey != "sk-personal" || ra.Account != "personal" {
		t.Fatalf("first account should be default: cfg=%+v ra=%+v err=%v", cfg, ra, err)
	}
	cfg, ra, _ = resolveConfig(info, "work", Overrides{})
	if cfg.APIKey != "sk-work" || cfg.BaseURL != "https://proxy.example/v1" || ra.Account != "work" {
		t.Fatalf("work account: %+v", cfg)
	}
	if err := auth.SetDefaultAccount("openai", "work"); err != nil {
		t.Fatal(err)
	}
	if cfg, _, _ = resolveConfig(info, "", Overrides{}); cfg.APIKey != "sk-work" {
		t.Fatalf("default switch failed: %+v", cfg)
	}
	t.Setenv("OPENAI_API_KEY", "sk-env")
	if cfg, ra, _ = resolveConfig(info, "", Overrides{}); cfg.APIKey != "sk-env" || ra.Source != "env" {
		t.Fatalf("env should win without --account: %+v", ra)
	}
	if cfg, _, _ = resolveConfig(info, "personal", Overrides{}); cfg.APIKey != "sk-personal" {
		t.Fatal("explicit account should win over env")
	}
	if _, _, err := resolveConfig(info, "missing", Overrides{}); err == nil || !strings.Contains(err.Error(), "personal, work") && !strings.Contains(err.Error(), "work, personal") {
		t.Fatalf("missing account error = %v", err)
	}
	if n, _ := auth.Delete("openai", "work"); n != 1 {
		t.Fatal("delete failed")
	}
	f, _ := auth.Load()
	if names := f.AccountNames("openai"); len(names) != 1 || names[0] != "personal" {
		t.Fatalf("names = %v", names)
	}
}

func TestLegacyAuthFile(t *testing.T) {
	isolate(t)
	if err := os.MkdirAll(auth.ConfigDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(auth.Path(), []byte(`{"apiKey":"vck_legacy"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cred, account, ok := auth.Get("gateway", "")
	if !ok || cred.APIKey != "vck_legacy" || account != auth.DefaultAccount {
		t.Fatalf("legacy key not migrated: %+v %s %v", cred, account, ok)
	}
}

func TestDefaultProviderSelection(t *testing.T) {
	isolate(t)
	if p, _ := defaultProvider(""); p != "gateway" {
		t.Fatalf("no credentials should fall back to gateway, got %s", p)
	}
	t.Setenv("FAL_KEY", "x")
	if p, _ := defaultProvider(""); p != "fal" {
		t.Fatalf("expected fal, got %s", p)
	}
	_ = auth.SetDefaultProvider("higgsfield")
	if p, _ := defaultProvider(""); p != "higgsfield" {
		t.Fatalf("stored default should win, got %s", p)
	}
	t.Setenv("AI_CLI_IMAGE_PROVIDER", "openai")
	if p, _ := defaultProvider(""); p != "openai" {
		t.Fatalf("env should win, got %s", p)
	}
	if p, _ := defaultProvider("bfl"); p != "bfl" {
		t.Fatalf("flag should win, got %s", p)
	}
}

func mustInfo(t *testing.T, name string) *providerInfo {
	t.Helper()
	info, ok := lookupProvider(name)
	if !ok {
		t.Fatalf("missing provider %s", name)
	}
	return info
}

func pngBytes(t *testing.T, w, h int) []byte {
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, w, h))); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func mockOpenAI(t *testing.T, fail map[string]bool) *httptest.Server {
	data := pngBytes(t, 8, 8)
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if fail[body["model"].(string)] {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"model not supported"}}`))
			return
		}
		n := 1
		if v, ok := body["n"].(float64); ok {
			n = int(v)
		}
		items := make([]any, n)
		for i := range items {
			items[i] = map[string]any{"b64_json": base64.StdEncoding.EncodeToString(data)}
		}
		w.Header().Set("x-request-id", "req_abc")
		_ = json.NewEncoder(w).Encode(map[string]any{"data": items})
	}))
}

func TestRunSavesFilesAndManifest(t *testing.T) {
	isolate(t)
	srv := mockOpenAI(t, nil)
	defer srv.Close()
	t.Setenv("OPENAI_API_KEY", "sk")
	dir := t.TempDir() + "/out/"
	specs, err := ResolveModelSpecs(context.Background(), "openai:gpt-image-1,openai:gpt-image-2", "", "")
	if err != nil {
		t.Fatal(err)
	}
	var stdout bytes.Buffer
	res := Run(context.Background(), specs, RunOptions{
		Prompt: "x", N: 3, MaxPerCall: 2, Concurrency: 2, Timeout: 10 * time.Second, Output: dir, Mode: ModeFile, JSON: true,
		Overrides: Overrides{BaseURL: srv.URL}, Stdout: &stdout,
	})
	if res.ExitCode() != 0 {
		t.Fatalf("exit %d: %+v", res.ExitCode(), res.Results[0].Error)
	}
	for mi, r := range res.Results {
		if len(r.Images) != 3 || len(r.Batches) != 2 {
			t.Fatalf("model %d: images=%d batches=%d", mi, len(r.Images), len(r.Batches))
		}
		for _, img := range r.Images {
			if img.File == nil || !strings.HasPrefix(filepath.Base(*img.File), "req_abc-") || img.Width != 8 {
				t.Fatalf("bad artifact %+v", img)
			}
			if _, err := os.Stat(*img.File); err != nil {
				t.Fatal(err)
			}
		}
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 6 {
		t.Fatalf("expected 6 files, got %d", len(entries))
	}
	if stdout.Len() != 0 {
		t.Fatal("Run must not print the manifest itself")
	}
}

func TestRunPartialFailureExitCode(t *testing.T) {
	isolate(t)
	srv := mockOpenAI(t, map[string]bool{"dall-e-2": true})
	defer srv.Close()
	t.Setenv("OPENAI_API_KEY", "sk")
	specs, _ := ResolveModelSpecs(context.Background(), "openai:gpt-image-1,openai:dall-e-2", "", "")
	res := Run(context.Background(), specs, RunOptions{Prompt: "x", N: 1, Timeout: 5 * time.Second, Mode: ModeFile, JSON: true, NoSave: true, Overrides: Overrides{BaseURL: srv.URL, MaxRetries: 0}})
	if res.ExitCode() != 2 {
		t.Fatalf("exit = %d", res.ExitCode())
	}
	if res.Results[1].Error == nil || !strings.Contains(res.Results[1].Error.Message, "model not supported") {
		t.Fatalf("error = %+v", res.Results[1].Error)
	}
}

func TestRunBase64AndRawModes(t *testing.T) {
	isolate(t)
	srv := mockOpenAI(t, nil)
	defer srv.Close()
	t.Setenv("OPENAI_API_KEY", "sk")
	specs, _ := ResolveModelSpecs(context.Background(), "openai:gpt-image-1", "", "")
	cwd, _ := os.Getwd()
	tmp := t.TempDir()
	_ = os.Chdir(tmp)
	defer os.Chdir(cwd)

	var out bytes.Buffer
	Run(context.Background(), specs, RunOptions{Prompt: "x", N: 2, Timeout: 5 * time.Second, Mode: ModeBase64, Quiet: true, Stdout: &out, Overrides: Overrides{BaseURL: srv.URL}})
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 base64 lines, got %q", out.String())
	}
	if data, err := base64.StdEncoding.DecodeString(lines[0]); err != nil || !bytes.HasPrefix(data, []byte("\x89PNG")) {
		t.Fatal("line is not base64 PNG")
	}

	out.Reset()
	Run(context.Background(), specs, RunOptions{Prompt: "x", N: 1, Timeout: 5 * time.Second, Mode: ModeDataURL, Quiet: true, Stdout: &out, Overrides: Overrides{BaseURL: srv.URL}})
	if !strings.HasPrefix(out.String(), "data:image/png;base64,") {
		t.Fatalf("data url = %q", out.String()[:min(40, out.Len())])
	}

	out.Reset()
	Run(context.Background(), specs, RunOptions{Prompt: "x", N: 1, Timeout: 5 * time.Second, Mode: ModeRaw, Quiet: true, Stdout: &out, Overrides: Overrides{BaseURL: srv.URL}})
	if !bytes.HasPrefix(out.Bytes(), []byte("\x89PNG")) {
		t.Fatal("raw mode should write PNG bytes")
	}

	out.Reset()
	res := Run(context.Background(), specs, RunOptions{Prompt: "x", N: 1, Timeout: 5 * time.Second, Mode: ModeBase64, JSON: true, Stdout: &out, Overrides: Overrides{BaseURL: srv.URL}})
	if out.Len() != 0 || res.Results[0].Images[0].Base64 == "" {
		t.Fatal("--json --base64 should embed base64 in the manifest only")
	}
	if entries, _ := os.ReadDir(tmp); len(entries) != 0 {
		t.Fatalf("stdout modes must not write files without -o, found %d", len(entries))
	}

	// Default mode with a piped stdout writes a single image raw.
	out.Reset()
	Run(context.Background(), specs, RunOptions{Prompt: "x", N: 1, Timeout: 5 * time.Second, Mode: ModeFile, Quiet: true, Stdout: &out, StdoutIsTTY: false, Overrides: Overrides{BaseURL: srv.URL}})
	if !bytes.HasPrefix(out.Bytes(), []byte("\x89PNG")) {
		t.Fatal("piped single image should go to stdout")
	}
}

func TestBuildRunValidation(t *testing.T) {
	isolate(t)
	base := func() *imageFlags {
		return &imageFlags{n: 1, concurrency: 1, timeout: 1, model: "openai:gpt-image-1"}
	}
	f := base()
	f.raw, f.base64 = true, true
	if _, _, err := buildRun(context.Background(), f, "x", nil); err == nil {
		t.Error("expected mutually exclusive error")
	}
	f = base()
	f.raw, f.n = true, 2
	if _, _, err := buildRun(context.Background(), f, "x", nil); err == nil {
		t.Error("expected raw single-image error")
	}
	f = base()
	f.size = "big"
	if _, _, err := buildRun(context.Background(), f, "x", nil); err == nil {
		t.Error("expected size error")
	}
	f = base()
	f.mask = "mask.png"
	if _, _, err := buildRun(context.Background(), f, "x", nil); err == nil {
		t.Error("expected mask error")
	}
	if _, _, err := buildRun(context.Background(), base(), "", nil); err == nil {
		t.Error("expected missing prompt error")
	}
	f = base()
	f.seed = "0"
	opts, _, err := buildRun(context.Background(), f, "x", nil)
	if err != nil || opts.Seed == nil || *opts.Seed != 0 {
		t.Errorf("seed 0 should be kept: %v %v", opts.Seed, err)
	}
}

func TestKittyEncoding(t *testing.T) {
	t.Setenv("TMUX", "")
	seq, err := output.EncodeKitty(pngBytes(t, 4, 4), output.KittyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	s := string(seq)
	if !strings.HasPrefix(s, "\x1b_Ga=T,f=100,q=2,m=0") || !strings.HasSuffix(s, "\x1b\\\n") {
		t.Fatalf("unexpected kitty sequence %q", s[:min(len(s), 40)])
	}
	big, err := output.EncodeKitty(pngBytes(t, 400, 400), output.KittyOptions{Columns: 40})
	if err != nil || !strings.Contains(string(big), "c=40") {
		t.Fatal("expected column limit in first chunk")
	}
	if _, err := output.EncodeKitty([]byte("<svg></svg>"), output.KittyOptions{}); err == nil {
		t.Fatal("svg should not be previewed")
	}
}

func TestSchemaIncludesImageFlags(t *testing.T) {
	isolate(t)
	root := NewRoot()
	cs := describe(root)
	var image *commandSchema
	for i := range cs.Commands {
		if cs.Commands[i].Name == "image" {
			image = &cs.Commands[i]
		}
	}
	if image == nil {
		t.Fatal("image command missing from schema")
	}
	want := map[string]bool{"model": false, "raw": false, "base64": false, "data-url": false, "json": false, "account": false, "dry-run": false}
	for _, f := range image.Flags {
		if _, ok := want[f.Name]; ok {
			want[f.Name] = true
		}
	}
	for name, ok := range want {
		if !ok {
			t.Errorf("schema is missing --%s", name)
		}
	}
}

func TestMCPGenerate(t *testing.T) {
	isolate(t)
	srv := mockOpenAI(t, nil)
	defer srv.Close()
	t.Setenv("OPENAI_API_KEY", "sk")
	t.Setenv("OPENAI_BASE_URL", srv.URL)
	outDir := t.TempDir()
	in := strings.NewReader(strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"generate_image","arguments":{"prompt":"a cat","model":"openai:gpt-image-1","output_dir":"` + outDir + `","return_images":true}}}`,
	}, "\n") + "\n")
	var out bytes.Buffer
	if err := serveMCP(context.Background(), in, &out); err != nil {
		t.Fatal(err)
	}
	responses := map[float64]map[string]any{}
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		var msg map[string]any
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			t.Fatalf("bad json %q", line)
		}
		responses[msg["id"].(float64)] = msg
	}
	if len(responses) != 3 {
		t.Fatalf("expected 3 responses, got %d: %s", len(responses), out.String())
	}
	tools := responses[2]["result"].(map[string]any)["tools"].([]any)
	if len(tools) != 4 {
		t.Fatalf("tools = %d", len(tools))
	}
	result := responses[3]["result"].(map[string]any)
	if result["isError"] != false {
		t.Fatalf("generate failed: %v", result)
	}
	content := result["content"].([]any)
	if len(content) != 2 || content[1].(map[string]any)["type"] != "image" {
		t.Fatalf("content = %v", content)
	}
	if entries, _ := os.ReadDir(outDir); len(entries) != 1 {
		t.Fatalf("expected 1 saved file, got %d", len(entries))
	}
}

func TestPrepareSharesInstancesPerAccount(t *testing.T) {
	isolate(t)
	_ = auth.Set("codex", "a", auth.Credential{AccessToken: "t1"}, false)
	_ = auth.Set("codex", "b", auth.Credential{AccessToken: "t2"}, false)
	specs := []ModelSpec{{Provider: "codex", Account: "a", Model: "gpt-image-2"}, {Provider: "codex", Account: "a", Model: "gpt-5.5"}, {Provider: "codex", Account: "b", Model: "gpt-image-2"}}
	ms := prepare(specs, Overrides{})
	if ms[0].impl != ms[1].impl {
		t.Fatal("same account should share one provider instance")
	}
	if ms[0].impl == ms[2].impl {
		t.Fatal("different accounts must not share an instance")
	}
	if ms[1].spec.Model != "gpt-5.5" {
		t.Fatal("shared instance must keep its own spec")
	}
}

func TestAccountFlagOnlyForUnprefixedModels(t *testing.T) {
	isolate(t)
	t.Setenv("AI_CLI_IMAGE_PROVIDER", "openai")
	specs, err := ResolveModelSpecs(context.Background(), "gpt-image-2,fal:fal-ai/flux/dev,fal@team:flux", "", "work")
	if err != nil {
		t.Fatal(err)
	}
	if specs[0].Account != "work" || specs[1].Account != "" || specs[2].Account != "team" {
		t.Fatalf("accounts = %q %q %q", specs[0].Account, specs[1].Account, specs[2].Account)
	}
}

func TestBaseURLRejectedAcrossProviders(t *testing.T) {
	isolate(t)
	f := &imageFlags{n: 1, concurrency: 1, timeout: 1, model: "openai:gpt-image-1,fal:flux", baseURL: "http://localhost:1/v1"}
	if _, _, err := buildRun(context.Background(), f, "x", nil); err == nil || !strings.Contains(err.Error(), "--base-url") {
		t.Fatalf("err = %v", err)
	}
	f.model = "openai:gpt-image-1,openai:gpt-image-2"
	if _, _, err := buildRun(context.Background(), f, "x", nil); err != nil {
		t.Fatal(err)
	}
}

func TestRefreshStoredReusesNewerTokens(t *testing.T) {
	isolate(t)
	_ = auth.Set("grok", "default", auth.Credential{AccessToken: "old", RefreshToken: "r1"}, false)
	never := func(auth.Credential) bool { return false }
	always := func(c auth.Credential) bool { return c.AccessToken == "old" }
	calls := 0
	refresh := func(c auth.Credential) (auth.Credential, error) {
		calls++
		if c.RefreshToken != "r1" {
			t.Errorf("refresh must use the stored refresh token, got %q", c.RefreshToken)
		}
		c.AccessToken, c.RefreshToken = "new", "r2"
		return c, nil
	}
	first, err := auth.RefreshStored("grok", "default", auth.Credential{AccessToken: "old", RefreshToken: "r1"}, false, always, refresh)
	if err != nil || first.AccessToken != "new" || calls != 1 {
		t.Fatalf("first = %+v err=%v calls=%d", first, err, calls)
	}
	// A second instance still holding the old token reuses the stored one.
	second, err := auth.RefreshStored("grok", "default", auth.Credential{AccessToken: "old", RefreshToken: "r1"}, true, never, refresh)
	if err != nil || second.AccessToken != "new" || calls != 1 {
		t.Fatalf("second = %+v err=%v calls=%d", second, err, calls)
	}
	stored, _, _ := auth.Get("grok", "")
	if stored.RefreshToken != "r2" {
		t.Fatalf("stored = %+v", stored)
	}
}

func TestLegacyKeyKeptInSync(t *testing.T) {
	isolate(t)
	_ = os.MkdirAll(auth.ConfigDir(), 0o700)
	_ = os.WriteFile(auth.Path(), []byte(`{"apiKey":"vck_legacy"}`), 0o600)
	if err := auth.Set("openai", "", auth.Credential{APIKey: "sk"}, false); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(auth.Path())
	var doc map[string]any
	_ = json.Unmarshal(raw, &doc)
	if doc["apiKey"] != "vck_legacy" {
		t.Fatalf("legacy root apiKey must be preserved, got %v", doc["apiKey"])
	}
}

func TestCodexImportLinks(t *testing.T) {
	isolate(t)
	codexHome := os.Getenv("CODEX_HOME")
	_ = os.MkdirAll(codexHome, 0o700)
	_ = os.WriteFile(filepath.Join(codexHome, "auth.json"), []byte(`{"tokens":{"access_token":"cli-at","refresh_token":"cli-rt","account_id":"acct"}}`), 0o600)
	_ = auth.Set("codex", "default", auth.Credential{Link: "codex-cli"}, false)
	info := mustInfo(t, "codex")
	cfg, ra, err := resolveConfig(info, "", Overrides{})
	if err != nil || cfg.OAuth == nil || cfg.OAuth.AccessToken != "cli-at" || ra.Source != "codex-cli" || ra.Account != "default" {
		t.Fatalf("cfg=%+v ra=%+v err=%v", cfg.OAuth, ra, err)
	}
	stored, _, _ := auth.Get("codex", "default")
	if stored.RefreshToken != "" {
		t.Fatal("linked accounts must not copy the refresh token")
	}
}
