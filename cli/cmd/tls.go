package cmd

import (
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

func newTLSCmd() *cobra.Command {
	tlsCmd := &cobra.Command{
		Use:   "tls",
		Short: "TLS certificate commands",
	}
	tlsCmd.AddCommand(newTLSCheckCmd())
	return tlsCmd
}

func newTLSCheckCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "check <cert-path> <expected-host>",
		Short: "Verify a certificate's SAN list covers the given hostname before deploying it",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			certPath, expectedHost := args[0], args[1]

			data, err := os.ReadFile(certPath) //nolint:gosec // certPath is a local CLI positional arg the operator types themselves, not attacker input
			if err != nil {
				return fmt.Errorf("reading %s: %w", certPath, err)
			}
			block, _ := pem.Decode(data)
			if block == nil {
				return fmt.Errorf("%s does not contain a valid PEM block", certPath)
			}
			cert, err := x509.ParseCertificate(block.Bytes)
			if err != nil {
				return fmt.Errorf("parsing certificate in %s: %w", certPath, err)
			}

			if err := cert.VerifyHostname(expectedHost); err != nil {
				return fmt.Errorf("%s's SAN list %v does not cover %q: %w — this is the exact failure mode Helm chart deployments hit when demo certs (SAN: core/sidecar) are reused with a real release name, and it produces no server-side log", certPath, cert.DNSNames, expectedHost, err)
			}

			if _, err := fmt.Fprintf(cmd.OutOrStdout(), "%s: SAN list %v covers %q\n", certPath, cert.DNSNames, expectedHost); err != nil {
				return err
			}
			return nil
		},
	}
}
