package cli

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/sanchithb/hardware-aware-push-cdn/internal/bench"
)

func benchCmd() *cobra.Command {
	var (
		concurrency int
		duration    time.Duration
		follow      bool
		insecure    bool
		signPath    string
	)
	cmd := &cobra.Command{
		Use:   "bench [url]",
		Short: "Load-test a URL with the built-in benchmark tool",
		Long: `Load-test any URL, or benchmark this cluster's routing path directly:

  hpcdn bench --sign /play/stream1/index.m3u8 -c 500 -d 30s
      mints a signed URL via the controller and hammers the routing endpoint

  hpcdn bench https://edge-1:8081/healthz -c 200 -d 15s
      benchmarks an arbitrary URL`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			url := ""
			if len(args) == 1 {
				url = args[0]
			}
			if signPath != "" {
				c, err := apiClient()
				if err != nil {
					return err
				}
				resp, err := c.Sign(cmd.Context(), signPath, "", time.Hour)
				if err != nil {
					return err
				}
				url = strings.TrimRight(flagController, "/") + resp.URL
				fmt.Fprintf(os.Stderr, "benchmarking signed routing URL:\n  %s\n\n", url)
			}
			if url == "" {
				return fmt.Errorf("pass a URL or --sign <path>")
			}
			res, err := bench.Run(cmd.Context(), bench.Options{
				URL: url, Concurrency: concurrency, Duration: duration,
				FollowRedirects: follow, Insecure: insecure,
			})
			if err != nil {
				return err
			}
			if flagJSON {
				printJSON(res)
				return nil
			}
			fmt.Print(bench.Format(res))
			return nil
		},
	}
	cmd.Flags().IntVarP(&concurrency, "concurrency", "c", 100, "concurrent virtual users")
	cmd.Flags().DurationVarP(&duration, "duration", "d", 15*time.Second, "test duration")
	cmd.Flags().BoolVar(&follow, "follow-redirects", false, "follow 302s to the edge (end-to-end path)")
	cmd.Flags().BoolVar(&insecure, "insecure", false, "skip TLS verification")
	cmd.Flags().StringVar(&signPath, "sign", "", "sign this /play path via the controller and benchmark it")
	return cmd
}
