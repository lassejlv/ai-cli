package cli

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/lassejlv/ai-cli/internal/media"
	"github.com/lassejlv/ai-cli/internal/output"
	"github.com/lassejlv/ai-cli/internal/provider"
)

// OutputMode selects how generated images reach the user.
type OutputMode string

const (
	ModeFile    OutputMode = "file"
	ModeRaw     OutputMode = "raw"
	ModeBase64  OutputMode = "base64"
	ModeDataURL OutputMode = "data-url"
)

// RunOptions is a fully validated generation request.
type RunOptions struct {
	Prompt          string
	Images          []media.Image
	Mask            *media.Image
	N               int
	Size            string
	AspectRatio     string
	Seed            *int64
	ProviderOptions map[string]any
	Params          map[string]any
	MaxPerCall      int
	Timeout         time.Duration
	Concurrency     int
	Overrides       Overrides

	Output  string
	Mode    OutputMode
	NoSave  bool
	JSON    bool
	Quiet   bool
	Preview bool
	// IncludeBase64 adds base64 data to JSON results.
	IncludeBase64 bool

	Stdout      io.Writer
	StdoutIsTTY bool
}

// ArtifactInfo describes one saved or emitted image.
type ArtifactInfo struct {
	File      *string `json:"file"`
	MediaType string  `json:"mediaType"`
	Bytes     int     `json:"bytes"`
	ID        string  `json:"id,omitempty"`
	URL       string  `json:"url,omitempty"`
	Base64    string  `json:"base64,omitempty"`
	Width     int     `json:"width,omitempty"`
	Height    int     `json:"height,omitempty"`

	data []byte
}

// ErrorDetail is a JSON-safe error.
type ErrorDetail struct {
	Name    string `json:"name"`
	Message string `json:"message"`
}

// Failure records one failed batch.
type Failure struct {
	Kind      string      `json:"kind"`
	Index     int         `json:"index"`
	Requested int         `json:"requested"`
	Error     ErrorDetail `json:"error"`
}

// BatchInfo records one provider call.
type BatchInfo struct {
	Index            int             `json:"index"`
	Requested        int             `json:"requested"`
	Received         int             `json:"received"`
	ID               string          `json:"id,omitempty"`
	Usage            *provider.Usage `json:"usage,omitempty"`
	ProviderMetadata any             `json:"providerMetadata,omitempty"`
}

// ModelResult is the outcome for one model.
type ModelResult struct {
	Provider      string          `json:"provider"`
	Account       string          `json:"account,omitempty"`
	Model         string          `json:"model"`
	Success       bool            `json:"success"`
	ElapsedMs     int64           `json:"elapsedMs"`
	Images        []*ArtifactInfo `json:"images"`
	Text          string          `json:"text,omitempty"`
	RevisedPrompt string          `json:"revisedPrompt,omitempty"`
	Warnings      []string        `json:"warnings,omitempty"`
	Usage         *provider.Usage `json:"usage,omitempty"`
	Batches       []BatchInfo     `json:"batches,omitempty"`
	Failures      []Failure       `json:"failures,omitempty"`
	Error         *ErrorDetail    `json:"error,omitempty"`
}

// RunResult is the JSON manifest of a run.
type RunResult struct {
	Success   bool           `json:"success"`
	ElapsedMs int64          `json:"elapsedMs"`
	Results   []*ModelResult `json:"results"`
}

// ExitCode maps results to 0 (all ok), 1 (all failed) or 2 (partial).
func (r *RunResult) ExitCode() int {
	failed := 0
	for _, res := range r.Results {
		if !res.Success {
			failed++
		}
	}
	switch {
	case failed == 0:
		return 0
	case failed == len(r.Results):
		return 1
	default:
		return 2
	}
}

func errorDetail(err error) ErrorDetail {
	var authErr *provider.AuthError
	if errors.As(err, &authErr) {
		return ErrorDetail{Name: "AuthenticationError", Message: authErr.Message}
	}
	var apiErr *provider.APIError
	if errors.As(err, &apiErr) {
		return ErrorDetail{Name: "APIError", Message: apiErr.Error()}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return ErrorDetail{Name: "TimeoutError", Message: "request timed out (use --timeout to allow longer)"}
	}
	return ErrorDetail{Name: "Error", Message: err.Error()}
}

type preparedModel struct {
	spec    ModelSpec
	account ResolvedAccount
	impl    provider.Provider
	err     error
}

// prepare instantiates providers for each spec. Specs that share a provider
// and account share one instance, so OAuth refresh tokens rotate only once.
func prepare(specs []ModelSpec, ov Overrides) []preparedModel {
	out := make([]preparedModel, len(specs))
	cache := map[string]preparedModel{}
	for i, spec := range specs {
		key := spec.Provider + "@" + spec.Account
		m, ok := cache[key]
		if !ok {
			m = prepareOne(spec, ov)
			cache[key] = m
		}
		m.spec = spec
		out[i] = m
	}
	return out
}

func prepareOne(spec ModelSpec, ov Overrides) preparedModel {
	m := preparedModel{spec: spec}
	info, ok := provider.Lookup(spec.Provider)
	if !ok {
		m.err = fmt.Errorf("unknown provider %q", spec.Provider)
		return m
	}
	cfg, ra, err := resolveConfig(info, spec.Account, ov)
	m.account = ra
	if err != nil {
		m.err = err
		return m
	}
	cfg.HTTP = &http.Client{}
	m.impl, m.err = info.New(cfg)
	return m
}

// providerOptionsFor returns the options passed to one provider.
func providerOptionsFor(name string, opts RunOptions) map[string]any {
	var own map[string]any
	if v, ok := opts.ProviderOptions[name].(map[string]any); ok {
		own = v
	}
	return provider.Merge(provider.Merge(own, globalParams(opts.Params)), scopedParams(name, opts.Params))
}

func globalParams(params map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range params {
		if !strings.HasPrefix(k, "provider:") {
			out[k] = v
		}
	}
	return out
}

func scopedParams(name string, params map[string]any) map[string]any {
	v, _ := params["provider:"+name].(map[string]any)
	return v
}

// Run generates images for every model and writes the outputs.
func Run(ctx context.Context, specs []ModelSpec, opts RunOptions) *RunResult {
	start := time.Now()
	if opts.Stdout == nil {
		opts.Stdout = os.Stdout
	}
	if opts.Concurrency <= 0 {
		opts.Concurrency = 4
	}
	if opts.N <= 0 {
		opts.N = 1
	}
	models := prepare(specs, opts.Overrides)
	if anyGateway(specs) {
		// Populate the catalog so language image models route correctly.
		_, _ = provider.GatewayImageModels(ctx)
	}

	progress := output.NewProgress(opts.Quiet || opts.JSON)
	lines := make([]int, len(models))
	for i, m := range models {
		lines[i] = progress.Add(fmt.Sprintf("Generating image with %s", m.spec))
	}
	results := make([]*ModelResult, len(models))
	sem := make(chan struct{}, opts.Concurrency)
	var wg sync.WaitGroup
	for i := range models {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			progress.Start(lines[i])
			res := runModel(ctx, models[i], opts)
			results[i] = res
			elapsed := output.FormatElapsed(time.Duration(res.ElapsedMs) * time.Millisecond)
			if res.Error != nil {
				progress.Done(lines[i], fmt.Sprintf("%s %s failed: %s (%s)", cross(), models[i].spec, res.Error.Message, elapsed))
			} else {
				progress.Done(lines[i], fmt.Sprintf("%s Generated %d image%s with %s (%s)", check(), len(res.Images), plural(len(res.Images)), models[i].spec, elapsed))
			}
		}(i)
	}
	wg.Wait()
	progress.Close()

	total := 0
	for _, r := range results {
		total += len(r.Images)
	}
	multiple := len(models) > 1 || opts.N > 1 || total > 1
	showPreview := opts.Preview && !opts.JSON && opts.Mode != ModeRaw && opts.StdoutIsTTY && output.SupportsKitty()
	outDir := opts.Output
	if outDir == "" {
		outDir = os.Getenv("AI_CLI_OUTPUT_DIR")
	}
	for mi, res := range results {
		for ai, art := range res.Images {
			suffix := ""
			if multiple {
				suffix = fmt.Sprintf("%d-%d", mi+1, ai+1)
			}
			emit(opts, res, art, outDir, suffix, multiple)
			if showPreview {
				output.DisplayKitty(os.Stderr, art.data, output.KittyOptions{})
			}
		}
		if !opts.JSON && !opts.Quiet {
			for _, w := range res.Warnings {
				fmt.Fprintf(os.Stderr, "Warning (%s): %s\n", models[mi].spec, w)
			}
			if res.RevisedPrompt != "" {
				fmt.Fprintf(os.Stderr, "Revised prompt: %s\n", res.RevisedPrompt)
			}
			if strings.TrimSpace(res.Text) != "" {
				fmt.Fprintln(os.Stderr, strings.TrimSpace(res.Text))
			}
		}
	}
	run := &RunResult{ElapsedMs: time.Since(start).Milliseconds(), Results: results}
	run.Success = run.ExitCode() == 0
	return run
}

func anyGateway(specs []ModelSpec) bool {
	for _, s := range specs {
		if s.Provider == "gateway" {
			return true
		}
	}
	return false
}

// emit saves or prints one artifact according to the output mode.
func emit(opts RunOptions, res *ModelResult, art *ArtifactInfo, outDir, suffix string, multiple bool) {
	saveFile := func(target string) {
		path, err := output.Save(target, art.data)
		if err != nil {
			res.Warnings = append(res.Warnings, "could not save image: "+err.Error())
			return
		}
		art.File = &path
		if !opts.JSON && !opts.Quiet {
			fmt.Fprintf(os.Stderr, "Saved to %s\n", path)
		}
	}
	target := output.Target(outDir, art.ID, art.MediaType, suffix)
	switch opts.Mode {
	case ModeRaw:
		_, _ = opts.Stdout.Write(art.data)
		if opts.Output != "" {
			saveFile(target)
		}
	case ModeBase64, ModeDataURL:
		encoded := base64.StdEncoding.EncodeToString(art.data)
		if opts.Mode == ModeDataURL {
			encoded = "data:" + art.MediaType + ";base64," + encoded
		}
		if opts.JSON {
			art.Base64 = encoded
		} else {
			fmt.Fprintln(opts.Stdout, encoded)
		}
		if opts.Output != "" {
			saveFile(target)
		}
	default:
		if opts.IncludeBase64 {
			art.Base64 = base64.StdEncoding.EncodeToString(art.data)
		}
		if opts.NoSave {
			return
		}
		// A single image piped to another command is written as raw bytes,
		// like the TypeScript CLI.
		if !opts.JSON && !opts.StdoutIsTTY && !multiple && outDir == "" {
			_, _ = opts.Stdout.Write(art.data)
			return
		}
		saveFile(target)
	}
}

func runModel(ctx context.Context, m preparedModel, opts RunOptions) *ModelResult {
	start := time.Now()
	res := &ModelResult{Provider: m.spec.Provider, Account: m.account.Account, Model: m.spec.Model, Images: []*ArtifactInfo{}}
	defer func() { res.ElapsedMs = time.Since(start).Milliseconds() }()
	if m.err != nil {
		d := errorDetail(m.err)
		res.Error = &d
		return res
	}
	perCall := opts.MaxPerCall
	if perCall <= 0 {
		perCall = m.impl.MaxImagesPerCall(m.spec.Model)
	}
	perCall = max(1, min(perCall, opts.N))
	var counts []int
	for left := opts.N; left > 0; left -= perCall {
		counts = append(counts, min(perCall, left))
	}
	base := provider.Request{
		Model:          m.spec.Model,
		Prompt:         opts.Prompt,
		Images:         opts.Images,
		Mask:           opts.Mask,
		Size:           opts.Size,
		AspectRatio:    opts.AspectRatio,
		Seed:           opts.Seed,
		Options:        providerOptionsFor(m.spec.Provider, opts),
		GatewayOptions: gatewayOptions(m.spec.Model, opts),
		Headers:        opts.Overrides.Headers,
	}
	type outcome struct {
		result *provider.Result
		err    error
	}
	outcomes := make([]outcome, len(counts))
	sem := make(chan struct{}, opts.Concurrency)
	var wg sync.WaitGroup
	for i, count := range counts {
		wg.Add(1)
		go func(i, count int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			callCtx := ctx
			if opts.Timeout > 0 {
				var cancel context.CancelFunc
				callCtx, cancel = context.WithTimeout(ctx, opts.Timeout)
				defer cancel()
			}
			req := base
			req.N = count
			r, err := m.impl.Generate(callCtx, req)
			if err == nil && (r == nil || len(r.Images) == 0) {
				msg := "returned no images"
				if r != nil && strings.TrimSpace(r.Text) != "" {
					msg += ": " + strings.TrimSpace(r.Text)
				}
				err = errors.New(msg)
			}
			if err != nil && errors.Is(callCtx.Err(), context.DeadlineExceeded) {
				err = fmt.Errorf("request timed out after %s (use --timeout to allow longer)", opts.Timeout)
			}
			outcomes[i] = outcome{r, err}
		}(i, count)
	}
	wg.Wait()

	for i, o := range outcomes {
		if o.err != nil {
			res.Failures = append(res.Failures, Failure{Kind: "batch", Index: i + 1, Requested: counts[i], Error: errorDetail(o.err)})
			continue
		}
		r := o.result
		res.Batches = append(res.Batches, BatchInfo{Index: i + 1, Requested: counts[i], Received: len(r.Images), ID: r.ID, Usage: r.Usage, ProviderMetadata: r.ProviderMetadata})
		res.Warnings = append(res.Warnings, r.Warnings...)
		if r.Text != "" {
			res.Text = strings.TrimSpace(res.Text + "\n" + r.Text)
		}
		if res.RevisedPrompt == "" {
			res.RevisedPrompt = r.RevisedPrompt
		}
		res.Usage = addUsage(res.Usage, r.Usage)
		for _, img := range r.Images {
			art := &ArtifactInfo{MediaType: img.MediaType, Bytes: len(img.Data), ID: r.ID, URL: img.URL, data: img.Data}
			art.Width, art.Height = output.Dimensions(img.Data)
			res.Images = append(res.Images, art)
		}
	}
	res.Warnings = dedupe(res.Warnings)
	switch {
	case len(res.Images) == 0 && len(res.Failures) > 0:
		d := res.Failures[0].Error
		res.Error = &d
	case len(res.Images) < opts.N || len(res.Failures) > 0:
		msg := fmt.Sprintf("requested %d image(s), received %d", opts.N, len(res.Images))
		if len(res.Failures) > 0 {
			msg += fmt.Sprintf("; %d batch failure(s): %s", len(res.Failures), res.Failures[0].Error.Message)
		}
		res.Error = &ErrorDetail{Name: "IncompleteResult", Message: msg}
	default:
		res.Success = true
	}
	// Deterministic order for JSON manifests.
	sort.SliceStable(res.Batches, func(a, b int) bool { return res.Batches[a].Index < res.Batches[b].Index })
	return res
}

// gatewayOptions builds AI Gateway providerOptions. --param values are keyed
// by the model's creator (openai/gpt-image-2 -> "openai"), which is where the
// Gateway forwards provider-specific settings.
func gatewayOptions(model string, opts RunOptions) map[string]any {
	params := provider.Merge(globalParams(opts.Params), scopedParams("gateway", opts.Params))
	if len(params) == 0 {
		return opts.ProviderOptions
	}
	creator, _, _ := strings.Cut(model, "/")
	return provider.Merge(opts.ProviderOptions, map[string]any{creator: params})
}

func addUsage(a, b *provider.Usage) *provider.Usage {
	if b == nil {
		return a
	}
	if a == nil {
		c := *b
		return &c
	}
	sum := func(x, y *int) *int {
		if x == nil {
			return y
		}
		if y == nil {
			return x
		}
		v := *x + *y
		return &v
	}
	return &provider.Usage{InputTokens: sum(a.InputTokens, b.InputTokens), OutputTokens: sum(a.OutputTokens, b.OutputTokens), TotalTokens: sum(a.TotalTokens, b.TotalTokens)}
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func check() string {
	if output.ColorEnabled() {
		return "\x1b[32m✓\x1b[0m"
	}
	return "✓"
}

func cross() string {
	if output.ColorEnabled() {
		return "\x1b[31m✗\x1b[0m"
	}
	return "✗"
}
