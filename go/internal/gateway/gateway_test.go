package gateway

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/RooobinYe/ai-pr-review-cloud/go/internal/store"
)

type fakeRecorder struct {
	calls []store.NewRun
	seen  map[string]bool
	err   error
}

func (f *fakeRecorder) RecordTrigger(_ context.Context, delivery string, nr store.NewRun) (string, bool, error) {
	if f.err != nil {
		return "", false, f.err
	}
	f.calls = append(f.calls, nr)
	if f.seen[delivery] {
		return "", false, nil
	}
	f.seen[delivery] = true
	return "run-1", true, nil
}

const opened = `{"action":"opened","pull_request":{"number":5,"head":{"sha":"abc"}},
	"repository":{"full_name":"o/r"},"installation":{"id":9}}`

func sign(secret, body string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(body))
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func TestHandler(t *testing.T) {
	rec := &fakeRecorder{seen: map[string]bool{}}
	h := &Handler{Secret: []byte("s"), Recorder: rec, BudgetUSD: 0.5, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	send := func(event, body, sig, delivery string) int {
		req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(body))
		req.Header.Set("X-GitHub-Event", event)
		req.Header.Set("X-Hub-Signature-256", sig)
		req.Header.Set("X-GitHub-Delivery", delivery)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w.Code
	}

	tests := []struct {
		name, event, body, sig, delivery string
		want                             int
	}{
		{"bad signature", "pull_request", opened, sign("other", opened), "d1", http.StatusUnauthorized},
		{"ignored event", "push", `{}`, sign("s", `{}`), "d1", http.StatusNoContent},
		{"opened", "pull_request", opened, sign("s", opened), "d1", http.StatusAccepted},
		{"redelivery", "pull_request", opened, sign("s", opened), "d1", http.StatusAccepted},
		{"no delivery id", "pull_request", opened, sign("s", opened), "", http.StatusBadRequest},
	}
	for _, tt := range tests {
		if got := send(tt.event, tt.body, tt.sig, tt.delivery); got != tt.want {
			t.Errorf("%s: status %d, want %d", tt.name, got, tt.want)
		}
	}
	want := store.NewRun{Repo: "o/r", PRNumber: 5, HeadSHA: "abc", InstallationID: 9, BudgetUSD: 0.5}
	if len(rec.calls) != 2 || rec.calls[0] != want {
		t.Errorf("recorded %+v, want two calls of %+v", rec.calls, want)
	}

	rec.err = errors.New("db down")
	if got := send("pull_request", opened, sign("s", opened), "d2"); got != http.StatusServiceUnavailable {
		t.Errorf("store failure: status %d, want 503", got)
	}
}
