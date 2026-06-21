package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

var cfgFile string

var rootCmd = &cobra.Command{
	Use:   "cbi",
	Short: "Build portable knowledge bundles (GraphRAG) from any domain",
	Long: `cbi turns a domain (a prose corpus or structured data) into a portable
knowledge bundle — a DuckDB graph + browsable OKF markdown + an agent skill —
that any agent can query, all local-first.

  BUILD     extract · ingest · bundle      (input → graph → portable bundle)
  INSPECT   query · graph · schema         (validate the graph)
  CONSUME   agent                          (chat / --ask over a bundle, fully local)
  bench …   answer · eval · convert        (benchmark the local agent)
  site …    generate · serve               (hosted graph viewer)`,
}

func Execute() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func init() {
	cobra.OnInitialize(initConfig)
	rootCmd.PersistentFlags().StringVar(&cfgFile, "config", "", "domain config file (default: ./domain.yaml)")
}

func initConfig() {
	if cfgFile != "" {
		viper.SetConfigFile(cfgFile)
	} else {
		viper.SetConfigName("domain")
		viper.SetConfigType("yaml")
		viper.AddConfigPath(".")
	}
	viper.AutomaticEnv()
	_ = viper.ReadInConfig()
}
