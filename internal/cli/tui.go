package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/huh"
	"github.com/charmbracelet/lipgloss"
	"github.com/spf13/cobra"

	"github.com/lassejlv/ai-cli/internal/auth"
	"github.com/lassejlv/ai-cli/internal/media"
	"github.com/lassejlv/ai-cli/internal/output"
	"github.com/lassejlv/ai-cli/internal/provider"
)

const customModel = "__custom__"

type tuiDefaults struct {
	flags  *imageFlags
	prompt string
}

// tuiState holds every choice the user makes; forms bind to these fields.
type tuiState struct {
	provider    string
	account     string
	model       string
	customModel string
	prompt      string
	aspect      string
	size        string
	n           string
	seed        string
	images      string
	output      string
	params      string
	next        string
}

var (
	titleStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#FAFAFA")).Background(lipgloss.Color("#7D56F4")).Padding(0, 1)
	dimStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("244"))
	okStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("42"))
)

func newTUICmd() *cobra.Command {
	f := &imageFlags{n: 1, concurrency: defaultImageConcurrency, timeout: defaultImageTimeout, maxRetries: 2}
	cmd := &cobra.Command{
		Use:     "tui",
		Aliases: []string{"interactive"},
		Short:   "Open the interactive image studio",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if !output.IsTTY(os.Stdin) || !output.IsTTY(os.Stdout) {
				return usageErr("the TUI needs an interactive terminal; use `ai image` with flags instead")
			}
			return runTUI(cmd.Context(), tuiDefaults{flags: f})
		},
	}
	cmd.Flags().StringVarP(&f.model, "model", "m", "", "Preselect a model (provider[@account]:model)")
	cmd.Flags().StringVarP(&f.outputPath, "output", "o", "", "Output directory")
	return cmd
}

func theme() *huh.Theme {
	t := huh.ThemeCharm()
	return t
}

func runTUI(ctx context.Context, d tuiDefaults) error {
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt)
	defer stop()
	s := initialState(ctx, d)
	fmt.Fprintln(os.Stderr, titleStyle.Render("ai · image studio")+"  "+dimStyle.Render("esc/ctrl+c to quit"))

	if err := chooseProvider(ctx, s); err != nil {
		return quietAbort(err)
	}
	if err := composeForm(s, true); err != nil {
		return quietAbort(err)
	}
	for {
		result, err := generateFromState(ctx, s, d.flags)
		if err != nil {
			fmt.Fprintln(os.Stderr, lipgloss.NewStyle().Foreground(lipgloss.Color("203")).Render("Error: "+err.Error()))
		}
		action := "again"
		actions := []huh.Option[string]{
			huh.NewOption("Generate again", "again"),
			huh.NewOption("Edit prompt", "prompt"),
			huh.NewOption("Change settings", "settings"),
			huh.NewOption("Switch provider / model", "provider"),
		}
		if result != nil {
			if file := firstFile(result); file != "" {
				actions = append(actions, huh.NewOption("Edit this result (use it as reference)", "reference"))
			}
		}
		actions = append(actions, huh.NewOption("Quit", "quit"))
		if err := huh.NewForm(huh.NewGroup(
			huh.NewSelect[string]().Title("What next?").Options(actions...).Value(&action),
		)).WithTheme(theme()).RunWithContext(ctx); err != nil {
			return quietAbort(err)
		}
		switch action {
		case "quit":
			return nil
		case "prompt":
			err = composeForm(s, false)
		case "settings":
			err = settingsForm(s)
		case "provider":
			if err = chooseProvider(ctx, s); err == nil {
				err = composeForm(s, false)
			}
		case "reference":
			s.images = firstFile(result)
			s.prompt = ""
			err = composeForm(s, false)
		}
		if err != nil {
			return quietAbort(err)
		}
	}
}

func quietAbort(err error) error {
	if errors.Is(err, huh.ErrUserAborted) || errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

func initialState(ctx context.Context, d tuiDefaults) *tuiState {
	f := d.flags
	s := &tuiState{prompt: d.prompt, aspect: f.aspectRatio, size: f.size, n: strconv.Itoa(max(f.n, 1)), seed: f.seed,
		images: strings.Join(f.images, ", "), output: f.outputPath, params: strings.Join(f.params, " "), account: f.account}
	if s.output == "" {
		s.output = os.Getenv("AI_CLI_OUTPUT_DIR")
	}
	if f.model != "" {
		if specs, err := ResolveModelSpecs(ctx, f.model, f.providerName, f.account); err == nil {
			s.provider, s.account, s.model = specs[0].Provider, specs[0].Account, specs[0].Model
		}
	}
	if s.provider == "" {
		s.provider, _ = defaultProvider(f.providerName)
	}
	return s
}

func providerOptions() []huh.Option[string] {
	var ready, other []huh.Option[string]
	for _, r := range listProviders() {
		label := fmt.Sprintf("%-11s %s", r.Name, r.Title)
		if r.Configured {
			label = okStyle.Render("●") + " " + label
			if len(r.Accounts) > 1 {
				label += dimStyle.Render(fmt.Sprintf("  (%d accounts)", len(r.Accounts)))
			}
			ready = append(ready, huh.NewOption(label, r.Name))
		} else {
			label = dimStyle.Render("○ "+label+"  — sign in") + ""
			other = append(other, huh.NewOption(label, r.Name))
		}
	}
	return append(ready, other...)
}

func chooseProvider(ctx context.Context, s *tuiState) error {
	prev := s.provider
	if err := huh.NewForm(huh.NewGroup(
		huh.NewSelect[string]().Title("Provider").Description("● ready   ○ needs sign-in").Options(providerOptions()...).Value(&s.provider).Height(14),
	)).WithTheme(theme()).RunWithContext(ctx); err != nil {
		return err
	}
	if s.provider != prev {
		s.model, s.account = "", ""
	}
	info, _ := provider.Lookup(s.provider)
	if !isConfigured(info) {
		login := true
		if err := huh.NewForm(huh.NewGroup(
			huh.NewConfirm().Title(fmt.Sprintf("You are not signed in to %s. Sign in now?", info.Title)).Value(&login),
		)).WithTheme(theme()).RunWithContext(ctx); err != nil {
			return err
		}
		if !login {
			return chooseProvider(ctx, s)
		}
		lf := &loginFlags{}
		if info.Name == "codex" {
			if _, err := auth.ReadCodexCLIAuth(); err == nil {
				useCLI := true
				_ = huh.NewForm(huh.NewGroup(huh.NewConfirm().Title("Import the existing Codex CLI login from ~/.codex/auth.json?").Value(&useCLI))).WithTheme(theme()).RunWithContext(ctx)
				lf.importCLI = useCLI
			}
		}
		if err := runLogin(ctx, info, lf); err != nil {
			fmt.Fprintln(os.Stderr, "Sign-in failed: "+err.Error())
			return chooseProvider(ctx, s)
		}
	}
	f, _ := auth.Load()
	if f != nil {
		if names := f.AccountNames(s.provider); len(names) > 1 {
			opts := make([]huh.Option[string], 0, len(names))
			for _, n := range names {
				_, cred := n, f.Providers[s.provider].Accounts[n]
				label := n
				if d := credentialDetail(cred); d != "" {
					label += dimStyle.Render("  " + d)
				}
				opts = append(opts, huh.NewOption(label, n))
			}
			if s.account == "" {
				s.account = f.Providers[s.provider].DefaultName()
			}
			if err := huh.NewForm(huh.NewGroup(
				huh.NewSelect[string]().Title("Account").Options(opts...).Value(&s.account),
			)).WithTheme(theme()).RunWithContext(ctx); err != nil {
				return err
			}
		}
	}
	return nil
}

func modelOptions(s *tuiState) []huh.Option[string] {
	rows := listModels(context.Background(), s.provider, s.provider == "gateway")
	opts := make([]huh.Option[string], 0, len(rows)+1)
	found := false
	for _, r := range rows {
		label := r.ID
		if r.Default {
			label += dimStyle.Render("  default")
		}
		if r.Description != "" {
			label += dimStyle.Render("  " + r.Description)
		}
		opts = append(opts, huh.NewOption(label, r.ID))
		if r.ID == s.model {
			found = true
		}
	}
	if s.model != "" && s.model != customModel && !found {
		s.customModel = s.model
		s.model = customModel
	}
	if s.model == "" {
		info, _ := provider.Lookup(s.provider)
		s.model = info.DefaultModel
	}
	return append(opts, huh.NewOption("Custom model ID…", customModel))
}

func composeForm(s *tuiState, first bool) error {
	s.next = "generate"
	nextOpts := []huh.Option[string]{huh.NewOption("Generate", "generate"), huh.NewOption("Adjust settings first", "settings")}
	if err := huh.NewForm(
		huh.NewGroup(
			huh.NewSelect[string]().Title("Model").Options(modelOptions(s)...).Value(&s.model).Height(12),
		),
		huh.NewGroup(
			huh.NewInput().Title("Model ID").Description("Any model the provider accepts").Value(&s.customModel).
				Validate(func(v string) error {
					if strings.TrimSpace(v) == "" {
						return errors.New("enter a model ID")
					}
					return nil
				}),
		).WithHideFunc(func() bool { return s.model != customModel }),
		huh.NewGroup(
			huh.NewText().Title("Prompt").Description(promptHint(s)).Value(&s.prompt).CharLimit(8000).Lines(5).
				Validate(func(v string) error {
					if strings.TrimSpace(v) == "" && strings.TrimSpace(s.images) == "" {
						return errors.New("describe the image you want")
					}
					return nil
				}),
			huh.NewSelect[string]().Title("Next").Options(nextOpts...).Value(&s.next).Inline(true),
		),
	).WithTheme(theme()).Run(); err != nil {
		return err
	}
	if s.next == "settings" {
		return settingsForm(s)
	}
	return nil
}

func promptHint(s *tuiState) string {
	hint := "alt+enter / ctrl+j for a new line"
	if strings.TrimSpace(s.images) != "" {
		hint = "Editing " + s.images + " · " + hint
	}
	return hint
}

var aspectChoices = []string{"", "1:1", "16:9", "9:16", "4:3", "3:4", "3:2", "2:3", "21:9"}

func settingsForm(s *tuiState) error {
	aspects := make([]huh.Option[string], 0, len(aspectChoices))
	for _, a := range aspectChoices {
		label := a
		if a == "" {
			label = "provider default"
		}
		aspects = append(aspects, huh.NewOption(label, a))
	}
	return huh.NewForm(
		huh.NewGroup(
			huh.NewSelect[string]().Title("Aspect ratio").Options(aspects...).Value(&s.aspect).Inline(true),
			huh.NewInput().Title("Size").Description("WxH, e.g. 1024x1536 (optional)").Value(&s.size).Validate(optionalMatch(`^[1-9]\d*x[1-9]\d*$`, "use WxH, e.g. 1024x1024")),
			huh.NewInput().Title("Images").Description("How many images to generate").Value(&s.n).Validate(func(v string) error {
				n, err := strconv.Atoi(strings.TrimSpace(v))
				if err != nil || n < 1 || n > 16 {
					return errors.New("enter a number from 1 to 16")
				}
				return nil
			}),
			huh.NewInput().Title("Seed").Description("Optional integer").Value(&s.seed).Validate(optionalMatch(`^-?\d+$`, "enter an integer")),
		),
		huh.NewGroup(
			huh.NewInput().Title("Reference images").Description("Comma-separated paths or URLs (optional)").Value(&s.images).Validate(func(v string) error {
				for _, ref := range splitList(v) {
					if strings.Contains(ref, "://") || strings.HasPrefix(ref, "data:") {
						continue
					}
					if _, err := os.Stat(expandHome(ref)); err != nil {
						return fmt.Errorf("%s: %v", ref, err)
					}
				}
				return nil
			}),
			huh.NewInput().Title("Provider options").Description("Space-separated key=value, e.g. quality=high background=transparent").Value(&s.params).Validate(func(v string) error {
				_, err := parseParams(strings.Fields(v))
				return err
			}),
			huh.NewInput().Title("Output directory").Description("Empty saves in the current directory").Value(&s.output),
		),
	).WithTheme(theme()).Run()
}

func optionalMatch(pattern, msg string) func(string) error {
	re := regexp.MustCompile(pattern)
	return func(v string) error {
		if v = strings.TrimSpace(v); v != "" && !re.MatchString(v) {
			return errors.New(msg)
		}
		return nil
	}
}

func splitList(v string) []string {
	var out []string
	for _, part := range strings.Split(v, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func expandHome(p string) string {
	if strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, p[2:])
		}
	}
	return p
}

func generateFromState(ctx context.Context, s *tuiState, base *imageFlags) (*RunResult, error) {
	model := s.model
	if model == customModel {
		model = strings.TrimSpace(s.customModel)
	}
	spec := ModelSpec{Provider: s.provider, Account: s.account, Model: model}
	info, _ := provider.Lookup(s.provider)
	spec.Model = info.ResolveModel(spec.Model)
	if spec.Provider == "gateway" {
		spec.Model = expandGatewayModel(ctx, spec.Model)
	}
	n, _ := strconv.Atoi(strings.TrimSpace(s.n))
	opts := RunOptions{
		Prompt:      strings.TrimSpace(s.prompt),
		N:           max(n, 1),
		Size:        strings.TrimSpace(s.size),
		AspectRatio: s.aspect,
		Concurrency: max(base.concurrency, 1),
		Timeout:     time.Duration(max(base.timeout, 1)) * time.Second,
		Output:      expandHome(strings.TrimSpace(s.output)),
		Mode:        ModeFile,
		Preview:     !base.noPreview,
		Stdout:      os.Stdout,
		StdoutIsTTY: true,
		Overrides:   Overrides{MaxRetries: base.maxRetries, BaseURL: base.baseURL, Scope: base.scope},
	}
	if opts.Output != "" && !strings.HasSuffix(opts.Output, "/") && filepath.Ext(opts.Output) == "" {
		opts.Output += "/"
	}
	if v := strings.TrimSpace(s.seed); v != "" {
		seed, _ := strconv.ParseInt(v, 10, 64)
		opts.Seed = &seed
	}
	refs, err := media.LoadAll(mapStrings(splitList(s.images), expandHome), "images")
	if err != nil {
		return nil, err
	}
	opts.Images = refs
	if opts.Params, err = parseParams(strings.Fields(s.params)); err != nil {
		return nil, err
	}
	fmt.Fprintln(os.Stderr)
	result := Run(ctx, []ModelSpec{spec}, opts)
	fmt.Fprintln(os.Stderr)
	return result, nil
}

func mapStrings(in []string, fn func(string) string) []string {
	out := make([]string, len(in))
	for i, v := range in {
		out[i] = fn(v)
	}
	return out
}

func firstFile(r *RunResult) string {
	if r == nil {
		return ""
	}
	for _, res := range r.Results {
		for _, img := range res.Images {
			if img.File != nil {
				return *img.File
			}
		}
	}
	return ""
}

// pickProvider asks for a provider (used by `ai login` without arguments).
func pickProvider(title string) (string, error) {
	choice := "gateway"
	err := huh.NewForm(huh.NewGroup(
		huh.NewSelect[string]().Title(title).Options(providerOptions()...).Value(&choice).Height(14),
	)).WithTheme(theme()).Run()
	if errors.Is(err, huh.ErrUserAborted) {
		return "", &exitError{code: 130}
	}
	return choice, err
}
