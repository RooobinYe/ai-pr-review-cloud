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
	calls     []store.NewRun
	approvals []store.Approval
	seen      map[string]bool
	err       error
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

const opened = `{"action":"opened","pull_request":{"number":5,"head":{"sha":"abababababababababababababababababababab"}},
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
	want := store.NewRun{Repo: "o/r", PRNumber: 5, HeadSHA: "abababababababababababababababababababab", InstallationID: 9, BudgetUSD: 0.5}
	if len(rec.calls) != 2 || rec.calls[0] != want {
		t.Errorf("recorded %+v, want two calls of %+v", rec.calls, want)
	}

	rec.err = errors.New("db down")
	if got := send("pull_request", opened, sign("s", opened), "d2"); got != http.StatusServiceUnavailable {
		t.Errorf("store failure: status %d, want 503", got)
	}
}

func (f *fakeRecorder) RecordApproval(_ context.Context, delivery string, a store.Approval) error {
	if f.err != nil {
		return f.err
	}
	if !f.seen[delivery] {
		f.approvals = append(f.approvals, a)
		f.seen[delivery] = true
	}
	return nil
}

func TestApprovalWebhook(t *testing.T) {
	rec := &fakeRecorder{seen: map[string]bool{}}
	h := &Handler{Secret: []byte("s"), Recorder: rec, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	body := `{"action":"created","repository":{"full_name":"o/r"},"installation":{"id":42},"issue":{"number":7,"pull_request":{}},"comment":{"body":"/merge","user":{"login":"human","type":"User"},"created_at":"2026-10-24T00:00:00Z"}}`
	for _, tt := range []struct {
		delivery, signature string
		failure             bool
		want                int
	}{
		{"d", "bad", false, 401}, {"", sign("s", body), false, 400}, {"d", sign("s", body), false, 202}, {"d", sign("s", body), false, 202}, {"e", sign("s", body), true, 503},
	} {
		req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(body))
		req.Header.Set("X-GitHub-Event", "issue_comment")
		req.Header.Set("X-GitHub-Delivery", tt.delivery)
		req.Header.Set("X-Hub-Signature-256", tt.signature)
		if tt.failure {
			rec.err = errors.New("db down")
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != tt.want {
			t.Fatalf("status %d want %d", w.Code, tt.want)
		}
	}
	if len(rec.calls) != 0 || len(rec.approvals) != 1 || rec.approvals[0].Actor != "human" {
		t.Fatalf("unexpected records %+v", rec)
	}
}

// A draft can receive commits and be reopened without creating a run. Marking
// the same head ready for review must then create the usual review trigger.
func TestDraftPullRequestLifecycle(t *testing.T) {
	rec := &fakeRecorder{seen: map[string]bool{}}
	h := &Handler{Secret: []byte("s"), Recorder: rec, BudgetUSD: 0.5, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	for _, tt := range []struct {
		action     string
		draft      string
		wantStatus int
		wantCalls  int
	}{
		{"opened", "true", http.StatusNoContent, 0},
		{"synchronize", "true", http.StatusNoContent, 0},
		{"reopened", "true", http.StatusNoContent, 0},
		{"ready_for_review", "false", http.StatusAccepted, 1},
	} {
		t.Run(tt.action, func(t *testing.T) {
			body := strings.Replace(opened, `"action":"opened"`, `"action":"`+tt.action+`"`, 1)
			body = strings.Replace(body, `"number":5`, `"number":5,"draft":`+tt.draft, 1)
			req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(body))
			req.Header.Set("X-GitHub-Event", "pull_request")
			req.Header.Set("X-Hub-Signature-256", sign("s", body))
			req.Header.Set("X-GitHub-Delivery", tt.action)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)
			if w.Code != tt.wantStatus || len(rec.calls) != tt.wantCalls {
				t.Fatalf("status=%d calls=%d, want status=%d calls=%d", w.Code, len(rec.calls), tt.wantStatus, tt.wantCalls)
			}
		})
	}
	want := store.NewRun{Repo: "o/r", PRNumber: 5, HeadSHA: "abababababababababababababababababababab", InstallationID: 9, BudgetUSD: 0.5}
	if len(rec.calls) != 1 || rec.calls[0] != want || len(rec.approvals) != 0 {
		t.Fatalf("unexpected records: %+v", rec)
	}
}
