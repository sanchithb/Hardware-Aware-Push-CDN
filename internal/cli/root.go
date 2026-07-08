// Package cli implements the hpcdn command-line interface: run commands
// for the three node roles plus a full admin client for a running cluster.
package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/sanchithb/hardware-aware-push-cdn/internal/client"
	"github.com/sanchithb/hardware-aware-push-cdn/pkg/version"
)

var (
	flagController string
	flagAPIKey     string
	flagJSON       bool
)

// Root builds the hpcdn command tree.
func Root() *cobra.Command {
	root := &cobra.Command{
		Use:   "hpcdn",
		Short: "hpcdn — self-hosted, hardware-aware push CDN",
		Long: `hpcdn is a self-hosted content delivery network for live video and
static content. One binary runs all three roles:

  controller   control plane: node registry, routing, admin API, web console
  edge         cache node: serves viewers, receives pushes, pulls on miss
  origin       content source: watches a directory and pushes to edges

plus a complete admin client (status, nodes, tokens, sign, purge, bench…)
for operating a running cluster.`,
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	root.PersistentFlags().StringVar(&flagController, "controller",
		envOr("HPCDN_CONTROLLER", "http://127.0.0.1:8080"),
		"controller base URL (env HPCDN_CONTROLLER)")
	root.PersistentFlags().StringVar(&flagAPIKey, "api-key",
		os.Getenv("HPCDN_API_KEY"),
		"admin API key (env HPCDN_API_KEY; auto-discovered from the local controller data dir)")
	root.PersistentFlags().BoolVar(&flagJSON, "json", false, "output machine-readable JSON")

	root.AddCommand(
		controllerCmd(),
		edgeCmd(),
		originCmd(),
		statusCmd(),
		nodesCmd(),
		tokensCmd(),
		signCmd(),
		purgeCmd(),
		settingsCmd(),
		logsCmd(),
		benchCmd(),
		versionCmd(),
	)
	return root
}

// Execute runs the CLI and returns a process exit code.
func Execute() int {
	if err := Root().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		return 1
	}
	return 0
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// apiClient builds the admin client, auto-discovering the admin key from
// the local controller's data dir when talking to localhost.
func apiClient() (*client.Client, error) {
	key := flagAPIKey
	if key == "" && isLocal(flagController) {
		if base, err := os.UserConfigDir(); err == nil {
			p := filepath.Join(base, "hpcdn", "controller", "admin.key")
			if b, rerr := os.ReadFile(p); rerr == nil {
				key = strings.TrimSpace(string(b))
			}
		}
	}
	if key == "" {
		return nil, fmt.Errorf("no admin API key: pass --api-key, set HPCDN_API_KEY, or run on the controller host")
	}
	return client.New(flagController, key), nil
}

func isLocal(url string) bool {
	return strings.Contains(url, "127.0.0.1") || strings.Contains(url, "localhost")
}

// table prints an aligned text table.
func table(header []string, rows [][]string) {
	w := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
	fmt.Fprintln(w, strings.Join(header, "\t"))
	for _, r := range rows {
		fmt.Fprintln(w, strings.Join(r, "\t"))
	}
	w.Flush()
}

func humanBytes(b float64) string {
	units := []string{"B", "KiB", "MiB", "GiB", "TiB"}
	i := 0
	for b >= 1024 && i < len(units)-1 {
		b /= 1024
		i++
	}
	return fmt.Sprintf("%.1f %s", b, units[i])
}

func versionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print version information",
		Run: func(_ *cobra.Command, _ []string) {
			fmt.Println(version.String())
		},
	}
}
