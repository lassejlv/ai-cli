// Package cli wires the ai command line.
package cli

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/lassejlv/ai-cli/internal/output"
	"github.com/lassejlv/ai-cli/internal/provider"
)

// Version is set at build time with -ldflags "-X github.com/lassejlv/ai-cli/internal/cli.Version=...".
var Version = "dev"

type exitError struct {
	code int
	msg  string
}

func (e *exitError) Error() string { return e.msg }

func usageErr(format string, args ...any) error {
	return &exitError{code: 1, msg: fmt.Sprintf(format, args...)}
}

// NewRoot builds the root command.
func NewRoot() *cobra.Command {
	root := &cobra.Command{
		Use:   "ai",
		Short: "Generate images from your terminal",
		Long: `Generate images from your terminal with AI Gateway, OpenAI, your ChatGPT
(Codex) or Grok subscription, Higgsfield, fal, Replicate, Black Forest Labs,
Google, xAI, Stability AI and OpenRouter.

Run ` + "`ai`" + ` with no arguments in a terminal to open the interactive TUI.
Agents: every command supports --json; see ` + "`ai schema`" + ` and ` + "`ai mcp`" + `.`,
		Version:       Version,
		SilenceUsage:  true,
		SilenceErrors: true,
		Args:          cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if output.IsTTY(os.Stdin) && output.IsTTY(os.Stdout) && os.Getenv("AI_CLI_NO_TUI") == "" {
				return runTUI(cmd.Context(), tuiDefaults{flags: &imageFlags{n: 1, concurrency: defaultImageConcurrency, timeout: defaultImageTimeout, maxRetries: 2}})
			}
			return cmd.Help()
		},
	}
	root.SetVersionTemplate("{{.Version}}\n")
	provider.UserAgent = "ai-cli-go/" + Version
	root.AddCommand(
		newImageCmd(),
		newLoginCmd(),
		newLogoutCmd(),
		newAccountsCmd(),
		newProvidersCmd(),
		newModelsCmd(),
		newSchemaCmd(),
		newMCPCmd(),
		newTUICmd(),
		newHistoryCmd(),
		newReplayCmd(false),
		newReplayCmd(true),
	)
	return root
}

// Execute runs the CLI and returns the process exit code.
func Execute() int {
	root := NewRoot()
	err := root.ExecuteContext(context.Background())
	if err == nil {
		return 0
	}
	var ee *exitError
	if errors.As(err, &ee) {
		if ee.msg != "" {
			fmt.Fprintf(os.Stderr, "Error: %s\n", ee.msg)
		}
		return ee.code
	}
	var authErr *provider.AuthError
	if errors.As(err, &authErr) {
		fmt.Fprintf(os.Stderr, "Error: %s\n", authErr.Message)
		return 1
	}
	fmt.Fprintf(os.Stderr, "Error: %s\n", err)
	return 1
}
