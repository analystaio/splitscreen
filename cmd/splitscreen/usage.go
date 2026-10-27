package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/analystaio/splitscreen/config"
	"github.com/analystaio/splitscreen/internal/store"
)

// usageCmd prints the latest subscription rate-limit reading per account.
//
// Readings arrive with turns: the harness reports the windows at the end of
// each one, and the gateway keeps the newest per account and window. Nothing
// is fetched here, so every row says when it was seen. Accounts are keyed by
// the runner's harness_secret; naming them is left to whoever calls this.
func usageCmd() *cobra.Command {
	var cfgPath string
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "usage",
		Short: "Show the latest subscription rate-limit reading per account",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load(cfgPath)
			if err != nil {
				return err
			}
			storePath := cfg.Gateway.Store
			if !filepath.IsAbs(storePath) {
				storePath = filepath.Join(filepath.Dir(cfgPath), storePath)
			}
			st, err := store.OpenReadOnly(storePath)
			if err != nil {
				return err
			}
			defer st.Close()
			rows, err := st.RateLimits()
			if err != nil {
				return err
			}

			type entry struct {
				Account     string  `json:"account"`
				Window      string  `json:"window"`
				Utilization float64 `json:"utilization"`
				ResetsAt    int64   `json:"resets_at,omitempty"`
				Status      string  `json:"status,omitempty"`
				Runner      string  `json:"runner"`
				At          int64   `json:"at"`
			}
			out := make([]entry, 0, len(rows))
			for _, r := range rows {
				e := entry{
					Account: r.Account, Window: r.Window, Utilization: r.Utilization,
					Status: r.Status, Runner: r.Runner, At: r.At.Unix(),
				}
				if !r.ResetsAt.IsZero() {
					e.ResetsAt = r.ResetsAt.Unix()
				}
				out = append(out, e)
			}

			if asJSON {
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(out)
			}
			if len(out) == 0 {
				fmt.Println("(no readings yet)")
			}
			for _, e := range out {
				fmt.Printf("%s %s %.0f%%", e.Account, e.Window, e.Utilization*100)
				if e.Status != "" {
					fmt.Printf(" %s", e.Status)
				}
				if e.ResetsAt != 0 {
					fmt.Printf(", resets %s", time.Unix(e.ResetsAt, 0).UTC().Format(time.RFC3339))
				}
				fmt.Printf(" (seen %s via %s)\n", time.Unix(e.At, 0).UTC().Format(time.RFC3339), e.Runner)
			}
			return nil
		},
	}
	cmd.Flags().StringVarP(&cfgPath, "config", "c", defaultConfigPath, "path to the configuration file")
	cmd.Flags().BoolVar(&asJSON, "json", false, "machine-readable output")
	return cmd
}
