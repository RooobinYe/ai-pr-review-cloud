package controller

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/RooobinYe/ai-pr-review-cloud/go/internal/githubapp"
	"github.com/RooobinYe/ai-pr-review-cloud/go/internal/kube"
	"github.com/RooobinYe/ai-pr-review-cloud/go/internal/review"
	"github.com/RooobinYe/ai-pr-review-cloud/go/internal/run"
	"github.com/RooobinYe/ai-pr-review-cloud/go/internal/store"
)

func TestReviewEvent(t *testing.T) {
	tests := map[review.Verdict]string{
		review.VerdictApprove:        "APPROVE",
		review.VerdictRequestChanges: "COMMENT", // never REQUEST_CHANGES: it would block merging
	}
	for v, want := range tests {
		if got := ReviewEvent(v); got != want {
			t.Errorf("ReviewEvent(%s) = %s, want %s", v, got, want)
		}
	}
}

func TestStaleApprovals(t *testing.T) {
	reviews := []githubapp.Review{
		approvedReview(1, bot, oldSHA),
		approvedReview(2, bot, headSHA),               // already for this commit
		approvedReview(3, "alice", oldSHA),            // humans are left alone
		{ID: 4, State: "COMMENTED", CommitID: oldSHA}, // not an approval
		{ID: 5, State: "DISMISSED", CommitID: oldSHA},
	}
	reviews[3].User.Login, reviews[4].User.Login = bot, bot
	if got := StaleApprovals(reviews, bot, headSHA); !slices.Equal(got, []int64{1}) {
		t.Errorf("StaleApprovals = %v, want [1]", got)
	}
}

func TestBuildFoldedReviewOrdersBySeverity(t *testing.T) {
	res := result(review.VerdictRequestChanges,
		review.Comment{Path: "b.go", Line: 2, Severity: review.SeverityLow, Confidence: review.ConfidenceLow, Body: "low"},
		review.Comment{Path: "a.go", Line: 9, Severity: review.SeverityCritical, Confidence: review.ConfidenceHigh, Body: "crit"},
	)
	body := BuildFoldedReview(runID, res, "COMMENT").Body
	if strings.Index(body, "`a.go:9`") > strings.Index(body, "`b.go:2`") {
		t.Errorf("critical comment not first:\n%s", body)
	}
	if !strings.HasSuffix(body, "`deepseek/deepseek-flash` · 2222222</sub>\n<!-- ai-pr-review run="+runID+" -->") {
		t.Errorf("missing footer or vote marker:\n%s", body)
	}
}

func TestFindVote(t *testing.T) {
	mine := BuildReview(runID, result(review.VerdictApprove), "APPROVE").Body
	other := BuildReview("0b4c1f6e-0000-4000-8000-000000000002", result(review.VerdictApprove), "APPROVE").Body
	review := func(id int64, login, state, body string) githubapp.Review {
		r := githubapp.Review{ID: id, State: state, Body: body}
		r.User.Login = login
		return r
	}
	tests := []struct {
		name    string
		reviews []githubapp.Review
		want    Vote
		found   bool
	}{
		{"none", nil, Vote{}, false},
		{"another run's vote", []githubapp.Review{review(1, bot, "APPROVED", other)}, Vote{}, false},
		{"someone quoting the marker", []githubapp.Review{review(1, "mallory", "APPROVED", mine)}, Vote{}, false},
		{"approval", []githubapp.Review{review(1, bot, "APPROVED", other), review(2, bot, "APPROVED", mine)}, Vote{ID: 2, Event: "APPROVE"}, true},
		{"comment", []githubapp.Review{review(3, bot, "COMMENTED", mine)}, Vote{ID: 3, Event: "COMMENT"}, true},
		{"dismissed approval no longer counts", []githubapp.Review{review(4, bot, "DISMISSED", mine)}, Vote{ID: 4, Event: "COMMENT"}, true},
	}
	for _, tt := range tests {
		if got, found := FindVote(tt.reviews, bot, runID); got != tt.want || found != tt.found {
			t.Errorf("%s: FindVote = %+v, %v; want %+v, %v", tt.name, got, found, tt.want, tt.found)
		}
	}
}

func TestRedeliveries(t *testing.T) {
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	at := func(id int64, guid string, status int, ago time.Duration) githubapp.Delivery {
		return githubapp.Delivery{ID: id, GUID: guid, StatusCode: status, DeliveredAt: now.Add(-ago)}
	}
	ds := []githubapp.Delivery{
		at(1, "ok", 202, time.Minute),
		at(2, "failed", 503, time.Minute),
		at(3, "retried-ok", 202, time.Minute), at(4, "retried-ok", 0, time.Hour),
		at(5, "retrying", 502, 2*time.Minute), at(6, "retrying", 0, time.Hour), // newest attempt is 5
		at(7, "ignored", 204, time.Minute), // an event the gateway does not act on still succeeded
		at(8, "ancient", 500, 25*time.Hour),
	}
	for i := range maxDeliveryAttempts {
		ds = append(ds, at(int64(100+i), "exhausted", 500, time.Duration(i)*time.Minute))
	}
	if got := Redeliveries(ds, now); !slices.Equal(got, []int64{2, 5}) {
		t.Errorf("Redeliveries = %v, want [2 5]", got)
	}
}

func TestTruncateKeepsUTF8(t *testing.T) {
	if got := truncate("评审结论", 4); got != "评…" {
		t.Errorf("truncate = %q", got)
	}
	if got := truncateFront("评审结论", 4); got != "…论" {
		t.Errorf("truncateFront = %q", got)
	}
}

func TestBuildJob(t *testing.T) {
	cfg := JobConfig{
		FetchImage: "f:1", RunnerImage: "r:1", ControllerURL: "http://ctl.ns.svc/", BotLogin: bot,
		LLMEnv: map[string]string{"LLM_MODEL": "m", "LLM_BASE_URL": ""}, ActiveDeadlineSeconds: 900,
	}
	r := store.Run{ID: runID, Repo: "o/r", PRNumber: 7, HeadSHA: headSHA, JobName: JobName(runID, 1)}
	job := BuildJob(cfg, r)
	spec := job.Spec.Template.Spec

	if *job.Spec.BackoffLimit != 0 || *job.Spec.ActiveDeadlineSeconds != 900 || *spec.AutomountServiceAccountToken || *spec.EnableServiceLinks {
		t.Errorf("job spec %+v", job.Spec)
	}
	fetchEnv := envMap(spec.InitContainers[0].Env)
	if ref := fetchEnv["GITHUB_TOKEN"].ValueFrom.SecretKeyRef; ref.Name != runSecretName(r.JobName) {
		t.Errorf("fetch token ref %+v", ref)
	}
	if fetchEnv["PR_NUMBER"].Value != "7" || fetchEnv["HEAD_SHA"].Value != headSHA || fetchEnv["BOT_LOGIN"].Value != bot {
		t.Errorf("fetch env %+v", fetchEnv)
	}
	if got := fetchEnv["HTTPS_PROXY"].Value; got != "http://"+runID+":$(PROXY_TOKEN)@ctl.ns.svc:80" {
		t.Errorf("fetch proxy %q", got)
	}
	for in, want := range map[string]string{
		"http://ctl:8080": "http://r:$(PROXY_TOKEN)@ctl:8080",
		"https://ctl/":    "https://r:$(PROXY_TOKEN)@ctl:443",
		"ctl":             "http://r:$(PROXY_TOKEN)@ctl:80",
	} {
		if got := egressProxyURL(in, "r"); got != want {
			t.Errorf("egressProxyURL(%q) = %q, want %q", in, got, want)
		}
	}
	if fetchEnv["PROXY_TOKEN"].ValueFrom.SecretKeyRef.Key != "PROXY_TOKEN" || indexOf(spec.InitContainers[0].Env, "PROXY_TOKEN") > indexOf(spec.InitContainers[0].Env, "HTTPS_PROXY") {
		t.Error("PROXY_TOKEN must be defined before HTTPS_PROXY refers to it")
	}
	runnerEnv := envMap(spec.Containers[0].Env)
	if _, leaked := runnerEnv["GITHUB_TOKEN"]; leaked {
		t.Error("runner container gets a GitHub token")
	}
	for _, name := range []string{"RUN_TOKEN", "LLM_API_KEY"} {
		if _, leaked := fetchEnv[name]; leaked {
			t.Errorf("fetch container gets %s", name)
		}
	}
	for _, name := range []string{"PROXY_TOKEN", "HTTPS_PROXY"} {
		if _, leaked := runnerEnv[name]; leaked {
			t.Errorf("runner container gets %s", name)
		}
	}
	if ref := runnerEnv["RUN_TOKEN"].ValueFrom.SecretKeyRef; ref.Key != "RUN_TOKEN" {
		t.Errorf("runner token ref %+v", ref)
	}
	if ref := runnerEnv["LLM_API_KEY"].ValueFrom.SecretKeyRef; ref.Name != runSecretName(r.JobName) || ref.Key != "LLM_API_KEY" {
		t.Errorf("runner model key ref %+v", ref)
	}
	if runnerEnv["RUN_ID"].Value != runID || runnerEnv["LLM_MODEL"].Value != "m" {
		t.Errorf("runner env %+v", runnerEnv)
	}
	if _, ok := runnerEnv["LLM_BASE_URL"]; ok {
		t.Error("empty LLM_BASE_URL passed through")
	}
	if _, ok := runnerEnv["REVIEW_BUDGET_USD"]; ok {
		t.Error("a run without a budget passed REVIEW_BUDGET_USD")
	}
	r.BudgetUSD = 0.5
	if got := envMap(BuildJob(cfg, r).Spec.Template.Spec.Containers[0].Env)["REVIEW_BUDGET_USD"].Value; got != "0.5" {
		t.Errorf("REVIEW_BUDGET_USD = %q, want 0.5", got)
	}
	if len(JobName(strings.Repeat("f", 36), 99)) > 52 {
		t.Error("job name leaves no room for the pod suffix within 63 characters")
	}

	job.Metadata.UID = "u"
	secret := BuildRunSecret(job, RunCredentials{GitHubToken: "tok", ProxyToken: "p", RunToken: "run-tok", LLMKey: "sk-v"})
	data, _ := json.Marshal(secret)
	if !strings.Contains(string(data), `"ownerReferences":[{"apiVersion":"batch/v1","kind":"Job","name":"`+r.JobName+`","uid":"u"}]`) {
		t.Errorf("secret %s", data)
	}
}

func indexOf(env []kube.EnvVar, name string) int {
	for i, e := range env {
		if e.Name == name {
			return i
		}
	}
	return -1
}

func envMap(env []kube.EnvVar) map[string]kube.EnvVar {
	m := map[string]kube.EnvVar{}
	for _, e := range env {
		m[e.Name] = e
	}
	return m
}

func TestPodFailure(t *testing.T) {
	terminated := func(code int32, reason, msg string) kube.ContainerState {
		return kube.ContainerState{Terminated: &kube.ContainerStateTerminated{ExitCode: code, Reason: reason, Message: msg}}
	}
	tests := []struct {
		name  string
		pod   kube.Pod
		want  string
		final bool
	}{
		{"fetch failed", kube.Pod{Status: kube.PodStatus{InitContainerStatuses: []kube.ContainerStatus{{Name: "fetch", State: terminated(1, "Error", "boom")}}}},
			"fetch exited with code 1: boom", false},
		{"head moved", kube.Pod{Status: kube.PodStatus{InitContainerStatuses: []kube.ContainerStatus{{Name: "fetch", State: terminated(run.ExitFinal, "Error", "PR head moved")}}}},
			"fetch exited with code 3: PR head moved", true},
		{"runner OOM", kube.Pod{Status: kube.PodStatus{
			InitContainerStatuses: []kube.ContainerStatus{{Name: "fetch", State: terminated(0, "Completed", "")}},
			ContainerStatuses:     []kube.ContainerStatus{{Name: "runner", State: terminated(137, "OOMKilled", "")}},
		}}, "runner exited with code 137: OOMKilled", false},
		{"image missing", kube.Pod{Status: kube.PodStatus{InitContainerStatuses: []kube.ContainerStatus{
			{Name: "fetch", State: kube.ContainerState{Waiting: &kube.ContainerStateWaiting{Reason: "ImagePullBackOff", Message: "not found"}}},
		}}}, "fetch is stuck: ImagePullBackOff not found", false},
		{"budget spent", kube.Pod{Status: kube.PodStatus{ContainerStatuses: []kube.ContainerStatus{
			{Name: "runner", State: terminated(1, "Error", "budget_exceeded: spent $0.51 of $0.50")},
		}}}, "budget_exceeded: spent $0.51 of $0.50", true},
		{"healthy", kube.Pod{Status: kube.PodStatus{InitContainerStatuses: []kube.ContainerStatus{
			{Name: "fetch", State: kube.ContainerState{Waiting: &kube.ContainerStateWaiting{Reason: "PodInitializing"}}},
		}}}, "", false},
	}
	for _, tt := range tests {
		if got, final := PodFailure(tt.pod); got != tt.want || final != tt.final {
			t.Errorf("%s: PodFailure = %q, %v; want %q, %v", tt.name, got, final, tt.want, tt.final)
		}
	}
}

// A runner's message is the tail of its log; the fatal error is the last line
// and must survive truncation.
func TestPodFailureKeepsLastLine(t *testing.T) {
	log := strings.Repeat("runner:   read a.go\n", 200) + "runner: model API error: 402 Insufficient Balance\n"
	pod := kube.Pod{Status: kube.PodStatus{ContainerStatuses: []kube.ContainerStatus{
		{Name: "runner", State: kube.ContainerState{Terminated: &kube.ContainerStateTerminated{ExitCode: 1, Message: log}}},
	}}}
	got, _ := PodFailure(pod)
	if !strings.HasPrefix(got, "runner exited with code 1: …") || !strings.HasSuffix(got, "runner: model API error: 402 Insufficient Balance") || len(got) > 1600 {
		t.Errorf("PodFailure = %q", got)
	}
}

func TestParseResources(t *testing.T) {
	res, err := ParseResources(`{"requests":{"cpu":"250m","memory":1073741824},"limits":{"cpu":1}}`)
	if err != nil {
		t.Fatal(err)
	}
	if res.Requests["cpu"] != "250m" || res.Requests["memory"] != "1073741824" || res.Limits["cpu"] != "1" {
		t.Errorf("ParseResources = %+v", res)
	}
	if res, err := ParseResources(""); err != nil || res.Requests != nil {
		t.Errorf("empty: %+v, %v", res, err)
	}
	if _, err := ParseResources(`{"requests":"x"}`); err == nil {
		t.Error("malformed resources accepted")
	}
}
