package cli

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/lassejlv/ai-cli/internal/provider"
)

type modelRow struct {
	Provider    string   `json:"provider"`
	ID          string   `json:"id"`
	Spec        string   `json:"spec"`
	Name        string   `json:"name,omitempty"`
	Description string   `json:"description,omitempty"`
	Aliases     []string `json:"aliases,omitempty"`
	Default     bool     `json:"default,omitempty"`
	Type        string   `json:"type,omitempty"`
}

func listModels(ctx context.Context, only string, live bool) []modelRow {
	var rows []modelRow
	for _, info := range provider.All() {
		if only != "" && info.Name != only {
			continue
		}
		aliasesFor := map[string][]string{}
		for alias, target := range info.Aliases {
			aliasesFor[target] = append(aliasesFor[target], alias)
		}
		seen := map[string]bool{}
		add := func(id, name, desc, typ string) {
			if seen[id] {
				return
			}
			seen[id] = true
			rows = append(rows, modelRow{Provider: info.Name, ID: id, Spec: info.Name + ":" + id, Name: name, Description: desc,
				Aliases: aliasesFor[id], Default: id == info.DefaultModel, Type: typ})
		}
		if info.Name == "gateway" && live {
			cctx, cancel := context.WithTimeout(ctx, 6*time.Second)
			models, err := provider.GatewayImageModels(cctx)
			cancel()
			if err != nil {
				fmt.Fprintln(os.Stderr, "Warning: could not fetch models from AI Gateway; showing built-in list")
			}
			for _, m := range models {
				add(m.ID, m.Name, "", m.Type)
			}
		}
		for _, m := range info.Models {
			add(m.ID, "", m.Description, "")
		}
	}
	return rows
}

func newModelsCmd() *cobra.Command {
	var providerName string
	var jsonOut, offline bool
	cmd := &cobra.Command{
		Use:   "models [provider]",
		Short: "List image models",
		Long: `List image models for every provider. AI Gateway models are fetched live
from the public catalog; other providers show a curated list, and any model ID
the provider accepts can be passed with -m provider:model.`,
		Example: `  ai models
  ai models higgsfield
  ai models --json`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 {
				providerName = args[0]
			}
			providerName = strings.ToLower(providerName)
			if providerName != "" {
				if _, ok := provider.Lookup(providerName); !ok {
					return usageErr("unknown provider %q (available: %s)", providerName, strings.Join(provider.Names(), ", "))
				}
			}
			rows := listModels(cmd.Context(), providerName, !offline)
			if jsonOut {
				writeJSON(os.Stdout, rows)
				return nil
			}
			current := ""
			for _, r := range rows {
				if r.Provider != current {
					current = r.Provider
					info, _ := provider.Lookup(current)
					fmt.Printf("\n%s  (%s)\n", info.Title, current)
				}
				line := "  " + r.ID
				if r.Default {
					line += "  [default]"
				}
				if r.Description != "" {
					line += "  — " + r.Description
				}
				fmt.Println(line)
			}
			fmt.Println()
			return nil
		},
	}
	cmd.Flags().StringVar(&providerName, "provider", "", "Only list one provider")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Output as JSON")
	cmd.Flags().BoolVar(&offline, "offline", false, "Skip the live AI Gateway catalog")
	return cmd
}
