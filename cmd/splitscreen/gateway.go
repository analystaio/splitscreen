package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/avarant/splitscreen/config"
	"github.com/avarant/splitscreen/internal/forge"
	"github.com/avarant/splitscreen/internal/gateway"
	"github.com/avarant/splitscreen/internal/secrets"
	"github.com/avarant/splitscreen/internal/store"
	"github.com/avarant/splitscreen/internal/surface"
	"github.com/avarant/splitscreen/internal/surface/slackx"
	"github.com/avarant/splitscreen/internal/wake"
)

func gatewayCmd() *cobra.Command {
	var cfgPath, logLevel, webAddr string

	cmd := &cobra.Command{
		Use:   "gateway",
		Short: "Run the control plane",
		Long: `Runs the singleton control plane.

Exactly one gateway may run against a given chat app. Two connections to one app
means the platform delivers each payload to one of them at random, which is the
delivery bug this architecture exists to eliminate.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			log := newLogger(logLevel)

			cfg, err := config.Load(cfgPath)
			if err != nil {
				return err
			}

			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()

			sec, err := buildSecrets(ctx, cfg, log)
			if err != nil {
				return err
			}
			// Fail at startup on a missing secret rather than at first use, when
			// the failure would surface as an unexplained refusal mid-conversation.
			if err := verifySecrets(cfg, sec, log); err != nil {
				return err
			}

			storePath := cfg.Gateway.Store
			if !filepath.IsAbs(storePath) {
				storePath = filepath.Join(filepath.Dir(cfgPath), storePath)
			}
			st, err := store.Open(storePath)
			if err != nil {
				return err
			}
			defer st.Close()

			surfaces, err := buildSurfaces(cfg, sec, log)
			if err != nil {
				return err
			}
			fg, err := buildForge(cfg, sec, log)
			if err != nil {
				return err
			}

			gw, err := gateway.New(gateway.Options{
				ConfigPath: cfgPath,
				Config:     cfg,
				Store:      st,
				Secrets:    sec,
				Forge:      fg,
				Surfaces:   surfaces,
				Logger:     log,
				// Clients are built on first use, so this costs nothing until a
				// runner declaring wake is actually asleep. Runners that name no
				// region use the Parameter Store region, then the ambient one.
				Waker: wake.NewEC2(cfg.Gateway.SecretsSSM.Region),
			})
			if err != nil {
				return err
			}

			// SIGHUP reloads. A validation failure leaves the running config
			// untouched: a bad edit must never partially apply.
			hup := make(chan os.Signal, 1)
			signal.Notify(hup, syscall.SIGHUP)
			go func() {
				for range hup {
					if err := gw.Reload(); err != nil {
						log.Error("reload rejected; keeping the running config", "err", err)
					}
				}
			}()

			errCh := make(chan error, 3)
			go func() { errCh <- gw.ServeRunners(ctx) }()
			go func() { errCh <- gw.Run(ctx) }()
			if webAddr != "" {
				go func() { errCh <- gw.ServeWeb(ctx, webAddr) }()
			}

			log.Info("gateway ready",
				"runners", len(cfg.Runners), "routes", len(cfg.Routes),
				"surfaces", len(surfaces), "store", storePath)

			select {
			case <-ctx.Done():
				log.Info("shutting down")
				return nil
			case err := <-errCh:
				return err
			}
		},
	}

	cmd.Flags().StringVarP(&cfgPath, "config", "c", defaultConfigPath, "path to the configuration file")
	cmd.Flags().StringVar(&logLevel, "log-level", "info", "debug, info, warn, or error")
	cmd.Flags().StringVar(&webAddr, "web", "127.0.0.1:8480",
		"address for the read-only status page; loopback by default, reach it with a port-forward (empty to disable)")
	return cmd
}

func newLogger(level string) *slog.Logger {
	var lv slog.Level
	switch level {
	case "debug":
		lv = slog.LevelDebug
	case "warn":
		lv = slog.LevelWarn
	case "error":
		lv = slog.LevelError
	default:
		lv = slog.LevelInfo
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: lv}))
}

// buildSecrets layers the configured backends. Order is deliberate: an explicit
// local file wins over a cloud parameter, which wins over the environment. That
// makes a local override for debugging obvious and temporary, rather than
// something that silently shadows the real source forever.
func buildSecrets(ctx context.Context, cfg *config.Config, log *slog.Logger) (secrets.Backend, error) {
	chain := secrets.Chain{}

	if cfg.Gateway.SecretsDir != "" {
		dir, err := secrets.NewDirBackend(cfg.Gateway.SecretsDir)
		if err != nil {
			return nil, err
		}
		chain = append(chain, dir)
		log.Info("secrets: directory backend", "dir", cfg.Gateway.SecretsDir)
	}

	if pfx := cfg.Gateway.SecretsSSM.Prefix; pfx != "" {
		ssmBackend, err := secrets.NewSSMBackend(ctx, secrets.SSMOptions{
			Prefix:   pfx,
			Region:   cfg.Gateway.SecretsSSM.Region,
			CacheTTL: cfg.Gateway.SecretsSSM.CacheTTL.Duration(),
		})
		if err != nil {
			return nil, err
		}
		chain = append(chain, ssmBackend)
		log.Info("secrets: ssm backend", "prefix", pfx)
	}

	chain = append(chain, secrets.NewEnvBackend())
	return chain, nil
}

func verifySecrets(cfg *config.Config, sec secrets.Backend, log *slog.Logger) error {
	var missing []string
	for _, name := range cfg.SecretRefs() {
		if _, err := sec.Get(name); err != nil {
			missing = append(missing, name)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	return fmt.Errorf("gateway: %d secret(s) referenced by the config could not be resolved: %v",
		len(missing), missing)
}

func buildSurfaces(cfg *config.Config, sec secrets.Backend, log *slog.Logger) (map[string]surface.Surface, error) {
	out := map[string]surface.Surface{}

	if cfg.Gateway.Slack.BotTokenSecret != "" {
		bot, err := sec.Get(cfg.Gateway.Slack.BotTokenSecret)
		if err != nil {
			return nil, fmt.Errorf("gateway: slack bot token: %w", err)
		}
		app, err := sec.Get(cfg.Gateway.Slack.AppTokenSecret)
		if err != nil {
			return nil, fmt.Errorf("gateway: slack app token: %w", err)
		}
		s, err := slackx.New(bot.Value, app.Value)
		if err != nil {
			return nil, err
		}
		out["slack"] = s
		log.Info("slack surface configured")
	}

	if len(out) == 0 {
		return nil, errors.New("gateway: no surfaces are configured; nothing could reach a runner")
	}
	return out, nil
}

func buildForge(cfg *config.Config, sec secrets.Backend, log *slog.Logger) (forge.Provider, error) {
	switch cfg.Gateway.Forge.Kind {
	case "":
		log.Warn("no forge configured; git credential requests will be refused")
		return nil, nil

	case config.ForgeGitHubApp:
		key, err := os.ReadFile(cfg.Gateway.Forge.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("gateway: read forge key: %w", err)
		}
		p, err := forge.NewGitHubApp(cfg.Gateway.Forge.AppID, cfg.Gateway.Forge.InstallationID, key)
		if err != nil {
			return nil, err
		}
		log.Info("forge configured", "kind", p.Name(), "app_id", cfg.Gateway.Forge.AppID)
		return p, nil

	case config.ForgeStatic:
		tok, err := sec.Get(cfg.Gateway.Forge.TokenSecret)
		if err != nil {
			return nil, fmt.Errorf("gateway: forge token: %w", err)
		}
		// A personal access token cannot be scoped per repository, so policy is
		// the only thing bounding it. The App path is the recommendation.
		log.Warn("forge is using a static token; per-repository scoping is unavailable")
		p := &forge.StaticToken{Username: cfg.Gateway.Forge.Username, Token: tok.Value}
		if tok.ExpiresAt != nil {
			p.Expires = *tok.ExpiresAt
		}
		return p, nil

	default:
		return nil, fmt.Errorf("gateway: unknown forge kind %q", cfg.Gateway.Forge.Kind)
	}
}
