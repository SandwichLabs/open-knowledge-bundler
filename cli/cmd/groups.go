package cmd

import "github.com/spf13/cobra"

// benchCmd groups research / benchmark scaffolding (answer, eval, convert) under a
// single namespace so the top-level surface stays focused on building and
// inspecting portable knowledge bundles. These tools evaluate the local agent;
// they are not part of the core bundle-building pipeline.
var benchCmd = &cobra.Command{
	Use:   "bench",
	Short: "Benchmark & dataset tools for evaluating the local agent",
	Long: `Research scaffolding, quarantined from the core build/inspect surface:

  bench answer        batch-answer a question set with the local agent (no scoring)
  bench eval          score answers against a known-answer key (deterministic)
  bench convert       convert external datasets into the okb ingest format

None of these are needed to build or ship a knowledge bundle.`,
}

// siteCmd groups the hosted-viewer concern (static-site generation + a live HTTP
// API/UI). This is a different product from the portable .duckdb + OKF + Skill
// bundle, so it lives under its own namespace.
var siteCmd = &cobra.Command{
	Use:   "site",
	Short: "Hosted graph viewer: static-site generation and a live HTTP API/UI",
	Long: `Build or serve an interactive view of the knowledge graph:

  site generate       self-contained static site (index.html + D3 viewer + data)
  site serve          live HTTP API (/api/query, /api/node, /api/sql, /api/stats) + UI

This is separate from the portable knowledge bundle ('okb bundle').`,
}

func init() {
	rootCmd.AddCommand(benchCmd)
	rootCmd.AddCommand(siteCmd)
}
