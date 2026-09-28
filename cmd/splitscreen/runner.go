package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/avarant/splitscreen/internal/harness"
	"github.com/avarant/splitscreen/internal/runner"
)

func runnerCmd() *cobra.Command {
	var (
		name        string
		gatewayURL  string
		token       string
		tokenFile   string
		fingerprint string
		insecure    bool
		cwd         string
		harnessName string
		harnessCred string
		runtimeRoot string
		idle        time.Duration
		maxSessions int
		logLevel    string
	)

	cmd := &cobra.Command{
		Use:   "runner",
		Short: "Run an agent-side daemon",
		Long: `Runs one working tree under one harness.

The runner dials out to the gateway — it never listens for it — so it works from
a private subnet, behind NAT, or on a laptop with no inbound rules. It holds no
chat credentials, no forge credentials, and no routing configuration.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			log := newLogger(logLevel)

			// Prefer a file or the environment over a flag: a token in argv is
			// visible in the process table to every user on the box.
			if token == "" && tokenFile != "" {
				raw, err := os.ReadFile(tokenFile)
				if err != nil {
					return fmt.Errorf("runner: read token file: %w", err)
				}
				token = strings.TrimSpace(string(raw))
			}
			if token == "" {
				token = os.Getenv("SPLITSCREEN_TOKEN")
			}
			if token == "" {
				return errors.New("runner: no enrollment token (use --token-file or SPLITSCREEN_TOKEN)")
			}
			if cwd == "" {
				return errors.New("runner: --cwd is required; it is the working tree the agent operates on")
			}
			if info, err := os.Stat(cwd); err != nil || !info.IsDir() {
				return fmt.Errorf("runner: --cwd %q is not a directory", cwd)
			}

			r, err := runner.New(runner.Options{
				Name:               name,
				Gateway:            gatewayURL,
				Token:              token,
				Fingerprint:        fingerprint,
				Insecure:           insecure,
				Cwd:                cwd,
				HarnessName:        harnessName,
				HarnessCredentials: harnessCred,
				RuntimeRoot:        runtimeRoot,
				IdleTimeout:        idle,
				MaxSessions:        maxSessions,
				Logger:             log,
			})
			if err != nil {
				return err
			}

			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()

			log.Info("runner starting",
				"name", name, "gateway", gatewayURL, "cwd", cwd,
				"harness", harnessName, "idle", idle, "max_sessions", maxSessions)
			return r.Run(ctx)
		},
	}

	f := cmd.Flags()
	f.StringVar(&name, "name", os.Getenv("SPLITSCREEN_RUNNER"), "runner name, as declared in the gateway config")
	f.StringVar(&gatewayURL, "gateway", os.Getenv("SPLITSCREEN_GATEWAY"), "gateway URL, e.g. wss://10.0.0.5:8443")
	f.StringVar(&token, "token", "", "enrollment token (prefer --token-file)")
	f.StringVar(&tokenFile, "token-file", "", "file containing the enrollment token")
	f.StringVar(&fingerprint, "fingerprint", os.Getenv("SPLITSCREEN_GATEWAY_FINGERPRINT"),
		"pinned sha256 fingerprint of the gateway certificate")
	f.BoolVar(&insecure, "insecure", false, "skip TLS verification (development only)")
	f.StringVar(&cwd, "cwd", "", "working tree the agent operates on")
	f.StringVar(&harnessName, "harness", "claude-code",
		fmt.Sprintf("harness adapter (available: %s)", strings.Join(harness.Names(), ", ")))
	f.StringVar(&harnessCred, "harness-credentials", os.Getenv("SPLITSCREEN_HARNESS_CREDENTIALS"),
		"existing credentials file on this host to expose in the harness config dir "+
			"(for subscription auth, where the credential is already on disk and refreshed in place)")
	f.StringVar(&runtimeRoot, "runtime-root", "", "runtime directory; should be tmpfs (default /run/splitscreen)")
	f.DurationVar(&idle, "idle", 30*time.Minute, "kill a session after this much silence; it resumes on the next message")
	f.IntVar(&maxSessions, "max-sessions", envInt("SPLITSCREEN_MAX_SESSIONS", 0),
		"cap on resident harness processes; at the cap the longest-idle session between turns is reaped, and if none exists the new session is refused (0 = unlimited)")
	f.StringVar(&logLevel, "log-level", "info", "debug, info, warn, or error")

	_ = cmd.MarkFlagRequired("name")
	cmd.AddCommand(runnerManageCmds()...)
	return cmd
}

// envInt reads an integer environment variable, so a deployed unit file can
// gain the setting without being edited.
func envInt(key string, fallback int) int {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return fallback
	}
	return n
}
