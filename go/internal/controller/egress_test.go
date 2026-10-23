package controller

import (
	"context"
	"crypto/tls"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/RooobinYe/ai-pr-review-cloud/go/internal/run"
)

func TestEgressProxy(t *testing.T) {
	github := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "hello from github")
	}))
	defer github.Close()
	other := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer other.Close()

	h := newHarness()
	h.c.cfg.EgressHosts = []string{strings.TrimPrefix(github.URL, "https://")}
	h.reconcile()
	proxyToken := string(h.kube.secrets[runSecretName(h.state().JobName)].Data["PROXY_TOKEN"])
	proxy := httptest.NewServer(http.HandlerFunc(h.c.EgressProxy))
	defer proxy.Close()

	get := func(target string, user *url.Userinfo) (string, error) {
		proxyURL, _ := url.Parse(proxy.URL)
		proxyURL.User = user
		client := &http.Client{Transport: &http.Transport{
			Proxy:           http.ProxyURL(proxyURL),
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // test servers
		}}
		res, err := client.Get(target)
		if err != nil {
			return "", err
		}
		defer res.Body.Close()
		body, _ := io.ReadAll(res.Body)
		return string(body), nil
	}

	if body, err := get(github.URL, url.UserPassword(runID, proxyToken)); err != nil || body != "hello from github" {
		t.Fatalf("tunnel to GitHub: %q, %v", body, err)
	}
	denied := []struct {
		name   string
		target string
		user   *url.Userinfo
		want   string
	}{
		{"no credentials", github.URL, nil, "Proxy Authentication Required"},
		{"the runner's callback token", github.URL, url.UserPassword(runID, h.runToken()), "Forbidden"},
		{"another run", github.URL, url.UserPassword("0b4c1f6e-0000-4000-8000-000000000002", proxyToken), "Forbidden"},
		{"host not allowed", other.URL, url.UserPassword(runID, proxyToken), "Forbidden"},
	}
	for _, tt := range denied {
		if _, err := get(tt.target, tt.user); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: err %v, want %q", tt.name, err, tt.want)
		}
	}

	// Once the run has a result, fetch is long done and the proxy closes.
	if err := h.store.Transition(context.Background(), runID, run.StateRunning, run.StateFailed, "x"); err != nil {
		t.Fatal(err)
	}
	if _, err := get(github.URL, url.UserPassword(runID, proxyToken)); err == nil || !strings.Contains(err.Error(), "Forbidden") {
		t.Errorf("finished run: err %v, want Forbidden", err)
	}
}

func TestEgressProxyOnlyTunnels(t *testing.T) {
	h := newHarness()
	rec := httptest.NewRecorder()
	h.c.EgressProxy(rec, httptest.NewRequest(http.MethodGet, "http://github.com/", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("plain GET: %d, want 405", rec.Code)
	}
}
