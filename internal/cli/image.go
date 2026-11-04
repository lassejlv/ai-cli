package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/lassejlv/ai-cli/internal/media"
	"github.com/lassejlv/ai-cli/internal/output"
	"github.com/lassejlv/ai-cli/internal/provider"
)

const (
	defaultImageTimeout     = 300
	defaultImageConcurrency = 4
)

type imageFlags struct {
	model           string
	providerName    string
	account         string
	outputPath      string
	n               int
	images          []string
	mask            string
	size            string
	aspectRatio     string
	seed            string
	providerOptions string
	params          []string
	headers         string
	maxRetries      int
	baseURL         string
	scope           string
	teamIDOrSlug    string
	maxPerCall      int
	concurrency     int
	timeout         int
	quiet           bool
	json            bool
	noPreview       bool
	raw             bool
	base64          bool
	dataURL         bool
	noSave          bool
	dryRun          bool
	interactive     bool
	noStdin         bool
}

func newImageCmd() *cobra.Command {
	f := &imageFlags{}
	cmd := &cobra.Command{
		Use:   "image [prompt]",
		Short: "Generate or edit images",
		Long: `Generate or edit images with any configured provider.

Models are written provider[@account]:model, for example openai:gpt-image-2,
openai@work:gpt-image-1, higgsfield:soul or fal:fal-ai/flux/dev. Unprefixed
models use the default provider (--provider, AI_CLI_IMAGE_PROVIDER,
` + "`ai providers default`" + `, or the first configured provider).

Run without a prompt in a terminal to open the interactive TUI.`,
		Example: `  ai image "a cute dog"
  ai image -m openai:gpt-image-2 --param quality=high "a lighthouse at dusk"
  ai image -m higgsfield:soul --aspect-ratio 16:9 "editorial portrait"
  ai image -m "codex:gpt-image-2,grok:grok-imagine-image" "a red bicycle"
  ai image -i sketch.png "render this as a product photo"
  ai image --base64 "an icon" > icon.b64
  ai image --json -o ./renders/ -n 3 "a sunset"`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runImage(cmd, f, args)
		},
	}
	fl := cmd.Flags()
	fl.StringVarP(&f.model, "model", "m", "", "Model ID, provider[@account]:model; comma-separated for multiple models")
	fl.StringVar(&f.providerName, "provider", "", "Provider for unprefixed model IDs")
	fl.StringVar(&f.account, "account", "", "Saved account for unprefixed model IDs (see `ai accounts`)")
	fl.StringVarP(&f.outputPath, "output", "o", "", "Output file or directory (use a trailing / for a new directory)")
	fl.IntVarP(&f.n, "n", "n", 1, "Number of images per model")
	fl.StringArrayVarP(&f.images, "images", "i", nil, "Reference image path, URL or data URL (repeatable)")
	fl.StringVar(&f.mask, "mask", "", "Mask image for editing (requires a reference image)")
	fl.StringVar(&f.size, "size", "", "Image size WxH, e.g. 1024x1024")
	fl.StringVar(&f.aspectRatio, "aspect-ratio", "", "Aspect ratio W:H, e.g. 16:9")
	fl.StringVar(&f.seed, "seed", "", "Generation seed, including 0")
	fl.StringVar(&f.providerOptions, "provider-options", "", "JSON file of options keyed by provider name")
	fl.StringArrayVar(&f.params, "param", nil, "Provider option key=value (repeatable; dotted keys nest, values parse as JSON, provider:key scopes to one provider)")
	fl.StringVar(&f.headers, "headers", "", "JSON file of extra HTTP headers")
	fl.IntVar(&f.maxRetries, "max-retries", 2, "Retries for transient errors (0 disables)")
	fl.StringVar(&f.baseURL, "base-url", "", "Override the provider API base URL")
	fl.StringVar(&f.scope, "scope", "", "AI Gateway team ID or slug")
	fl.StringVar(&f.teamIDOrSlug, "team-id-or-slug", "", "Deprecated: use --scope")
	_ = fl.MarkHidden("team-id-or-slug")
	fl.IntVar(&f.maxPerCall, "max-images-per-call", 0, "Batching limit per provider call (default: provider limit)")
	fl.IntVarP(&f.concurrency, "concurrency", "p", defaultImageConcurrency, "Parallel models and batches")
	fl.IntVar(&f.timeout, "timeout", defaultImageTimeout, "Per-request timeout in seconds")
	fl.BoolVarP(&f.quiet, "quiet", "q", false, "Suppress progress; errors remain visible")
	fl.BoolVar(&f.json, "json", false, "Write a JSON manifest to stdout")
	fl.BoolVar(&f.noPreview, "no-preview", false, "Disable inline Kitty graphics previews")
	fl.BoolVar(&f.raw, "raw", false, "Write raw image bytes to stdout (single image)")
	fl.BoolVar(&f.base64, "base64", false, "Print each image as base64 on its own stdout line (with --json: embed in the manifest)")
	fl.BoolVar(&f.dataURL, "data-url", false, "Print each image as a data: URL on its own stdout line")
	fl.BoolVar(&f.noSave, "no-save", false, "Do not write image files (preview or stdout only)")
	fl.BoolVar(&f.dryRun, "dry-run", false, "Print the resolved request as JSON without calling any provider")
	fl.BoolVarP(&f.interactive, "interactive", "I", false, "Open the interactive TUI, prefilled with these flags")
	fl.BoolVar(&f.noStdin, "no-stdin", false, "Do not read a prompt or reference image from stdin (also AI_CLI_NO_STDIN=1)")
	return cmd
}

func runImage(cmd *cobra.Command, f *imageFlags, args []string) error {
	prompt := ""
	if len(args) > 0 {
		prompt = strings.TrimSpace(args[0])
	}
	stdinTTY := output.IsTTY(os.Stdin)
	stdoutTTY := output.IsTTY(os.Stdout)

	var stdinImage *media.Image
	if !stdinTTY && !f.interactive && !f.noStdin && os.Getenv("AI_CLI_NO_STDIN") == "" {
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			return usageErr("could not read stdin: %v", err)
		}
		if len(data) > 0 {
			if media.IsImage(data) {
				img := media.FromBytes(data)
				stdinImage = &img
			} else if text := strings.TrimSpace(string(data)); text != "" {
				if prompt == "" {
					prompt = text
				} else {
					prompt = text + "\n\n" + prompt
				}
			}
		}
	}

	if f.interactive || (prompt == "" && stdinImage == nil && len(f.images) == 0 && stdinTTY && stdoutTTY && !f.json && !f.dryRun && os.Getenv("AI_CLI_NO_TUI") == "") {
		return runTUI(cmd.Context(), tuiDefaults{flags: f, prompt: prompt})
	}

	opts, specs, err := buildRun(cmd.Context(), f, prompt, stdinImage)
	if err != nil {
		if f.json {
			writeJSON(os.Stdout, map[string]any{"success": false, "error": ErrorDetail{Name: "UsageError", Message: err.Error()}})
		}
		return usageErr("%v", err)
	}
	opts.StdoutIsTTY = stdoutTTY

	if f.dryRun {
		return writeDryRun(specs, opts)
	}

	ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	result := Run(ctx, specs, opts)
	if opts.JSON {
		writeJSON(os.Stdout, result)
	}
	if code := result.ExitCode(); code != 0 {
		return &exitError{code: code}
	}
	return nil
}

// buildRun validates flags into RunOptions.
func buildRun(ctx context.Context, f *imageFlags, prompt string, stdinImage *media.Image) (RunOptions, []ModelSpec, error) {
	opts := RunOptions{
		Prompt:      prompt,
		N:           f.n,
		MaxPerCall:  f.maxPerCall,
		Concurrency: f.concurrency,
		Timeout:     time.Duration(f.timeout) * time.Second,
		Output:      f.outputPath,
		Mode:        ModeFile,
		NoSave:      f.noSave,
		JSON:        f.json,
		Quiet:       f.quiet,
		Preview:     !f.noPreview,
		Stdout:      os.Stdout,
	}
	if f.n < 1 {
		return opts, nil, errors.New("--n must be a positive integer")
	}
	if f.concurrency < 1 {
		return opts, nil, errors.New("--concurrency must be a positive integer")
	}
	if f.timeout < 1 {
		return opts, nil, errors.New("--timeout must be a positive number of seconds")
	}
	if f.maxRetries < 0 {
		return opts, nil, errors.New("--max-retries must be a non-negative integer")
	}
	if f.maxPerCall < 0 {
		return opts, nil, errors.New("--max-images-per-call must be a positive integer")
	}
	modes := 0
	for _, on := range []bool{f.raw, f.base64, f.dataURL} {
		if on {
			modes++
		}
	}
	if modes > 1 {
		return opts, nil, errors.New("--raw, --base64 and --data-url are mutually exclusive")
	}
	switch {
	case f.raw:
		opts.Mode = ModeRaw
		if f.json {
			return opts, nil, errors.New("--raw cannot be combined with --json; use --json --base64")
		}
	case f.base64:
		opts.Mode = ModeBase64
	case f.dataURL:
		opts.Mode = ModeDataURL
	}
	if f.size != "" && !regexp.MustCompile(`^[1-9]\d*x[1-9]\d*$`).MatchString(f.size) {
		return opts, nil, fmt.Errorf("--size must be in WxH format (e.g. 1024x1024), got %q", f.size)
	}
	opts.Size = f.size
	if f.aspectRatio != "" && !regexp.MustCompile(`^\d+(\.\d+)?:\d+(\.\d+)?$`).MatchString(f.aspectRatio) {
		return opts, nil, fmt.Errorf("--aspect-ratio must be in W:H format (e.g. 16:9), got %q", f.aspectRatio)
	}
	opts.AspectRatio = f.aspectRatio
	if f.seed != "" {
		seed, err := strconv.ParseInt(f.seed, 10, 64)
		if err != nil {
			return opts, nil, fmt.Errorf("--seed must be an integer, got %q", f.seed)
		}
		opts.Seed = &seed
	}

	if stdinImage != nil {
		opts.Images = append(opts.Images, *stdinImage)
	}
	refs, err := media.LoadAll(f.images, "images")
	if err != nil {
		return opts, nil, err
	}
	opts.Images = append(opts.Images, refs...)
	if f.mask != "" {
		mask, err := media.Load(f.mask, "mask")
		if err != nil {
			return opts, nil, err
		}
		if len(opts.Images) == 0 {
			return opts, nil, errors.New("--mask requires --images or piped image input")
		}
		opts.Mask = &mask
	}
	if opts.Prompt == "" && len(opts.Images) == 0 {
		return opts, nil, errors.New("a prompt or reference image is required (pass a prompt, use --images, or pipe an image via stdin)")
	}

	if f.providerOptions != "" {
		raw, err := readJSONObject(f.providerOptions, "provider-options")
		if err != nil {
			return opts, nil, err
		}
		for k, v := range raw {
			if _, ok := v.(map[string]any); !ok {
				return opts, nil, fmt.Errorf("--provider-options must map provider names to JSON objects (%q is not an object)", k)
			}
		}
		opts.ProviderOptions = raw
	}
	params, err := parseParams(f.params)
	if err != nil {
		return opts, nil, err
	}
	opts.Params = params

	headers := map[string]string{}
	if f.headers != "" {
		raw, err := readJSONObject(f.headers, "headers")
		if err != nil {
			return opts, nil, err
		}
		for k, v := range raw {
			s, ok := v.(string)
			if !ok {
				return opts, nil, errors.New("--headers values must be strings")
			}
			headers[k] = s
		}
	}
	scope := f.scope
	if scope == "" {
		scope = f.teamIDOrSlug
	}
	if f.baseURL != "" && !regexp.MustCompile(`^https?://`).MatchString(f.baseURL) {
		return opts, nil, errors.New("--base-url must be an HTTP(S) URL")
	}
	opts.Overrides = Overrides{BaseURL: f.baseURL, Scope: scope, Headers: headers, MaxRetries: f.maxRetries}

	specs, err := ResolveModelSpecs(ctx, f.model, f.providerName, f.account)
	if err != nil {
		return opts, nil, err
	}
	if opts.Mode == ModeRaw && (len(specs) > 1 || f.n > 1) {
		return opts, nil, errors.New("--raw writes a single image; use --base64 or --data-url for multiple images")
	}
	return opts, specs, nil
}

// parseParams turns key=value pairs into a nested options object. A
// "provider:" prefix (higgsfield:enhance_prompt=true) scopes a value to one
// provider; scoped values are returned under the key "provider:<name>".
func parseParams(pairs []string) (map[string]any, error) {
	out := map[string]any{}
	for _, pair := range pairs {
		key, value, ok := strings.Cut(pair, "=")
		key = strings.TrimSpace(key)
		if !ok || key == "" {
			return nil, fmt.Errorf("--param must be key=value, got %q", pair)
		}
		root := out
		if scope, rest, scoped := strings.Cut(key, ":"); scoped {
			if _, known := provider.Lookup(scope); !known {
				return nil, fmt.Errorf("--param %q: unknown provider %q", pair, scope)
			}
			bucket, _ := out["provider:"+strings.ToLower(scope)].(map[string]any)
			if bucket == nil {
				bucket = map[string]any{}
				out["provider:"+strings.ToLower(scope)] = bucket
			}
			root, key = bucket, rest
		}
		var parsed any = value
		var decoded any
		if err := json.Unmarshal([]byte(value), &decoded); err == nil {
			parsed = decoded
		}
		parts := strings.Split(key, ".")
		node := root
		for _, part := range parts[:len(parts)-1] {
			child, ok := node[part].(map[string]any)
			if !ok {
				child = map[string]any{}
				node[part] = child
			}
			node = child
		}
		node[parts[len(parts)-1]] = parsed
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

func readJSONObject(path, name string) (map[string]any, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("--%s: could not read %s: %v", name, path, err)
	}
	var v map[string]any
	if err := json.Unmarshal(data, &v); err != nil {
		return nil, fmt.Errorf("--%s: %s must contain a JSON object: %v", name, path, err)
	}
	return v, nil
}

func writeDryRun(specs []ModelSpec, opts RunOptions) error {
	type plannedModel struct {
		ModelSpec
		Spec        string          `json:"spec"`
		Credentials ResolvedAccount `json:"credentials"`
		Options     map[string]any  `json:"options,omitempty"`
		Error       string          `json:"error,omitempty"`
	}
	var planned []plannedModel
	for _, m := range prepare(specs, opts.Overrides) {
		p := plannedModel{ModelSpec: m.spec, Spec: m.spec.String(), Credentials: m.account, Options: providerOptionsFor(m.spec.Provider, opts)}
		if m.err != nil {
			p.Error = m.err.Error()
		}
		planned = append(planned, p)
	}
	refs := make([]map[string]any, 0, len(opts.Images))
	for _, img := range opts.Images {
		if img.IsRemote() {
			refs = append(refs, map[string]any{"url": img.URL})
		} else {
			refs = append(refs, map[string]any{"mediaType": img.MediaType, "bytes": len(img.Data)})
		}
	}
	writeJSON(os.Stdout, map[string]any{
		"dryRun":      true,
		"prompt":      opts.Prompt,
		"models":      planned,
		"n":           opts.N,
		"size":        opts.Size,
		"aspectRatio": opts.AspectRatio,
		"seed":        opts.Seed,
		"images":      refs,
		"mask":        opts.Mask != nil,
		"output":      opts.Output,
		"mode":        opts.Mode,
		"timeoutSec":  int(opts.Timeout.Seconds()),
	})
	for _, p := range planned {
		if p.Error != "" {
			return &exitError{code: 1}
		}
	}
	return nil
}

func writeJSON(w io.Writer, v any) {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}
