package cmd

import (
	"github.com/spf13/cobra"
)

func NewRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "ratecapctl",
		Short: "Operator CLI for RateCap — validate config, benchmark a running sidecar",
	}
	root.AddCommand(newConfigCmd())
	root.AddCommand(newBenchCmd())
	return root
}

func newConfigCmd() *cobra.Command {
	configCmd := &cobra.Command{
		Use:   "config",
		Short: "Config-related commands",
	}
	configCmd.AddCommand(newConfigValidateCmd())
	return configCmd
}
