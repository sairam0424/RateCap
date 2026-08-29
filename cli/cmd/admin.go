package cmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"

	"github.com/spf13/cobra"
)

func newAdminCmd() *cobra.Command {
	adminCmd := &cobra.Command{
		Use:   "admin",
		Short: "Incident-response admin commands",
	}
	adminCmd.AddCommand(newAdminSetLimitCmd())
	return adminCmd
}

func newAdminSetLimitCmd() *cobra.Command {
	var sidecarAddr string
	var adminSecret string
	var tier string
	var value int

	cmd := &cobra.Command{
		Use:   "set-limit",
		Short: "Instantly change Tier 1's rate or Tier 3's reserved_critical_pct fleet-wide",
		RunE: func(cmd *cobra.Command, args []string) error {
			if adminSecret == "" {
				adminSecret = os.Getenv("RATECAP_ADMIN_SECRET")
			}
			if adminSecret == "" {
				return fmt.Errorf("--admin-secret is required (or set RATECAP_ADMIN_SECRET)")
			}

			body, err := json.Marshal(map[string]any{"tier": tier, "value": value})
			if err != nil {
				return err
			}

			req, err := http.NewRequestWithContext(cmd.Context(), http.MethodPost, sidecarAddr+"/admin/set-limit", bytes.NewReader(body))
			if err != nil {
				return err
			}
			req.Header.Set("X-RateCap-Admin-Secret", adminSecret)
			req.Header.Set("Content-Type", "application/json")

			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				return fmt.Errorf("calling sidecar: %w", err)
			}
			defer resp.Body.Close()

			respBody, err := io.ReadAll(resp.Body)
			if err != nil {
				return err
			}
			if resp.StatusCode != http.StatusOK {
				return fmt.Errorf("sidecar returned %d: %s", resp.StatusCode, respBody)
			}

			fmt.Fprintf(cmd.OutOrStdout(), "%s\n", respBody)
			return nil
		},
	}

	cmd.Flags().StringVar(&sidecarAddr, "sidecar-addr", "http://localhost:8080", "target sidecar address")
	cmd.Flags().StringVar(&adminSecret, "admin-secret", "", "admin secret (or set RATECAP_ADMIN_SECRET)")
	cmd.Flags().StringVar(&tier, "tier", "", "tier to change: rate_limiter or fleet_shedder")
	cmd.Flags().IntVar(&value, "value", 0, "new value: rate (rate_limiter) or reserved_critical_pct (fleet_shedder)")
	_ = cmd.MarkFlagRequired("tier")
	_ = cmd.MarkFlagRequired("value")

	return cmd
}
