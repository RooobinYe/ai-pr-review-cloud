// Package kube is a minimal Kubernetes API client for the few calls the
// controller makes: Jobs, per-run Secrets, and Pod status. It speaks the REST
// API directly to keep client-go's dependency tree out of the controller image.
package kube

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

var (
	ErrNotFound      = errors.New("not found")
	ErrAlreadyExists = errors.New("already exists")
)

const serviceAccountDir = "/var/run/secrets/kubernetes.io/serviceaccount"

// Client calls the API server for one namespace.
type Client struct {
	Host      string
	Namespace string
	HTTP      *http.Client
	// Token returns the bearer token for each request.
	Token func() (string, error)
}

// InCluster builds a client from the Pod's service account. The token file is
// re-read on every request because projected tokens rotate.
func InCluster(namespace string) (*Client, error) {
	host, port := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT")
	if host == "" || port == "" {
		return nil, errors.New("not running in a cluster: KUBERNETES_SERVICE_HOST/PORT unset")
	}
	ca, err := os.ReadFile(serviceAccountDir + "/ca.crt")
	if err != nil {
		return nil, fmt.Errorf("read service account CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		return nil, errors.New("service account CA has no certificates")
	}
	return &Client{
		Host:      "https://" + net.JoinHostPort(host, port),
		Namespace: namespace,
		HTTP: &http.Client{
			Timeout:   30 * time.Second,
			Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}},
		},
		Token: func() (string, error) {
			b, err := os.ReadFile(serviceAccountDir + "/token")
			return strings.TrimSpace(string(b)), err
		},
	}, nil
}

func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		data, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.Host+path, body)
	if err != nil {
		return err
	}
	if c.Token != nil {
		token, err := c.Token()
		if err != nil {
			return fmt.Errorf("kube token: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Accept", "application/json")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("kube %s %s: %w", method, path, err)
	}
	defer res.Body.Close()
	data, err := io.ReadAll(io.LimitReader(res.Body, 16<<20))
	if err != nil {
		return fmt.Errorf("kube %s %s: read body: %w", method, path, err)
	}
	switch {
	case res.StatusCode == http.StatusNotFound:
		return fmt.Errorf("kube %s %s: %w", method, path, ErrNotFound)
	case res.StatusCode == http.StatusConflict && method == http.MethodPost:
		return fmt.Errorf("kube %s %s: %w", method, path, ErrAlreadyExists)
	case res.StatusCode/100 != 2:
		var status struct {
			Message string `json:"message"`
		}
		msg := string(data)
		if json.Unmarshal(data, &status) == nil && status.Message != "" {
			msg = status.Message
		}
		return fmt.Errorf("kube %s %s: %d %s", method, path, res.StatusCode, msg)
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("kube %s %s: decode: %w", method, path, err)
		}
	}
	return nil
}

func (c *Client) path(group, resource, name string) string {
	p := fmt.Sprintf("/%s/namespaces/%s/%s", group, url.PathEscape(c.Namespace), resource)
	if name != "" {
		p += "/" + url.PathEscape(name)
	}
	return p
}

// CreateJob creates a Job and returns it as stored (with its UID).
func (c *Client) CreateJob(ctx context.Context, job Job) (Job, error) {
	job.APIVersion, job.Kind = "batch/v1", "Job"
	var out Job
	err := c.do(ctx, http.MethodPost, c.path("apis/batch/v1", "jobs", ""), job, &out)
	return out, err
}

func (c *Client) GetJob(ctx context.Context, name string) (Job, error) {
	var out Job
	err := c.do(ctx, http.MethodGet, c.path("apis/batch/v1", "jobs", name), nil, &out)
	return out, err
}

// DeleteJob deletes a Job together with its Pods; the Job's Secret goes with
// it through its owner reference.
func (c *Client) DeleteJob(ctx context.Context, name string) error {
	return c.do(ctx, http.MethodDelete, c.path("apis/batch/v1", "jobs", name)+"?propagationPolicy=Background", nil, nil)
}

func (c *Client) CreateSecret(ctx context.Context, s Secret) error {
	s.APIVersion, s.Kind = "v1", "Secret"
	return c.do(ctx, http.MethodPost, c.path("api/v1", "secrets", ""), s, nil)
}

func (c *Client) GetSecret(ctx context.Context, name string) (Secret, error) {
	var out Secret
	err := c.do(ctx, http.MethodGet, c.path("api/v1", "secrets", name), nil, &out)
	return out, err
}

// ListPods lists Pods matching a label selector such as "job-name=x".
func (c *Client) ListPods(ctx context.Context, selector string) ([]Pod, error) {
	var out struct {
		Items []Pod `json:"items"`
	}
	err := c.do(ctx, http.MethodGet, c.path("api/v1", "pods", "")+"?labelSelector="+url.QueryEscape(selector), nil, &out)
	return out.Items, err
}
