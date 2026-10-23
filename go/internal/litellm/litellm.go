// Package litellm manages per-run virtual keys on a LiteLLM proxy
// (docs/architecture.md §5, §7.3). The proxy holds the real model key; a
// runner only ever sees a virtual key with a budget and an expiry.
package litellm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Client calls the proxy's key management API with the master key.
type Client struct {
	BaseURL   string
	MasterKey string
	HTTP      *http.Client
}

// Key describes a virtual key to generate.
type Key struct {
	// Alias shows up in LiteLLM's spend logs.
	Alias string
	// Models the key may call; empty means all.
	Models []string
	// MaxBudgetUSD is the most the key may spend; the proxy rejects requests
	// once it is reached.
	MaxBudgetUSD float64
	// Duration after which the key expires even if never deleted.
	Duration time.Duration
	Metadata map[string]string
}

// APIError is a non-2xx response from the proxy.
type APIError struct {
	Path    string
	Status  int
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("litellm %s: %d %s", e.Path, e.Status, e.Message)
}

func (c *Client) do(ctx context.Context, path string, in, out any) error {
	data, err := json.Marshal(in)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(c.BaseURL, "/")+path, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.MasterKey)
	req.Header.Set("Content-Type", "application/json")
	httpClient := c.HTTP
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	res, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("litellm %s: %w", path, err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("litellm %s: read body: %w", path, err)
	}
	if res.StatusCode/100 != 2 {
		var e struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		msg := strings.TrimSpace(string(body))
		if json.Unmarshal(body, &e) == nil && e.Error.Message != "" {
			msg = e.Error.Message
		}
		return &APIError{Path: path, Status: res.StatusCode, Message: msg}
	}
	if out != nil {
		if err := json.Unmarshal(body, out); err != nil {
			return fmt.Errorf("litellm %s: decode: %w", path, err)
		}
	}
	return nil
}

// Generate creates a virtual key and returns its secret value.
func (c *Client) Generate(ctx context.Context, k Key) (string, error) {
	in := map[string]any{
		"key_alias":  k.Alias,
		"max_budget": k.MaxBudgetUSD,
		"metadata":   k.Metadata,
	}
	if len(k.Models) > 0 {
		in["models"] = k.Models
	}
	if k.Duration > 0 {
		in["duration"] = fmt.Sprintf("%ds", int64(k.Duration.Seconds()))
	}
	var out struct {
		Key string `json:"key"`
	}
	if err := c.do(ctx, "/key/generate", in, &out); err != nil {
		return "", err
	}
	if out.Key == "" {
		return "", fmt.Errorf("litellm /key/generate: response has no key")
	}
	return out.Key, nil
}

// Delete revokes a virtual key. A key that is already gone is not an error.
func (c *Client) Delete(ctx context.Context, key string) error {
	err := c.do(ctx, "/key/delete", map[string]any{"keys": []string{key}}, nil)
	if e, ok := err.(*APIError); ok && e.Status == http.StatusNotFound {
		return nil
	}
	return err
}
