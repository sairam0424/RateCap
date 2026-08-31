package cmd

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"
)

// Version is overridden at build time via
// -ldflags "-X github.com/ratecap/cli/cmd.Version=$(cat VERSION)". The
// repo-root VERSION file is the single source of truth (established Phase 0);
// this must never hardcode a second version string. cli/ is its own Go
// module (separate go.mod), so go:embed can't reach the repo-root VERSION
// file directly — ldflags injection is the only option.
var Version = "dev"

func NewRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:     "ratecapctl",
		Short:   "Operator CLI for RateCap — validate config, benchmark a running sidecar",
		Version: Version,
	}
	root.AddCommand(newConfigCmd())
	root.AddCommand(newBenchCmd())
	root.AddCommand(newAdminCmd())
	root.AddCommand(newTLSCmd())
	return root
}

// Run builds and executes the root command against args, writing to stdout/
// stderr, and returns the process exit code — mirroring main.go's original
// inline logic exactly, but without calling os.Exit, so tests can exercise
// the real exit-code mapping in-process instead of spawning a subprocess.
func Run(args []string, stdout, stderr io.Writer) int {
	root := NewRootCmd()
	root.SetArgs(args)
	root.SetOut(stdout)
	root.SetErr(stderr)
	if err := root.Execute(); err != nil {
		// Best-effort: a transient write failure on the error line must never
		// change the exit-code mapping this function exists to guarantee.
		// (errcheck's default os.Stdout/os.Stderr exemption for fmt.Fprint*
		// doesn't apply here since stderr is a generic io.Writer parameter,
		// not a literal os.Stderr reference.)
		fmt.Fprintln(stderr, err) //nolint:errcheck // see comment above
		return 1
	}
	return 0
}

func newConfigCmd() *cobra.Command {
	configCmd := &cobra.Command{
		Use:   "config",
		Short: "Config-related commands",
	}
	configCmd.AddCommand(newConfigValidateCmd())
	return configCmd
}
