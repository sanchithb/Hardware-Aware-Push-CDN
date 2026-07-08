package cli

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/sanchithb/hardware-aware-push-cdn/internal/controller"
	"github.com/sanchithb/hardware-aware-push-cdn/internal/edge"
	"github.com/sanchithb/hardware-aware-push-cdn/internal/origin"
	"github.com/sanchithb/hardware-aware-push-cdn/pkg/config"
	"github.com/sanchithb/hardware-aware-push-cdn/pkg/logx"
	"github.com/sanchithb/hardware-aware-push-cdn/web"
)

func signalContext() context.Context {
	ctx, _ := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	return ctx
}

func controllerCmd() *cobra.Command {
	var cfgPath string
	cfg := config.DefaultController()
	cmd := &cobra.Command{
		Use:   "controller",
		Short: "Run the control plane (registry, routing, admin API, web console)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			snap := snapshotChanged(cmd)
			if err := config.LoadFile(cfgPath, &cfg, cmd.Flags().Changed("config")); err != nil {
				return err
			}
			config.ApplyControllerEnv(&cfg)
			restoreChanged(cmd, snap)

			ring := logx.NewRing(500)
			log := logx.New(logx.Options{Level: cfg.LogLevel, Format: cfg.LogFormat, Ring: ring})

			ctrl, err := controller.New(cfg, log, ring)
			if err != nil {
				return err
			}
			ctrl.ConsoleFS = web.Console()
			if ctrl.AdminKey != "" {
				fmt.Fprintf(os.Stderr, `
┌──────────────────────────────────────────────────────────────────────┐
  Admin API key (shown once, stored hashed):

    %s

  Use it for the CLI and the web console:
    export HPCDN_API_KEY=%s
    hpcdn status
└──────────────────────────────────────────────────────────────────────┘

`, ctrl.AdminKey, ctrl.AdminKey)
			}
			return ctrl.Run(signalContext())
		},
	}
	cmd.Flags().StringVar(&cfgPath, "config", "hpcdn-controller.yaml", "YAML config file")
	addCommonFlags(cmd, &cfg.Common)
	cmd.Flags().StringVar(&cfg.RoutingMode, "routing-mode", cfg.RoutingMode, "redirect | proxy")
	cmd.Flags().StringVar(&cfg.AdminKey, "admin-key", "", "set the admin API key explicitly (default: generate once)")
	return cmd
}

func edgeCmd() *cobra.Command {
	var cfgPath string
	cfg := config.DefaultEdge()
	cmd := &cobra.Command{
		Use:   "edge",
		Short: "Run an edge cache node",
		RunE: func(cmd *cobra.Command, _ []string) error {
			snap := snapshotChanged(cmd)
			if err := config.LoadFile(cfgPath, &cfg, cmd.Flags().Changed("config")); err != nil {
				return err
			}
			config.ApplyEdgeEnv(&cfg)
			restoreChanged(cmd, snap)

			log := logx.New(logx.Options{Level: cfg.LogLevel, Format: cfg.LogFormat})
			e := &edge.Edge{Cfg: cfg, Log: log}
			return e.Run(signalContext())
		},
	}
	cmd.Flags().StringVar(&cfgPath, "config", "hpcdn-edge.yaml", "YAML config file")
	addCommonFlags(cmd, &cfg.Common)
	addNodeFlags(cmd, &cfg.Node)
	cmd.Flags().StringVar(&cfg.CacheDir, "cache-dir", "", "content cache directory (default: <data-dir>/cache)")
	cmd.Flags().Int64Var(&cfg.CacheMaxBytes, "cache-max-bytes", cfg.CacheMaxBytes, "cache size budget in bytes (SIEVE eviction)")
	cmd.Flags().IntVar(&cfg.Capacity, "capacity", cfg.Capacity, "soft concurrent-connection capacity for load scoring")
	cmd.Flags().DurationVar(&cfg.HoldTimeout, "hold-timeout", cfg.HoldTimeout, "max wait for a not-yet-arrived live segment")
	return cmd
}

func originCmd() *cobra.Command {
	var cfgPath string
	cfg := config.DefaultOrigin()
	cmd := &cobra.Command{
		Use:   "origin",
		Short: "Run an origin node (watches a directory, pushes to edges)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			snap := snapshotChanged(cmd)
			if err := config.LoadFile(cfgPath, &cfg, cmd.Flags().Changed("config")); err != nil {
				return err
			}
			config.ApplyOriginEnv(&cfg)
			restoreChanged(cmd, snap)

			log := logx.New(logx.Options{Level: cfg.LogLevel, Format: cfg.LogFormat})
			o := &origin.Origin{Cfg: cfg, Log: log}
			return o.Run(signalContext())
		},
	}
	cmd.Flags().StringVar(&cfgPath, "config", "hpcdn-origin.yaml", "YAML config file")
	addCommonFlags(cmd, &cfg.Common)
	addNodeFlags(cmd, &cfg.Node)
	cmd.Flags().StringVar(&cfg.WatchDir, "watch-dir", cfg.WatchDir, "content directory to watch and distribute")
	cmd.Flags().IntVar(&cfg.PushWorkers, "push-workers", cfg.PushWorkers, "concurrent push workers")
	cmd.Flags().DurationVar(&cfg.DebounceWindow, "debounce", cfg.DebounceWindow, "quiet window before a written file is pushed")
	return cmd
}

// addCommonFlags binds shared server flags directly onto cfg fields.
func addCommonFlags(cmd *cobra.Command, c *config.Common) {
	cmd.Flags().StringVar(&c.Listen, "listen", c.Listen, "listen address (host:port)")
	cmd.Flags().StringVar(&c.PublicURL, "public-url", c.PublicURL, "URL peers/clients use to reach this node")
	cmd.Flags().StringVar(&c.DataDir, "data-dir", c.DataDir, "state directory")
	cmd.Flags().StringVar(&c.LogLevel, "log-level", c.LogLevel, "debug | info | warn | error")
	cmd.Flags().StringVar(&c.LogFormat, "log-format", c.LogFormat, "text | json")
	cmd.Flags().BoolVar(&c.TLS.Enabled, "tls", c.TLS.Enabled, "serve HTTPS (self-signed unless cert/key given)")
	cmd.Flags().StringVar(&c.TLS.CertFile, "tls-cert", c.TLS.CertFile, "TLS certificate file")
	cmd.Flags().StringVar(&c.TLS.KeyFile, "tls-key", c.TLS.KeyFile, "TLS key file")
}

func addNodeFlags(cmd *cobra.Command, n *config.Node) {
	cmd.Flags().StringVar(&n.ControllerURL, "controller-url", n.ControllerURL, "controller base URL")
	cmd.Flags().StringVar(&n.JoinToken, "join-token", n.JoinToken, "enrollment token (first start only; env HPCDN_JOIN_TOKEN)")
	cmd.Flags().StringVar(&n.Name, "name", n.Name, "node name (default: hostname)")
	cmd.Flags().StringVar(&n.Region, "region", n.Region, "point-of-presence region label, e.g. us-east")
	cmd.Flags().BoolVar(&n.InsecureSkipVerify, "insecure-skip-verify", n.InsecureSkipVerify, "skip TLS verification for intra-cluster calls (lab use)")
}

// Precedence: defaults < YAML < env < flags. Flags bind by pointer into
// cfg, so the YAML/env pass would clobber explicitly-set flags; snapshot
// them before loading and restore after.
func snapshotChanged(cmd *cobra.Command) map[string]string {
	m := map[string]string{}
	cmd.Flags().Visit(func(f *pflag.Flag) { m[f.Name] = f.Value.String() })
	return m
}

func restoreChanged(cmd *cobra.Command, m map[string]string) {
	for k, v := range m {
		_ = cmd.Flags().Set(k, v)
	}
}
