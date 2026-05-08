// Package cmd implements the postbox CLI. The root command groups all
// subcommands: serve, register, discover, status, smtp, domain, agent, and mail.
package cmd

import (
	"os"

	"github.com/rbaliyan/postbox/cmd/mail"
	"github.com/spf13/cobra"
)

var rootCmd = &cobra.Command{
	Use:               "postbox",
	Short:             "Postbox — agent communication backbone and gRPC mail server",
	PersistentPreRunE: persistentPreRun,
}

func Execute() {
	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

func init() {
	rootCmd.PersistentFlags().String("addr", "localhost:50051", "postbox gRPC server address")
	rootCmd.PersistentFlags().String("config", "", "path to config file (YAML)")

	rootCmd.AddCommand(serveCmd)
	rootCmd.AddCommand(registerCmd)
	rootCmd.AddCommand(discoverCmd)
	rootCmd.AddCommand(statusCmd)
	rootCmd.AddCommand(smtpCmd)
	rootCmd.AddCommand(domainCmd)
	rootCmd.AddCommand(userCmd)
	rootCmd.AddCommand(mail.Command())
}

// persistentPreRun loads the config file when --config is set.
// It runs before every subcommand so the viper state is ready when
// subcommand RunE functions call resolveConfig.
func persistentPreRun(cmd *cobra.Command, _ []string) error {
	cfgPath, _ := cmd.Root().PersistentFlags().GetString("config")
	return loadConfig(cfgPath)
}
