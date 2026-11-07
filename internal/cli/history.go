package cli

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
	"unicode"

	"github.com/spf13/cobra"

	"github.com/lassejlv/ai-cli/internal/auth"
	"github.com/lassejlv/ai-cli/internal/media"
	"github.com/lassejlv/ai-cli/internal/output"
	"github.com/lassejlv/ai-cli/internal/provider"
)

// History stores requests separately from credentials. Inline input images are
// retained so reruns do not depend on stdin or the original reference file.
type historyEntry struct {
	ID        string          `json:"id"`
	CreatedAt time.Time       `json:"createdAt"`
	Prompt    string          `json:"prompt"`
	Models    []ModelSpec     `json:"models"`
	Settings  historySettings `json:"settings"`
	Outputs   []historyOutput `json:"outputs"`
	Success   bool            `json:"success"`
}

type historySettings struct {
	N               int            `json:"n"`
	Size            string         `json:"size,omitempty"`
	AspectRatio     string         `json:"aspectRatio,omitempty"`
	Seed            *int64         `json:"seed,omitempty"`
	Images          []string       `json:"images,omitempty"`
	Mask            string         `json:"mask,omitempty"`
	Params          map[string]any `json:"params,omitempty"`
	ProviderOptions map[string]any `json:"providerOptions,omitempty"`
	MaxPerCall      int            `json:"maxImagesPerCall,omitempty"`
	Concurrency     int            `json:"concurrency"`
	Timeout         int            `json:"timeoutSeconds"`
	MaxRetries      int            `json:"maxRetries"`
	BaseURL         string         `json:"baseURL,omitempty"`
	Scope           string         `json:"scope,omitempty"`
	NeedsHeaders    bool           `json:"needsHeaders,omitempty"`
	NeedsBaseURL    bool           `json:"needsBaseURL,omitempty"`
}

type historyOutput struct {
	ModelIndex int    `json:"modelIndex"`
	File       string `json:"file"`
	MediaType  string `json:"mediaType"`
	Width      int    `json:"width,omitempty"`
	Height     int    `json:"height,omitempty"`
}

func historyDir() string { return filepath.Join(auth.ConfigDir(), "history") }

func historyReference(img media.Image) string {
	if img.IsRemote() {
		return img.URL
	}
	return "data:" + img.MediaType + ";base64," + base64.StdEncoding.EncodeToString(img.Data)
}

func saveHistory(models []preparedModel, opts RunOptions, result *RunResult) (string, error) {
	now := time.Now().UTC()
	var random [6]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	id := now.Format("20060102T150405Z") + "-" + hex.EncodeToString(random[:])
	e := historyEntry{ID: id, CreatedAt: now, Prompt: opts.Prompt, Success: result.Success,
		Settings: historySettings{
			N: opts.N, Size: opts.Size, AspectRatio: opts.AspectRatio, Seed: opts.Seed,
			Params: opts.Params, ProviderOptions: opts.ProviderOptions,
			MaxPerCall: opts.MaxPerCall, Concurrency: opts.Concurrency, Timeout: int(opts.Timeout.Seconds()),
			MaxRetries: opts.Overrides.MaxRetries, BaseURL: opts.Overrides.BaseURL, Scope: opts.Overrides.Scope,
			NeedsHeaders: len(opts.Overrides.Headers) > 0,
		}}
	// Connection URLs can contain credentials. Require those again instead of
	// writing their user info or query parameters to history.
	if u, err := url.Parse(e.Settings.BaseURL); err == nil && (u.User != nil || u.RawQuery != "" || u.Fragment != "") {
		e.Settings.BaseURL, e.Settings.NeedsBaseURL = "", true
	}
	for _, img := range opts.Images {
		e.Settings.Images = append(e.Settings.Images, historyReference(img))
	}
	if opts.Mask != nil {
		e.Settings.Mask = historyReference(*opts.Mask)
	}
	for i, m := range models {
		spec := m.spec
		// Bind saved accounts, but keep the implicit Codex CLI fallback implicit:
		// its synthetic account name is not a saved ai-cli account.
		if m.account.Source == "login" || (m.account.Source == "codex-cli" && m.account.Account != "codex-cli") {
			spec.Account = m.account.Account
		}
		e.Models = append(e.Models, spec)
		for _, img := range result.Results[i].Images {
			if img.File != nil {
				e.Outputs = append(e.Outputs, historyOutput{ModelIndex: i, File: *img.File, MediaType: img.MediaType, Width: img.Width, Height: img.Height})
			}
		}
	}
	data, err := json.MarshalIndent(e, "", "  ")
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(historyDir(), 0o700); err != nil {
		return "", err
	}
	// A separate, atomically published file per run avoids lost updates across
	// concurrent CLI processes and MCP requests.
	f, err := os.CreateTemp(historyDir(), ".run-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(f.Name())
	_, writeErr := f.Write(data)
	closeErr := f.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		return "", err
	}
	if err := os.Rename(f.Name(), filepath.Join(historyDir(), id+".json")); err != nil {
		return "", err
	}
	return id, nil
}

func readHistory(id string) (*historyEntry, error) {
	if id == "" || strings.ContainsAny(id, `/\`) || id == "." || id == ".." {
		return nil, fmt.Errorf("invalid history ID %q", id)
	}
	data, err := os.ReadFile(filepath.Join(historyDir(), id+".json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("history entry %q not found; use `ai history` to list runs", id)
	}
	if err != nil {
		return nil, err
	}
	var e historyEntry
	if err := json.Unmarshal(data, &e); err != nil {
		return nil, fmt.Errorf("could not read history entry %q: %w", id, err)
	}
	if e.ID != id || len(e.Models) == 0 || len(e.Outputs) == 0 {
		return nil, fmt.Errorf("invalid history entry %q", id)
	}
	for _, img := range e.Outputs {
		if img.ModelIndex < 0 || img.ModelIndex >= len(e.Models) {
			return nil, fmt.Errorf("invalid model index in history entry %q", id)
		}
	}
	return &e, nil
}

func listHistory(limit int) ([]historyEntry, error) {
	files, err := os.ReadDir(historyDir())
	if errors.Is(err, os.ErrNotExist) {
		return []historyEntry{}, nil
	}
	if err != nil {
		return nil, err
	}
	entries := []historyEntry{}
	for _, f := range files {
		if f.IsDir() || strings.HasPrefix(f.Name(), ".") || !strings.HasSuffix(f.Name(), ".json") {
			continue
		}
		e, err := readHistory(strings.TrimSuffix(f.Name(), ".json"))
		if err != nil {
			return nil, err
		}
		entries = append(entries, *e)
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].CreatedAt.Equal(entries[j].CreatedAt) {
			return entries[i].ID > entries[j].ID
		}
		return entries[i].CreatedAt.After(entries[j].CreatedAt)
	})
	if limit > 0 && len(entries) > limit {
		entries = entries[:limit]
	}
	return entries, nil
}

func newHistoryCmd() *cobra.Command {
	var asJSON bool
	var limit int
	cmd := &cobra.Command{
		Use: "history [id]", Short: "List saved generations, or inspect a run as JSON",
		Args:    cobra.MaximumNArgs(1),
		Example: "  ai history\n  ai history --json --limit 5\n  ai history <id>",
		RunE: func(cmd *cobra.Command, args []string) error {
			if limit < 1 {
				return historyError(asJSON, errors.New("--limit must be a positive integer"))
			}
			if len(args) > 0 {
				e, err := readHistory(args[0])
				if err != nil {
					return historyError(asJSON, err)
				}
				writeJSON(os.Stdout, e)
				return nil
			}
			entries, err := listHistory(limit)
			if err != nil {
				return historyError(asJSON, err)
			}
			if asJSON {
				writeJSON(os.Stdout, entries)
				return nil
			}
			if len(entries) == 0 {
				fmt.Fprintln(os.Stdout, "No saved generations yet. Run `ai image` with file output to start history.")
				return nil
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintln(w, "ID\tCREATED (UTC)\tMODELS\tIMAGES\tPROMPT")
			for _, e := range entries {
				var models []string
				for _, m := range e.Models {
					models = append(models, m.String())
				}
				prompt := []rune(strings.Map(func(r rune) rune {
					if unicode.IsControl(r) {
						return ' '
					}
					return r
				}, e.Prompt))
				if len(prompt) > 70 {
					prompt = append(prompt[:67], '.', '.', '.')
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%s\n", e.ID, e.CreatedAt.Format("2006-01-02 15:04:05"), strings.Join(models, ","), len(e.Outputs), string(prompt))
			}
			return w.Flush()
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "Write history as JSON")
	cmd.Flags().IntVar(&limit, "limit", 20, "Maximum number of runs to list, newest first")
	return cmd
}

func historyError(asJSON bool, err error) error {
	if asJSON {
		writeJSON(os.Stdout, map[string]any{"success": false, "error": ErrorDetail{Name: "HistoryError", Message: err.Error()}})
	}
	return usageErr("%v", err)
}

func newReplayCmd(edit bool) *cobra.Command {
	f := &imageFlags{}
	var last bool
	var from string
	var imageIndex int
	cmd := &cobra.Command{Use: "rerun [id]", Short: "Repeat a saved generation with its prompt and settings", Args: cobra.MaximumNArgs(1),
		Example: "  ai rerun --last\n  ai rerun <id> --seed 42\n  ai rerun --last --dry-run"}
	if edit {
		cmd.Use, cmd.Short, cmd.Args = "edit <prompt>", "Edit an image from a saved generation", cobra.ExactArgs(1)
		cmd.Example = "  ai edit --last \"make the fox younger\"\n  ai edit --from <id> --image-index 2 \"add snow\""
	}
	addImageFlags(cmd, f)
	// Replay takes its input from history, so these image-command flags do not
	// apply. Keep the explicit interactive error for callers passing it anyway.
	_ = cmd.Flags().MarkHidden("interactive")
	_ = cmd.Flags().MarkHidden("no-stdin")
	cmd.Flags().BoolVar(&last, "last", false, "Use the most recent run with saved images")
	if edit {
		cmd.Flags().StringVar(&from, "from", "", "History ID to edit (instead of --last)")
		cmd.Flags().IntVar(&imageIndex, "image-index", 1, "Saved image to edit, numbered from 1")
	}
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		if !edit && len(args) > 0 {
			from = args[0]
		}
		if last == (from != "") {
			return historyError(f.json, errors.New("choose either --last or a history ID"))
		}
		if f.interactive {
			return historyError(f.json, errors.New("history replay does not support --interactive"))
		}
		if last {
			entries, err := listHistory(1)
			if err != nil {
				return historyError(f.json, err)
			}
			if len(entries) == 0 {
				return historyError(f.json, errors.New("no saved generations yet; run `ai image` with file output first"))
			}
			from = entries[0].ID
		}
		e, err := readHistory(from)
		if err != nil {
			return historyError(f.json, err)
		}
		prompt, err := restoreHistory(cmd, f, e, edit, imageIndex, args)
		if err != nil {
			return historyError(f.json, err)
		}
		opts, specs, err := buildRun(cmd.Context(), f, prompt, nil)
		if err != nil {
			return historyError(f.json, err)
		}
		opts.Params = provider.Merge(e.Settings.Params, opts.Params)
		if !cmd.Flags().Changed("provider-options") {
			opts.ProviderOptions = e.Settings.ProviderOptions
		}
		opts.StdoutIsTTY = output.IsTTY(os.Stdout)
		return executeImage(cmd, f, opts, specs)
	}
	return cmd
}

func restoreHistory(cmd *cobra.Command, f *imageFlags, e *historyEntry, edit bool, imageIndex int, args []string) (string, error) {
	s := e.Settings
	models := e.Models
	prompt := e.Prompt
	if edit {
		if imageIndex < 1 || imageIndex > len(e.Outputs) {
			return "", fmt.Errorf("--image-index must be between 1 and %d", len(e.Outputs))
		}
		img := e.Outputs[imageIndex-1]
		if st, err := os.Stat(img.File); err != nil || st.IsDir() {
			return "", fmt.Errorf("saved image %q is missing or unreadable; restore the file or choose another history entry", img.File)
		}
		if strings.TrimSpace(args[0]) == "" {
			return "", errors.New("an edit prompt is required")
		}
		prompt, models = strings.TrimSpace(args[0]), []ModelSpec{e.Models[img.ModelIndex]}
		// Edits use the selected output, not the original run's references/mask.
		f.images = append([]string{img.File}, f.images...)
	} else {
		if !cmd.Flags().Changed("images") {
			f.images = s.Images
		}
		if !cmd.Flags().Changed("mask") {
			f.mask = s.Mask
		}
	}
	if !cmd.Flags().Changed("model") {
		var specs []string
		for _, m := range models {
			if cmd.Flags().Changed("provider") {
				m.Provider, m.Account = f.providerName, ""
			}
			if cmd.Flags().Changed("account") {
				m.Account = f.account
			}
			specs = append(specs, m.String())
		}
		f.model = strings.Join(specs, ",")
	}
	// Connection defaults belong to the original providers. Do not send a
	// different provider's credentials to a saved custom endpoint.
	defProvider, err := defaultProvider(f.providerName)
	if err != nil {
		return "", err
	}
	savedProviders := map[string]bool{}
	for _, m := range e.Models {
		savedProviders[m.Provider] = true
	}
	restoreConnection := true
	for _, raw := range strings.Split(f.model, ",") {
		m, err := ParseModelSpec(raw, defProvider, f.account)
		if err != nil {
			return "", err
		}
		if !savedProviders[m.Provider] {
			restoreConnection = false
		}
	}
	if restoreConnection {
		if s.NeedsHeaders && !cmd.Flags().Changed("headers") {
			return "", errors.New("this run used custom HTTP headers; pass --headers again (headers are not saved in history)")
		}
		if s.NeedsBaseURL && !cmd.Flags().Changed("base-url") {
			return "", errors.New("this run used a URL containing credentials or query parameters; pass --base-url again")
		}
	} else {
		s.BaseURL, s.Scope = "", ""
	}
	// Restore only generation settings. Output targets/modes come from this
	// invocation, so a rerun never implicitly replaces the previous image.
	defaults := map[string]string{"n": strconv.Itoa(s.N), "size": s.Size, "aspect-ratio": s.AspectRatio,
		"max-images-per-call": strconv.Itoa(s.MaxPerCall), "concurrency": strconv.Itoa(s.Concurrency),
		"timeout": strconv.Itoa(max(s.Timeout, 1)), "max-retries": strconv.Itoa(s.MaxRetries), "base-url": s.BaseURL, "scope": s.Scope}
	if edit {
		defaults["n"] = "1"
	}
	if s.Seed != nil {
		defaults["seed"] = strconv.FormatInt(*s.Seed, 10)
	}
	for name, value := range defaults {
		if !cmd.Flags().Changed(name) {
			if err := cmd.Flags().Lookup(name).Value.Set(value); err != nil {
				return "", err
			}
		}
	}
	if !cmd.Flags().Changed("output") {
		// Use a fresh filename even if the provider repeats a response ID.
		index := 0
		if edit {
			index = imageIndex - 1
		}
		var random [6]byte
		if _, err := rand.Read(random[:]); err != nil {
			return "", err
		}
		f.outputPath = filepath.Join(filepath.Dir(e.Outputs[index].File), cmd.Name()+"-"+hex.EncodeToString(random[:])+media.Extension(e.Outputs[index].MediaType))
	}
	return prompt, nil
}
