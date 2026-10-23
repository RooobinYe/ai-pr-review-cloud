package controller

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/RooobinYe/ai-pr-review-cloud/go/internal/kube"
	"github.com/RooobinYe/ai-pr-review-cloud/go/internal/review"
	"github.com/RooobinYe/ai-pr-review-cloud/go/internal/store"
)

func (h *harness) putUsage(body, token string) int {
	req := httptest.NewRequest(http.MethodPut, "/runs/"+runID+"/usage", bytes.NewReader([]byte(body)))
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	h.mux.ServeHTTP(rec, req)
	return rec.Code
}

func TestUsageReports(t *testing.T) {
	h := newHarness()
	h.reconcile()
	turn := func(n int, cost string) string {
		return `{"turn":` + string(rune('0'+n)) + `,"model":"deepseek/deepseek-flash","input_tokens":100,"output_tokens":20,"cache_read_tokens":900,"cache_write_tokens":0,"cost_usd":` + cost + `}`
	}
	for _, tc := range []struct {
		name, body, token string
		want              int
	}{
		{"first turn", turn(1, "0.01"), h.runToken(), http.StatusNoContent},
		{"repeat overwrites", turn(1, "0.02"), h.runToken(), http.StatusNoContent},
		{"second turn", turn(2, "0.03"), h.runToken(), http.StatusNoContent},
		{"wrong token", turn(3, "0.01"), "wrong", http.StatusUnauthorized},
		{"turn zero", turn(0, "0.01"), h.runToken(), http.StatusBadRequest},
		{"negative cost", turn(3, "-1"), h.runToken(), http.StatusBadRequest},
		{"missing model", `{"turn":1}`, h.runToken(), http.StatusBadRequest},
		{"negative tokens", `{"turn":1,"model":"m","input_tokens":-1}`, h.runToken(), http.StatusBadRequest},
		{"unknown field", `{"turn":3,"model":"m","extra":1}`, h.runToken(), http.StatusBadRequest},
		{"not json", `turn 3`, h.runToken(), http.StatusBadRequest},
	} {
		if code := h.putUsage(tc.body, tc.token); code != tc.want {
			t.Errorf("%s: %d, want %d", tc.name, code, tc.want)
		}
	}
	got := h.store.usage[runID]
	want := store.Usage{Turn: 2, Model: "deepseek/deepseek-flash", InputTokens: 100, OutputTokens: 20, CacheReadTokens: 900, CostUSD: 0.03}
	if len(got) != 2 || got[1].CostUSD != 0.02 || got[2] != want {
		t.Errorf("stored usage %+v", got)
	}

	h.post(result(review.VerdictApprove))
	h.reconcile()
	if code := h.putUsage(turn(3, "0.01"), h.runToken()); code != http.StatusNoContent {
		t.Errorf("usage after the run finished: %d, want 204", code)
	}
}

func TestValidUsage(t *testing.T) {
	ok := store.Usage{Turn: 1, Model: "m", InputTokens: 1, CostUSD: 0.1}
	if err := ValidUsage(ok); err != nil {
		t.Errorf("valid usage rejected: %v", err)
	}
	for name, mutate := range map[string]func(*store.Usage){
		"turn":   func(u *store.Usage) { u.Turn = 0 },
		"model":  func(u *store.Usage) { u.Model = "" },
		"tokens": func(u *store.Usage) { u.CacheWriteTokens = -1 },
		"cost":   func(u *store.Usage) { u.CostUSD = 1e6 },
	} {
		u := ok
		mutate(&u)
		if ValidUsage(u) == nil {
			t.Errorf("%s: invalid usage accepted: %+v", name, u)
		}
	}
}

func terminated(name string, code int32, started, finished time.Time) kube.ContainerStatus {
	return kube.ContainerStatus{Name: name, State: kube.ContainerState{Terminated: &kube.ContainerStateTerminated{ExitCode: code, StartedAt: started, FinishedAt: finished}}}
}

func TestFetchTiming(t *testing.T) {
	t0 := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	t1 := t0.Add(7 * time.Second)
	waiting := kube.ContainerStatus{Name: "fetch", State: kube.ContainerState{Waiting: &kube.ContainerStateWaiting{Reason: "PodInitializing"}}}
	for _, tc := range []struct {
		name   string
		status []kube.ContainerStatus
		ok     bool
	}{
		{"finished", []kube.ContainerStatus{terminated("fetch", 0, t0, t1)}, true},
		{"failed", []kube.ContainerStatus{terminated("fetch", 1, t0, t1)}, false},
		{"still running", []kube.ContainerStatus{waiting}, false},
		{"other init container", []kube.ContainerStatus{terminated("setup", 0, t0, t1)}, false},
		{"no status yet", nil, false},
	} {
		started, finished, ok := FetchTiming(kube.Pod{Status: kube.PodStatus{InitContainerStatuses: tc.status}})
		if ok != tc.ok || (ok && (!started.Equal(t0) || !finished.Equal(t1))) {
			t.Errorf("%s: got %v %v %v", tc.name, started, finished, ok)
		}
	}
}

func TestFetchTimingRecordedOnPublish(t *testing.T) {
	h := newHarness()
	h.reconcile()
	t0 := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	h.kube.pods = []kube.Pod{{Status: kube.PodStatus{InitContainerStatuses: []kube.ContainerStatus{terminated("fetch", 0, t0, t0.Add(12*time.Second))}}}}
	h.post(result(review.VerdictApprove))
	h.reconcile()
	if got := h.store.fetchSeconds[runID]; got != 12 || !h.state().FetchTimed {
		t.Errorf("fetch seconds %v, timed %v; want 12, true", got, h.state().FetchTimed)
	}
}
