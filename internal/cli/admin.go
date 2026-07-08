package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/sanchithb/hardware-aware-push-cdn/internal/protocol"
)

func printJSON(v any) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func statusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show the cluster overview",
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := apiClient()
			if err != nil {
				return err
			}
			st, err := c.Stats(cmd.Context())
			if err != nil {
				return err
			}
			if flagJSON {
				printJSON(st)
				return nil
			}
			fmt.Printf("Cluster:        %s\n", flagController)
			fmt.Printf("Controller up:  %s\n", (time.Duration(st.UptimeSeconds) * time.Second))
			fmt.Printf("Edges:          %d healthy / %d total\n", st.EdgesHealthy, st.EdgesTotal)
			fmt.Printf("Origins:        %d\n", st.OriginsTotal)
			fmt.Printf("Routing:        %.1f req/s now, %d sessions total\n", st.RoutedPerSec, st.RoutedTotal)
			fmt.Printf("Egress:         %s/s\n", humanBytes(st.BytesOutRate))
			fmt.Printf("Cache:          %s stored, %.1f%% hit ratio\n", humanBytes(float64(st.CacheBytes)), st.HitRatio*100)
			return nil
		},
	}
}

func nodesCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "nodes",
		Short: "Inspect and manage cluster nodes",
	}
	list := &cobra.Command{
		Use:   "list",
		Short: "List all nodes with live telemetry",
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := apiClient()
			if err != nil {
				return err
			}
			nodes, err := c.Nodes(cmd.Context())
			if err != nil {
				return err
			}
			if flagJSON {
				printJSON(nodes)
				return nil
			}
			rows := make([][]string, 0, len(nodes))
			for _, n := range nodes {
				rows = append(rows, []string{
					n.ID, string(n.Kind), n.Name, n.Region, n.State,
					fmt.Sprintf("%.0f", n.Score),
					fmt.Sprintf("%.0f%%", n.CPUPercent),
					fmt.Sprintf("%.0f%%", n.RAMPercent),
					strconv.Itoa(n.ActiveConns),
					humanBytes(n.BytesOutRate) + "/s",
					fmt.Sprintf("%.0f%%", n.HitRatio*100),
				})
			}
			table([]string{"ID", "KIND", "NAME", "REGION", "STATE", "SCORE", "CPU", "RAM", "CONNS", "EGRESS", "HIT"}, rows)
			return nil
		},
	}
	get := &cobra.Command{
		Use:   "get <node-id>",
		Short: "Show one node in detail",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient()
			if err != nil {
				return err
			}
			n, err := c.Node(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			printJSON(n)
			return nil
		},
	}
	drain := &cobra.Command{
		Use:   "drain <node-id>",
		Short: "Gracefully remove a node from routing (existing sessions finish)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient()
			if err != nil {
				return err
			}
			if err := c.Drain(cmd.Context(), args[0], true); err != nil {
				return err
			}
			fmt.Printf("node %s is draining\n", args[0])
			return nil
		},
	}
	undrain := &cobra.Command{
		Use:   "undrain <node-id>",
		Short: "Return a drained node to the routing pool",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient()
			if err != nil {
				return err
			}
			if err := c.Drain(cmd.Context(), args[0], false); err != nil {
				return err
			}
			fmt.Printf("node %s resumed\n", args[0])
			return nil
		},
	}
	remove := &cobra.Command{
		Use:   "remove <node-id>",
		Short: "Deregister a node permanently",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient()
			if err != nil {
				return err
			}
			if err := c.RemoveNode(cmd.Context(), args[0]); err != nil {
				return err
			}
			fmt.Printf("node %s removed\n", args[0])
			return nil
		},
	}
	cmd.AddCommand(list, get, drain, undrain, remove)
	return cmd
}

func tokensCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "tokens",
		Short: "Manage node join tokens",
	}
	var note string
	var ttl time.Duration
	var maxUses int
	create := &cobra.Command{
		Use:   "create",
		Short: "Mint a join token for enrolling new nodes",
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := apiClient()
			if err != nil {
				return err
			}
			info, err := c.CreateToken(cmd.Context(), note, ttl, maxUses)
			if err != nil {
				return err
			}
			if flagJSON {
				printJSON(info)
				return nil
			}
			fmt.Printf("Join token (shown once):\n\n  %s\n\nEnroll a node with:\n  hpcdn edge --controller-url %s --join-token %s\n",
				info.Token, flagController, info.Token)
			return nil
		},
	}
	create.Flags().StringVar(&note, "note", "", "what this token is for")
	create.Flags().DurationVar(&ttl, "ttl", 0, "expiry (e.g. 24h); 0 = never")
	create.Flags().IntVar(&maxUses, "max-uses", 0, "how many nodes may enroll with it; 0 = unlimited")

	list := &cobra.Command{
		Use:   "list",
		Short: "List join tokens",
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := apiClient()
			if err != nil {
				return err
			}
			toks, err := c.Tokens(cmd.Context())
			if err != nil {
				return err
			}
			if flagJSON {
				printJSON(toks)
				return nil
			}
			rows := make([][]string, 0, len(toks))
			for _, t := range toks {
				exp := "never"
				if t.ExpiresAt != nil {
					exp = t.ExpiresAt.Local().Format(time.RFC3339)
				}
				uses := strconv.Itoa(t.Uses)
				if t.MaxUses > 0 {
					uses += "/" + strconv.Itoa(t.MaxUses)
				}
				rows = append(rows, []string{t.ID, t.Note, t.CreatedAt.Local().Format("2006-01-02 15:04"), exp, uses})
			}
			table([]string{"ID", "NOTE", "CREATED", "EXPIRES", "USES"}, rows)
			return nil
		},
	}
	revoke := &cobra.Command{
		Use:   "revoke <token-id>",
		Short: "Revoke a join token",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient()
			if err != nil {
				return err
			}
			if err := c.DeleteToken(cmd.Context(), args[0]); err != nil {
				return err
			}
			fmt.Printf("token %s revoked\n", args[0])
			return nil
		},
	}
	cmd.AddCommand(create, list, revoke)
	return cmd
}

func signCmd() *cobra.Command {
	var ttl time.Duration
	var scope string
	cmd := &cobra.Command{
		Use:   "sign <path>",
		Short: "Mint a signed playback URL (e.g. hpcdn sign /play/stream1/index.m3u8)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient()
			if err != nil {
				return err
			}
			resp, err := c.Sign(cmd.Context(), args[0], scope, ttl)
			if err != nil {
				return err
			}
			if flagJSON {
				printJSON(resp)
				return nil
			}
			fmt.Printf("%s%s\n", flagController, resp.URL)
			fmt.Fprintf(os.Stderr, "expires: %s\n", resp.ExpiresAt.Local().Format(time.RFC3339))
			return nil
		},
	}
	cmd.Flags().DurationVar(&ttl, "ttl", 0, "signature lifetime (default: cluster setting)")
	cmd.Flags().StringVar(&scope, "scope", "", "path prefix the signature covers (default: the stream directory)")
	return cmd
}

func purgeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "purge <path-prefix>",
		Short: "Purge cached content from every edge (e.g. hpcdn purge stream1)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient()
			if err != nil {
				return err
			}
			n, err := c.Purge(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			fmt.Printf("purge of %q queued to %d edge node(s)\n", args[0], n)
			return nil
		},
	}
}

func settingsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "settings",
		Short: "View or tune live routing settings",
	}
	get := &cobra.Command{
		Use:   "get",
		Short: "Show current routing settings",
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := apiClient()
			if err != nil {
				return err
			}
			s, err := c.Settings(cmd.Context())
			if err != nil {
				return err
			}
			printJSON(s)
			return nil
		},
	}
	set := &cobra.Command{
		Use:   "set key=value [key=value…]",
		Short: "Update routing settings live (e.g. hpcdn settings set cpu_weight=0.6 region_penalty=40)",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient()
			if err != nil {
				return err
			}
			s, err := c.Settings(cmd.Context())
			if err != nil {
				return err
			}
			if err := applySettingsKVs(&s, args); err != nil {
				return err
			}
			if err := c.UpdateSettings(cmd.Context(), s); err != nil {
				return err
			}
			printJSON(s)
			return nil
		},
	}
	cmd.AddCommand(get, set)
	return cmd
}

// applySettingsKVs mutates s from key=value pairs using the JSON field names.
func applySettingsKVs(s *protocol.Settings, kvs []string) error {
	// Round-trip through JSON so CLI keys match the API exactly.
	m := map[string]any{}
	b, _ := json.Marshal(s)
	_ = json.Unmarshal(b, &m)
	for _, kv := range kvs {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			return fmt.Errorf("expected key=value, got %q", kv)
		}
		if _, exists := m[k]; !exists {
			return fmt.Errorf("unknown setting %q (see `hpcdn settings get`)", k)
		}
		switch v {
		case "true", "false":
			m[k] = v == "true"
		default:
			f, err := strconv.ParseFloat(v, 64)
			if err != nil {
				return fmt.Errorf("setting %q: %q is not a number or bool", k, v)
			}
			m[k] = f
		}
	}
	b, _ = json.Marshal(m)
	return json.Unmarshal(b, s)
}

func logsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "logs",
		Short: "Show recent controller log entries",
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := apiClient()
			if err != nil {
				return err
			}
			entries, err := c.Logs(cmd.Context())
			if err != nil {
				return err
			}
			if flagJSON {
				printJSON(entries)
				return nil
			}
			for _, e := range entries {
				fmt.Printf("%s %-5s %s  %s\n", e.Time.Local().Format("15:04:05.000"), e.Level, e.Message, e.Attrs)
			}
			return nil
		},
	}
}
