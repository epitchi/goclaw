package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/nextlevelbuilder/goclaw/pkg/config"
)

func modelsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "models",
		Short: "List available AI models and providers",
	}
	cmd.AddCommand(modelsListCmd())
	return cmd
}

type modelEntry struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
	Status   string `json:"status"`
}

func modelsListCmd() *cobra.Command {
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List configured models and providers",
		Run: func(cmd *cobra.Command, args []string) {
			cfgPath := resolveConfigPath()
			cfg, err := config.Load(cfgPath)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Error loading config: %s\n", err)
				os.Exit(1)
			}

			entries := buildModelList(cfg)

			if jsonOutput {
				data, _ := json.MarshalIndent(entries, "", "  ")
				fmt.Println(string(data))
				return
			}

			tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintf(tw, "PROVIDER\tMODEL\tSTATUS\n")
			for _, e := range entries {
				fmt.Fprintf(tw, "%s\t%s\t%s\n", e.Provider, e.Model, e.Status)
			}
			tw.Flush()
		},
	}
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "output as JSON")
	return cmd
}

func buildModelList(cfg *config.Config) []modelEntry {
	var entries []modelEntry

	// Default agent model
	entries = append(entries, modelEntry{
		Provider: cfg.Agents.Defaults.Provider,
		Model:    cfg.Agents.Defaults.Model,
		Status:   "default",
	})

	// Per-agent overrides
	for id, spec := range cfg.Agents.List {
		if spec.Model != "" {
			entries = append(entries, modelEntry{
				Provider: spec.Provider,
				Model:    spec.Model,
				Status:   "agent:" + id,
			})
		}
	}

	// Available providers
	type providerCheck struct {
		name   string
		hasKey bool
	}
	providers := []providerCheck{
		{"anthropic", cfg.Providers.Anthropic.APIKey != ""},
		{"openai", cfg.Providers.OpenAI.APIKey != ""},
		{"openrouter", cfg.Providers.OpenRouter.APIKey != ""},
		{"gemini", cfg.Providers.Gemini.APIKey != ""},
		{"groq", cfg.Providers.Groq.APIKey != ""},
		{"deepseek", cfg.Providers.DeepSeek.APIKey != ""},
		{"mistral", cfg.Providers.Mistral.APIKey != ""},
		{"xai", cfg.Providers.XAI.APIKey != ""},
	}
	for _, p := range providers {
		if p.hasKey {
			entries = append(entries, modelEntry{
				Provider: p.name,
				Model:    "(any)",
				Status:   "available",
			})
		}
	}

	return entries
}
