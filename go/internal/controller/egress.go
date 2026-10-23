package controller

import (
	"encoding/base64"
	"errors"
	"io"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/RooobinYe/ai-pr-review-cloud/go/internal/run"
	"github.com/RooobinYe/ai-pr-review-cloud/go/internal/store"
)

// EgressProxy is fetch's only way out of the cluster (docs/architecture.md §5).
// The runner network policy confines the whole Pod, init container included,
// to DNS, LiteLLM and the controller; this CONNECT proxy then tunnels to the
// GitHub hosts in Config.EgressHosts. It requires the run's PROXY_TOKEN, which
// only the fetch container receives, so the runner container cannot use it.
//
// TLS runs end to end between fetch and GitHub; the proxy never sees the
// GitHub token.
func (c *Controller) EgressProxy(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodConnect {
		http.Error(w, "only CONNECT is proxied", http.StatusMethodNotAllowed)
		return
	}
	target := req.Host
	if !slices.Contains(c.cfg.EgressHosts, target) {
		http.Error(w, target+" is not an allowed destination", http.StatusForbidden)
		return
	}
	runID, token, ok := proxyCredentials(req)
	if !ok {
		// git first connects without credentials and retries on a 407 challenge.
		w.Header().Set("Proxy-Authenticate", `Basic realm="ai-pr-review"`)
		http.Error(w, "proxy credentials required", http.StatusProxyAuthRequired)
		return
	}
	ctx := req.Context()
	r, err := c.store.Get(ctx, runID)
	if errors.Is(err, store.ErrNotFound) || (err == nil && r.State != run.StateRunning) {
		http.Error(w, "no running run "+runID, http.StatusForbidden)
		return
	}
	if err != nil {
		c.log.Error("load run for egress", "run_id", runID, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if ok, err := c.tokenMatches(ctx, r, "PROXY_TOKEN", token); err != nil {
		c.runLog(r).Error("check proxy token", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	} else if !ok {
		http.Error(w, "wrong proxy token", http.StatusForbidden)
		return
	}

	upstream, err := (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, "tcp", target)
	if err != nil {
		http.Error(w, "dial "+target+": "+err.Error(), http.StatusBadGateway)
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		upstream.Close()
		http.Error(w, "connection cannot be hijacked", http.StatusInternalServerError)
		return
	}
	client, buf, err := hj.Hijack()
	if err != nil {
		upstream.Close()
		c.runLog(r).Error("hijack egress connection", "err", err)
		return
	}
	c.runLog(r).Info("egress tunnel opened", "target", target)
	if _, err := client.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
		client.Close()
		upstream.Close()
		return
	}
	tunnel(client, buf.Reader, upstream)
}

// tunnel copies bytes both ways until either side closes, then closes both.
// pending holds client bytes the HTTP server read ahead of the hijack.
func tunnel(client net.Conn, pending io.Reader, upstream net.Conn) {
	var once sync.Once
	closeBoth := func() { once.Do(func() { client.Close(); upstream.Close() }) }
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(upstream, io.MultiReader(pending, client))
		closeBoth()
	}()
	go func() {
		defer wg.Done()
		_, _ = io.Copy(client, upstream)
		closeBoth()
	}()
	wg.Wait()
}

// proxyCredentials reads "Proxy-Authorization: Basic base64(runID:token)".
func proxyCredentials(req *http.Request) (runID, token string, ok bool) {
	encoded, ok := strings.CutPrefix(req.Header.Get("Proxy-Authorization"), "Basic ")
	if !ok {
		return "", "", false
	}
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", "", false
	}
	runID, token, ok = strings.Cut(string(raw), ":")
	if !ok || !uuidPattern.MatchString(runID) || token == "" {
		return "", "", false
	}
	return runID, token, true
}
