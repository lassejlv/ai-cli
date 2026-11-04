package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/lassejlv/ai-cli/internal/auth"
	"github.com/lassejlv/ai-cli/internal/output"
	"github.com/lassejlv/ai-cli/internal/provider"
)

type loginFlags struct {
	account   string
	noBrowser bool
	device    bool
	importCLI bool
	baseURL   string
	apiKey    string
	apiSecret string
	makeDef   bool
	noVerify  bool
	jsonOut   bool
}

func newLoginCmd() *cobra.Command {
	f := &loginFlags{}
	cmd := &cobra.Command{
		Use:   "login [provider]",
		Short: "Save credentials for a provider (default: gateway)",
		Long: `Save credentials for an image provider. Without a provider in a terminal,
a picker opens; otherwise the AI Gateway is used.

API-key providers prompt for the key, read it from stdin, or take --api-key.
codex signs in with your ChatGPT subscription (browser or --device), or
--import copies the tokens from the official Codex CLI. grok signs in with your
SuperGrok / X Premium subscription (browser or --device).

Each provider can hold several accounts; use --account to name them.`,
		Example: `  ai login openai
  ai login openai --account work --default
  echo "$OPENAI_API_KEY" | ai login openai --account ci
  ai login openai --base-url http://localhost:8080/v1 --account local
  ai login codex
  ai login codex --device
  ai login grok
  ai login higgsfield --api-key "$HF_KEY_ID" --api-secret "$HF_KEY_SECRET"`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := ""
			if len(args) > 0 {
				name = strings.ToLower(args[0])
			} else if output.IsTTY(os.Stdin) && output.IsTTY(os.Stderr) && f.apiKey == "" {
				picked, err := pickProvider("Sign in to which provider?")
				if err != nil {
					return err
				}
				name = picked
			} else {
				name = "gateway"
			}
			info, ok := provider.Lookup(name)
			if !ok {
				return usageErr("unknown provider %q (available: %s)", name, strings.Join(provider.Names(), ", "))
			}
			return runLogin(cmd.Context(), info, f)
		},
	}
	fl := cmd.Flags()
	fl.StringVar(&f.account, "account", "", "Account name to save under (default: \"default\")")
	fl.BoolVar(&f.noBrowser, "no-browser", false, "Do not open a browser")
	fl.BoolVar(&f.device, "device", false, "Use the device-code flow (codex, grok)")
	fl.BoolVar(&f.importCLI, "import", false, "codex: use the Codex CLI login in ~/.codex/auth.json (linked, refreshes are shared)")
	fl.StringVar(&f.baseURL, "base-url", "", "Custom API base URL saved with the account (e.g. an OpenAI-compatible server)")
	fl.StringVar(&f.apiKey, "api-key", "", "API key (non-interactive; prefer stdin to keep it out of shell history)")
	fl.StringVar(&f.apiSecret, "api-secret", "", "API key secret (higgsfield)")
	fl.BoolVar(&f.makeDef, "default", false, "Make this the default account and provider")
	fl.BoolVar(&f.noVerify, "no-verify", false, "Skip checking the key with the provider")
	fl.BoolVar(&f.jsonOut, "json", false, "Print the result as JSON")
	return cmd
}

func runLogin(ctx context.Context, info *provider.Info, f *loginFlags) error {
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt)
	defer stop()
	account := f.account
	if account == "" {
		account = auth.DefaultAccount
	}
	if strings.ContainsAny(account, ":@, ") {
		return usageErr("account names cannot contain ':', '@', ',' or spaces")
	}
	logf := func(msg string) { fmt.Fprintln(os.Stderr, msg) }
	var cred auth.Credential
	var err error

	switch info.Auth {
	case "oauth":
		opts := auth.CodexLoginOptions{Log: logf}
		if !f.noBrowser {
			opts.OpenBrowser = openBrowser
		}
		switch {
		case info.Name == "codex" && f.importCLI:
			// Link instead of copying: Codex refresh tokens are single-use,
			// so the Codex CLI's file stays the one owner of the tokens.
			var linked auth.Credential
			if linked, err = auth.ReadCodexCLIAuth(); err == nil {
				cred = auth.Credential{Link: "codex-cli", IDToken: linked.IDToken, AccountID: linked.AccountID}
			}
		case info.Name == "codex" && f.device:
			cred, err = auth.CodexDeviceLogin(ctx, logf)
		case info.Name == "codex":
			cred, err = auth.CodexBrowserLogin(ctx, opts)
		case info.Name == "grok" && f.device:
			cred, err = auth.GrokDeviceLogin(ctx, logf)
		case info.Name == "grok":
			cred, err = auth.GrokBrowserLogin(ctx, opts)
		}
		if err != nil {
			return err
		}
		cred.Email = auth.CodexEmail(cred.IDToken)
	default:
		cred, err = collectKey(info, f)
		if err != nil {
			return err
		}
		if !f.noVerify && info.Verify != nil {
			fmt.Fprintf(os.Stderr, "Checking key with %s...\n", info.Title)
			vctx, cancel := context.WithTimeout(ctx, 20*time.Second)
			err := info.Verify(vctx, provider.Config{APIKey: cred.APIKey, APISecret: cred.APISecret, BaseURL: cred.BaseURL})
			cancel()
			var apiErr *provider.APIError
			if errors.As(err, &apiErr) && (apiErr.Status == 401 || apiErr.Status == 403) {
				return fmt.Errorf("invalid API key: %s rejected it. Nothing was saved", info.Title)
			}
			if err != nil {
				return fmt.Errorf("could not verify the key (%v); use --no-verify to save it anyway", err)
			}
		}
	}
	if err := auth.Set(info.Name, account, cred, f.makeDef); err != nil {
		return err
	}
	if f.makeDef {
		if err := auth.SetDefaultProvider(info.Name); err != nil {
			return err
		}
	}
	if f.jsonOut {
		writeJSON(os.Stdout, map[string]any{"success": true, "provider": info.Name, "account": account, "path": auth.Path()})
		return nil
	}
	who := ""
	if cred.Email != "" {
		who = " as " + cred.Email
		if plan := auth.CodexPlan(cred.IDToken); plan != "" {
			who += " (" + plan + ")"
		}
	}
	fmt.Fprintf(os.Stderr, "Signed in to %s%s. Saved account %q to %s\n", info.Title, who, account, auth.Path())
	return nil
}

func collectKey(info *provider.Info, f *loginFlags) (auth.Credential, error) {
	cred := auth.Credential{APIKey: strings.TrimSpace(f.apiKey), APISecret: strings.TrimSpace(f.apiSecret), BaseURL: strings.TrimRight(f.baseURL, "/")}
	if cred.APIKey == "" {
		if output.IsTTY(os.Stdin) {
			if info.KeysURL != "" {
				fmt.Fprintf(os.Stderr, "Create an API key at %s\n", info.KeysURL)
				if !f.noBrowser {
					openBrowser(info.KeysURL)
				}
			}
			label := "Paste your API key: "
			if info.Auth == "keypair" {
				label = "API key ID: "
			}
			if cred.BaseURL != "" && info.Name == "openai" {
				label = "API key (leave empty if the server needs none): "
			}
			key, err := promptHidden(label)
			if err != nil {
				return cred, err
			}
			cred.APIKey = strings.TrimSpace(key)
			if info.Auth == "keypair" && cred.APISecret == "" && !strings.Contains(cred.APIKey, ":") {
				secret, err := promptHidden("API key secret: ")
				if err != nil {
					return cred, err
				}
				cred.APISecret = strings.TrimSpace(secret)
			}
		} else {
			data, err := io.ReadAll(bufio.NewReader(os.Stdin))
			if err != nil {
				return cred, err
			}
			cred.APIKey = strings.TrimSpace(string(data))
		}
	}
	if info.Auth == "keypair" && cred.APISecret == "" {
		if id, secret, ok := strings.Cut(cred.APIKey, ":"); ok {
			cred.APIKey, cred.APISecret = id, secret
		}
	}
	if cred.APIKey == "" && !(info.Name == "openai" && cred.BaseURL != "") {
		return cred, errors.New("no API key provided")
	}
	if info.Auth == "keypair" && cred.APISecret == "" {
		return cred, fmt.Errorf("%s needs a key ID and secret (pass id:secret, or --api-key and --api-secret)", info.Title)
	}
	return cred, nil
}

func promptHidden(label string) (string, error) {
	fmt.Fprint(os.Stderr, label)
	b, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return "", fmt.Errorf("could not read input: %w", err)
	}
	return string(b), nil
}

func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	cmd.Stdout, cmd.Stderr = nil, nil
	_ = cmd.Start()
}

func newLogoutCmd() *cobra.Command {
	var account string
	var all bool
	cmd := &cobra.Command{
		Use:   "logout [provider]",
		Short: "Delete saved credentials",
		Example: `  ai logout openai --account work
  ai logout codex
  ai logout --all`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if all {
				if err := auth.DeleteAll(); err != nil {
					return err
				}
				fmt.Fprintln(os.Stderr, "Signed out of every provider.")
				return nil
			}
			name := "gateway"
			if len(args) > 0 {
				name = strings.ToLower(args[0])
			}
			if _, ok := provider.Lookup(name); !ok {
				return usageErr("unknown provider %q", name)
			}
			n, err := auth.Delete(name, account)
			if err != nil {
				return err
			}
			if n == 0 {
				fmt.Fprintf(os.Stderr, "No saved %s credentials to remove.\n", name)
				return nil
			}
			if account != "" {
				fmt.Fprintf(os.Stderr, "Removed %s account %q.\n", name, account)
			} else {
				fmt.Fprintf(os.Stderr, "Removed %d %s account%s.\n", n, name, plural(n))
			}
			if name == "codex" {
				fmt.Fprintln(os.Stderr, "Tokens in ~/.codex/auth.json (Codex CLI) are left untouched.")
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&account, "account", "", "Remove only this account")
	cmd.Flags().BoolVar(&all, "all", false, "Remove every saved credential")
	return cmd
}

// accountRow is one saved or detected credential.
type accountRow struct {
	Provider string `json:"provider"`
	Account  string `json:"account"`
	Default  bool   `json:"default"`
	Source   string `json:"source"`
	Detail   string `json:"detail,omitempty"`
}

func listAccounts() ([]accountRow, error) {
	f, err := auth.Load()
	if err != nil {
		return nil, err
	}
	var rows []accountRow
	for _, info := range provider.All() {
		p, ok := f.Providers[info.Name]
		def := ""
		if ok {
			def = p.DefaultName()
		}
		for _, name := range f.AccountNames(info.Name) {
			c := p.Accounts[name]
			rows = append(rows, accountRow{Provider: info.Name, Account: name, Default: name == def, Source: "login", Detail: credentialDetail(c)})
		}
		if _, envName := envKey(info); envName != "" && info.Auth != "oauth" {
			rows = append(rows, accountRow{Provider: info.Name, Account: "env", Source: "env", Detail: envName + " (used when no --account is given)"})
		}
		if info.Name == "codex" && !ok {
			if c, err := auth.ReadCodexCLIAuth(); err == nil {
				rows = append(rows, accountRow{Provider: "codex", Account: "codex-cli", Default: true, Source: "codex-cli", Detail: credentialDetail(c) + " from " + auth.CodexCLIAuthPath()})
			}
		}
	}
	return rows, nil
}

func credentialDetail(c auth.Credential) string {
	var parts []string
	if c.Link == "codex-cli" {
		parts = append(parts, "linked to "+auth.CodexCLIAuthPath())
	}
	if c.Email != "" {
		parts = append(parts, c.Email)
	} else if email := auth.CodexEmail(c.IDToken); email != "" {
		parts = append(parts, email)
	}
	if plan := auth.CodexPlan(c.IDToken); plan != "" {
		parts = append(parts, plan)
	}
	if c.APIKey != "" {
		parts = append(parts, maskKey(c.APIKey))
	}
	if c.BaseURL != "" {
		parts = append(parts, c.BaseURL)
	}
	return strings.Join(parts, " · ")
}

func maskKey(k string) string {
	if len(k) <= 8 {
		return "••••"
	}
	return k[:4] + "…" + k[len(k)-4:]
}

func newAccountsCmd() *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "accounts",
		Short: "List saved accounts",
		Example: `  ai accounts
  ai accounts --json
  ai accounts use openai work`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			rows, err := listAccounts()
			if err != nil {
				return err
			}
			if jsonOut {
				if rows == nil {
					rows = []accountRow{}
				}
				writeJSON(os.Stdout, rows)
				return nil
			}
			if len(rows) == 0 {
				fmt.Fprintln(os.Stderr, "No accounts yet. Run `ai login <provider>`.")
				return nil
			}
			table := [][]string{{"PROVIDER", "ACCOUNT", "", "DETAILS"}}
			for _, r := range rows {
				mark := ""
				if r.Default {
					mark = "default"
				}
				table = append(table, []string{r.Provider, r.Account, mark, r.Detail})
			}
			printTable(table)
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Output as JSON")
	use := &cobra.Command{
		Use:   "use <provider> <account>",
		Short: "Make an account the provider's default",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := auth.SetDefaultAccount(strings.ToLower(args[0]), args[1]); err != nil {
				return err
			}
			fmt.Fprintf(os.Stderr, "%s now uses account %q by default.\n", args[0], args[1])
			return nil
		},
	}
	cmd.AddCommand(use)
	return cmd
}

type providerRow struct {
	Name         string   `json:"name"`
	Title        string   `json:"title"`
	Auth         string   `json:"auth"`
	Configured   bool     `json:"configured"`
	Default      bool     `json:"default"`
	Accounts     []string `json:"accounts"`
	EnvVars      []string `json:"envVars,omitempty"`
	DefaultModel string   `json:"defaultModel"`
	Docs         string   `json:"docs,omitempty"`
}

func listProviders() []providerRow {
	def, _ := defaultProvider("")
	f, _ := auth.Load()
	var rows []providerRow
	for _, info := range provider.All() {
		accounts := []string{}
		if f != nil {
			accounts = append(accounts, f.AccountNames(info.Name)...)
		}
		rows = append(rows, providerRow{
			Name: info.Name, Title: info.Title, Auth: info.Auth, Configured: isConfigured(info),
			Default: info.Name == def, Accounts: accounts, EnvVars: info.EnvVars, DefaultModel: info.DefaultModel, Docs: info.Docs,
		})
	}
	return rows
}

func newProvidersCmd() *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:     "providers",
		Aliases: []string{"status"},
		Short:   "List providers and whether they are configured",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			rows := listProviders()
			if jsonOut {
				writeJSON(os.Stdout, rows)
				return nil
			}
			table := [][]string{{"PROVIDER", "", "STATUS", "DEFAULT MODEL", "AUTH"}}
			for _, r := range rows {
				status := "not configured"
				if r.Configured {
					status = "ready"
					if len(r.Accounts) > 0 {
						status += " (" + strings.Join(r.Accounts, ", ") + ")"
					}
				}
				mark := ""
				if r.Default {
					mark = "*"
				}
				table = append(table, []string{r.Name, mark, status, r.DefaultModel, authLabel(r.Auth)})
			}
			printTable(table)
			fmt.Fprintln(os.Stderr, "\n* default provider for unprefixed models. Change it with `ai providers default <name>`.")
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Output as JSON")
	cmd.AddCommand(&cobra.Command{
		Use:   "default <provider>",
		Short: "Set the provider used for unprefixed model IDs",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := strings.ToLower(args[0])
			if _, ok := provider.Lookup(name); !ok {
				return usageErr("unknown provider %q", name)
			}
			if err := auth.SetDefaultProvider(name); err != nil {
				return err
			}
			fmt.Fprintf(os.Stderr, "Default provider set to %s.\n", name)
			return nil
		},
	})
	return cmd
}

func authLabel(a string) string {
	switch a {
	case "oauth":
		return "subscription sign-in"
	case "keypair":
		return "key id + secret"
	}
	return "API key"
}

func printTable(rows [][]string) {
	widths := make([]int, len(rows[0]))
	for _, row := range rows {
		for i, cell := range row {
			widths[i] = max(widths[i], len([]rune(cell)))
		}
	}
	for ri, row := range rows {
		var b strings.Builder
		for i, cell := range row {
			if i == len(row)-1 {
				b.WriteString(cell)
				continue
			}
			b.WriteString(cell + strings.Repeat(" ", widths[i]-len([]rune(cell))+2))
		}
		line := strings.TrimRight(b.String(), " ")
		if ri == 0 && output.ColorEnabled() {
			line = "\x1b[2m" + line + "\x1b[0m"
		}
		fmt.Println(line)
	}
}
