package cmd

import "github.com/spf13/cobra"

func newBenchCmd() *cobra.Command {
	benchCmd := &cobra.Command{
		Use:   "bench",
		Short: "Benchmarking commands",
	}
	benchCmd.AddCommand(newBenchRunCmd())
	return benchCmd
}
