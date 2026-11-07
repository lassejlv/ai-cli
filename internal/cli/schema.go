package cli

import (
	"os"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

type flagSchema struct {
	Name       string `json:"name"`
	Shorthand  string `json:"shorthand,omitempty"`
	Type       string `json:"type"`
	Default    string `json:"default,omitempty"`
	Usage      string `json:"usage"`
	Repeatable bool   `json:"repeatable,omitempty"`
}

type commandSchema struct {
	Name     string          `json:"name"`
	Path     string          `json:"path"`
	Use      string          `json:"use"`
	Short    string          `json:"short"`
	Aliases  []string        `json:"aliases,omitempty"`
	Example  string          `json:"example,omitempty"`
	Flags    []flagSchema    `json:"flags,omitempty"`
	Commands []commandSchema `json:"commands,omitempty"`
}

func describe(cmd *cobra.Command) commandSchema {
	cs := commandSchema{Name: cmd.Name(), Path: cmd.CommandPath(), Use: cmd.Use, Short: cmd.Short, Aliases: cmd.Aliases, Example: cmd.Example}
	cmd.Flags().VisitAll(func(f *pflag.Flag) {
		if f.Hidden || f.Name == "help" {
			return
		}
		cs.Flags = append(cs.Flags, flagSchema{
			Name: f.Name, Shorthand: f.Shorthand, Type: f.Value.Type(), Default: f.DefValue, Usage: f.Usage,
			Repeatable: f.Value.Type() == "stringArray",
		})
	})
	for _, child := range cmd.Commands() {
		if child.Hidden || child.Name() == "help" || child.Name() == "completion" {
			continue
		}
		cs.Commands = append(cs.Commands, describe(child))
	}
	return cs
}

func newSchemaCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "schema",
		Short: "Describe every command, flag, provider and exit code as JSON (for agents)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			writeJSON(os.Stdout, map[string]any{
				"name":      "ai",
				"version":   Version,
				"commands":  describe(cmd.Root()),
				"providers": listProviders(),
				"modelSpec": "provider[@account]:model — e.g. openai:gpt-image-2, openai@work:gpt-image-1, higgsfield:soul. Comma-separate for several models. Unprefixed IDs use the default provider.",
				"exitCodes": map[string]string{
					"0":   "success",
					"1":   "invalid input, authentication failure, or every generation failed",
					"2":   "partial failure: some models succeeded and others failed",
					"130": "cancelled",
				},
				"outputs": map[string]string{
					"default":    "files saved to -o / AI_CLI_OUTPUT_DIR / current directory; a single image is written raw to a piped stdout",
					"--json":     "JSON manifest on stdout; progress is suppressed",
					"--raw":      "raw bytes of one image on stdout",
					"--base64":   "one base64 string per image on stdout (embedded in the manifest with --json)",
					"--data-url": "one data: URL per image on stdout",
					"--dry-run":  "resolved request and credential sources as JSON, no provider call",
				},
				"environment": map[string]string{
					"AI_CLI_IMAGE_PROVIDER": "default provider for unprefixed models",
					"AI_CLI_IMAGE_MODEL":    "default model (may include a provider prefix)",
					"AI_CLI_OUTPUT_DIR":     "default output directory",
					"AI_CLI_PREVIEW":        "1 forces Kitty previews on, 0 disables them",
					"AI_CLI_NO_TUI":         "never open the interactive TUI",
					"AI_CLI_CONFIG_DIR":     "where auth.json and history/ are stored (default ~/.config/ai-cli)",
					"AI_CLI_NO_HISTORY":     "1 disables recording local generation history",
				},
			})
			return nil
		},
	}
}
