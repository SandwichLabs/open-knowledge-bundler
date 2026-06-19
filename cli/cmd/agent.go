package cmd

import (
	"context"
	"fmt"
	"os"

	"github.com/sandwich-labs/chicago-business-intelligence/cli/agent"
	"github.com/spf13/cobra"
)

var (
	agentBundle      string
	agentDB          string
	agentTier        string
	agentModel       string
	agentProcessor   string
	agentReconfigure bool
	agentAsk         string
)

var agentCmd = &cobra.Command{
	Use:   "agent",
	Short: "Chat with an OKF bundle using a self-contained local LLM",
	Long: `Opens an interactive chat TUI backed entirely by local models (via kronk /
llama.cpp). The agent answers questions about an OKF bundle produced by
'cbi generate okf --skill --include-db' using SQL queries against the bundle's
DuckDB graph plus exploration of its markdown concept documents.

On first run you choose a model size; the choice (and model/processor settings)
persist in ~/.config/cbi/config.yaml. Models download from Hugging Face on first
use. The default backend is Vulkan; override with --gpu or the config.

Examples:
  cbi agent --bundle ./okf-bundle
  cbi agent --bundle ./okf-bundle --tier large
  cbi agent --bundle ./okf-bundle --reconfigure`,
	RunE: func(cmd *cobra.Command, args []string) error {
		// 1. User config (first-run picker / --reconfigure). The picker is
		//    skipped in --ask mode so it stays scriptable.
		cfg, err := agent.LoadConfig(agentReconfigure, agentAsk == "")
		if err != nil {
			return err
		}
		if agentTier != "" {
			if err := cfg.SetTier(agentTier); err != nil {
				return err
			}
		}
		if agentProcessor != "" {
			cfg.Processor = agentProcessor
		}

		// 2. Force the llama.cpp backend before any kronk call (the embedded
		//    provider's libs loader reads KRONK_PROCESSOR too).
		if cfg.Processor != "" {
			if err := os.Setenv("KRONK_PROCESSOR", cfg.Processor); err != nil {
				return fmt.Errorf("setting KRONK_PROCESSOR: %w", err)
			}
			fmt.Printf("llama.cpp backend: %s\n", cfg.Processor)
		}

		// 3. Load the bundle.
		bundle, err := agent.LoadBundle(agentBundle, agentDB)
		if err != nil {
			return err
		}

		// 4. Resolve model sources (flags > config > bundle).
		llmSource := cfg.LLMSource()
		if agentModel != "" {
			llmSource = agentModel
		}
		embedSource := cfg.EmbedSource

		// 5. Wire the session (downloads + loads happen here, with progress).
		ctx := context.Background()
		sess, err := agent.NewSession(ctx, bundle, llmSource, embedSource, func(format string, a ...any) {
			fmt.Printf(format+"\n", a...)
		})
		if err != nil {
			return err
		}
		defer sess.Close()

		// 6. Chat (interactive TUI) or answer a single question (--ask).
		if agentAsk != "" {
			return sess.RunOnce(ctx, agentAsk)
		}
		return sess.Run(ctx)
	},
}

func init() {
	agentCmd.Flags().StringVar(&agentBundle, "bundle", "", "path to an OKF bundle directory (required)")
	agentCmd.Flags().StringVar(&agentDB, "db", "", "override the bundle's DuckDB path")
	agentCmd.Flags().StringVar(&agentTier, "tier", "", "model size tier (small|medium|large|xl|moe)")
	agentCmd.Flags().StringVar(&agentModel, "model", "", "override the LLM with an explicit kronk model source")
	agentCmd.Flags().StringVar(&agentProcessor, "gpu", "", "llama.cpp backend (cpu|cuda|rocm|vulkan); overrides config")
	agentCmd.Flags().BoolVar(&agentReconfigure, "reconfigure", false, "re-run the model size picker")
	agentCmd.Flags().StringVar(&agentAsk, "ask", "", "answer a single question non-interactively and exit (no TUI)")
	_ = agentCmd.MarkFlagRequired("bundle")
	rootCmd.AddCommand(agentCmd)
}
