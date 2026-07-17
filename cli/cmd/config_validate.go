package cmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/ratecap/core/config"
)

func newConfigValidateCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "validate <path>",
		Short: "Validate a ratecap.yaml config file",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			path := args[0]

			cfg, err := config.Load(path)
			if err != nil {
				return fmt.Errorf("loading %s: %w", path, err)
			}

			if err := cfg.Validate(); err != nil {
				return fmt.Errorf("invalid config: %w", err)
			}

			fmt.Fprintf(cmd.OutOrStdout(), "%s is valid\n", path)
			return nil
		},
	}
}
