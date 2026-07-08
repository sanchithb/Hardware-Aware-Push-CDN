// Package client is a typed Go client for the controller's admin API.
// The hpcdn CLI is built on it; it is also usable as a library for
// automation against a running cluster.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/sanchithb/hardware-aware-push-cdn/internal/protocol"
	"github.com/sanchithb/hardware-aware-push-cdn/pkg/logx"
	"github.com/sanchithb/hardware-aware-push-cdn/pkg/version"
)

// Client talks to one controller.
type Client struct {
	BaseURL string
	APIKey  string
	HTTP    *http.Client
}

// New creates a Client with sane defaults.
func New(baseURL, apiKey string) *Client {
	return &Client{
		BaseURL: strings.TrimRight(baseURL, "/"),
		APIKey:  apiKey,
		HTTP:    &http.Client{Timeout: 15 * time.Second},
	}
}

// APIError is a non-2xx response from the controller.
type APIError struct {
	Status int
	Msg    string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("controller returned %d: %s", e.Status, e.Msg)
}

func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	req.Header.Set("User-Agent", version.UserAgent())
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("cannot reach controller at %s: %w", c.BaseURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		var eb struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&eb)
		return &APIError{Status: resp.StatusCode, Msg: eb.Error}
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

// Stats fetches the cluster overview.
func (c *Client) Stats(ctx context.Context) (protocol.ClusterStats, error) {
	var s protocol.ClusterStats
	err := c.do(ctx, http.MethodGet, "/api/v1/stats", nil, &s)
	return s, err
}

// Nodes lists all nodes.
func (c *Client) Nodes(ctx context.Context) ([]protocol.NodeStatus, error) {
	var n []protocol.NodeStatus
	err := c.do(ctx, http.MethodGet, "/api/v1/nodes", nil, &n)
	return n, err
}

// Node fetches one node.
func (c *Client) Node(ctx context.Context, id string) (protocol.NodeStatus, error) {
	var n protocol.NodeStatus
	err := c.do(ctx, http.MethodGet, "/api/v1/nodes/"+id, nil, &n)
	return n, err
}

// Telemetry fetches a node's history.
func (c *Client) Telemetry(ctx context.Context, id string) ([]protocol.TelemetrySample, error) {
	var t []protocol.TelemetrySample
	err := c.do(ctx, http.MethodGet, "/api/v1/nodes/"+id+"/telemetry", nil, &t)
	return t, err
}

// Drain sets or clears drain mode on a node.
func (c *Client) Drain(ctx context.Context, id string, drain bool) error {
	action := "drain"
	if !drain {
		action = "undrain"
	}
	return c.do(ctx, http.MethodPost, "/api/v1/nodes/"+id+"/"+action, nil, nil)
}

// RemoveNode deregisters a node.
func (c *Client) RemoveNode(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, "/api/v1/nodes/"+id, nil, nil)
}

// Settings fetches routing settings.
func (c *Client) Settings(ctx context.Context) (protocol.Settings, error) {
	var s protocol.Settings
	err := c.do(ctx, http.MethodGet, "/api/v1/settings", nil, &s)
	return s, err
}

// UpdateSettings replaces routing settings.
func (c *Client) UpdateSettings(ctx context.Context, s protocol.Settings) error {
	return c.do(ctx, http.MethodPut, "/api/v1/settings", s, nil)
}

// CreateToken mints a join token.
func (c *Client) CreateToken(ctx context.Context, note string, ttl time.Duration, maxUses int) (protocol.JoinTokenInfo, error) {
	req := map[string]any{"note": note, "ttl_seconds": int(ttl.Seconds()), "max_uses": maxUses}
	var info protocol.JoinTokenInfo
	err := c.do(ctx, http.MethodPost, "/api/v1/tokens", req, &info)
	return info, err
}

// Tokens lists join tokens.
func (c *Client) Tokens(ctx context.Context) ([]protocol.JoinTokenInfo, error) {
	var t []protocol.JoinTokenInfo
	err := c.do(ctx, http.MethodGet, "/api/v1/tokens", nil, &t)
	return t, err
}

// DeleteToken revokes a join token.
func (c *Client) DeleteToken(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, "/api/v1/tokens/"+id, nil, nil)
}

// Sign mints a signed playback URL.
func (c *Client) Sign(ctx context.Context, path, scope string, ttl time.Duration) (protocol.SignResponse, error) {
	req := protocol.SignRequest{Path: path, Scope: scope, TTLSeconds: int(ttl.Seconds())}
	var resp protocol.SignResponse
	err := c.do(ctx, http.MethodPost, "/api/v1/sign", req, &resp)
	return resp, err
}

// Purge broadcasts a purge.
func (c *Client) Purge(ctx context.Context, path string) (int, error) {
	var resp map[string]int
	err := c.do(ctx, http.MethodPost, "/api/v1/purge", protocol.PurgeRequest{Path: path}, &resp)
	return resp["edges_notified"], err
}

// Logs fetches the controller's recent log ring.
func (c *Client) Logs(ctx context.Context) ([]logx.Entry, error) {
	var e []logx.Entry
	err := c.do(ctx, http.MethodGet, "/api/v1/logs", nil, &e)
	return e, err
}
