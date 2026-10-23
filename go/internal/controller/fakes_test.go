package controller

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/RooobinYe/ai-pr-review-cloud/go/internal/githubapp"
	"github.com/RooobinYe/ai-pr-review-cloud/go/internal/kube"
	"github.com/RooobinYe/ai-pr-review-cloud/go/internal/litellm"
	"github.com/RooobinYe/ai-pr-review-cloud/go/internal/repoconfig"
	"github.com/RooobinYe/ai-pr-review-cloud/go/internal/review"
	"github.com/RooobinYe/ai-pr-review-cloud/go/internal/run"
	"github.com/RooobinYe/ai-pr-review-cloud/go/internal/store"
)

// fakeStore mirrors the semantics of *store.Store in memory.
type fakeStore struct {
	signals      map[string][]store.MergeSignal
	mu           sync.Mutex
	runs         map[string]*store.Run
	seq          int
	sessions     map[string]session
	usage        map[string]map[int]store.Usage
	fetchSeconds map[string]float64
	now          func() time.Time
	// crashOn makes the next RecordAction of that kind fail, as if the
	// controller stopped right after the GitHub write.
	crashOn store.ActionKind
}

type session struct {
	jsonl []byte
	turn  int
}

func newFakeStore(runs ...store.Run) *fakeStore {
	s := &fakeStore{runs: map[string]*store.Run{}, sessions: map[string]session{}, usage: map[string]map[int]store.Usage{}, now: time.Now}
	for _, r := range runs {
		s.seq++
		r.Actions = map[store.ActionKind]string{}
		s.runs[r.ID] = &r
	}
	return s
}

func (s *fakeStore) copyOf(r *store.Run) store.Run {
	c := *r
	c.Actions = maps.Clone(r.Actions)
	return c
}

func (s *fakeStore) sorted() []*store.Run {
	var out []*store.Run
	for _, r := range s.runs {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (s *fakeStore) Claim(_ context.Context, maxRunning int, jobName func(string, int) string) ([]store.Run, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	inFlight := 0
	for _, r := range s.runs {
		if r.State == run.StateRunning {
			inFlight++
		}
	}
	var claimed []store.Run
	for _, r := range s.sorted() {
		if r.State != run.StateQueued || inFlight >= maxRunning {
			continue
		}
		r.State, r.Attempt = run.StateRunning, r.Attempt+1
		r.JobName = jobName(r.ID, r.Attempt)
		r.ClaimedAt = s.now()
		inFlight++
		claimed = append(claimed, s.copyOf(r))
	}
	return claimed, nil
}

func (s *fakeStore) Active(context.Context) ([]store.Run, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []store.Run
	for _, r := range s.sorted() {
		unreported := (r.State == run.StateFailed || r.State == run.StateSuperseded) && !r.Done(store.ActionCheckRunComplete)
		if r.State == run.StateRunning || r.State == run.StateReviewed || r.State == run.StateAwaitingApproval || r.State == run.StateMerging || unreported {
			out = append(out, s.copyOf(r))
		}
	}
	return out, nil
}

func (s *fakeStore) Supersede(context.Context) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var ids []string
	for _, r := range s.sorted() {
		switch r.State {
		case run.StateQueued, run.StateRunning, run.StatePreviewing, run.StateAwaitingApproval:
		default:
			continue
		}
		for _, n := range s.runs {
			if n.Repo == r.Repo && n.PRNumber == r.PRNumber && n.CreatedAt.After(r.CreatedAt) {
				r.State = run.StateSuperseded
				delete(r.Actions, store.ActionCheckRunComplete)
				ids = append(ids, r.ID)
				break
			}
		}
	}
	return ids, nil
}

func (s *fakeStore) PutSession(_ context.Context, id string, turn int, jsonl []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.runs[id].State != run.StateRunning {
		return store.ErrConflict
	}
	if cur, ok := s.sessions[id]; !ok || cur.turn <= turn {
		s.sessions[id] = session{jsonl: jsonl, turn: turn}
	}
	return nil
}

func (s *fakeStore) GetSession(_ context.Context, id string) ([]byte, int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.sessions[id]
	if !ok {
		return nil, 0, store.ErrNotFound
	}
	return cur.jsonl, cur.turn, nil
}

func (s *fakeStore) PutUsage(_ context.Context, id string, u store.Usage) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.usage[id] == nil {
		s.usage[id] = map[int]store.Usage{}
	}
	s.usage[id][u.Turn] = u
	return nil
}

func (s *fakeStore) RunCostUSD(_ context.Context, id string) (float64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var cost float64
	for _, usage := range s.usage[id] {
		cost += usage.CostUSD
	}
	return cost, nil
}

func (s *fakeStore) Get(_ context.Context, id string) (store.Run, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.runs[id]
	if !ok {
		return store.Run{}, store.ErrNotFound
	}
	return s.copyOf(r), nil
}

func (s *fakeStore) RecordAction(_ context.Context, id string, kind store.ActionKind, ext string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if kind == s.crashOn {
		s.crashOn = ""
		return fmt.Errorf("controller stopped before recording %s", kind)
	}
	s.runs[id].Actions[kind] = ext
	return nil
}

func (s *fakeStore) SubmitResult(_ context.Context, id string, res review.Result) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.runs[id]
	if r.State != run.StateRunning {
		return store.ErrConflict
	}
	r.State, r.Verdict, r.Result = run.StateReviewed, res.Verdict, &res
	return nil
}

func (s *fakeStore) Transition(_ context.Context, id string, from, to run.State, reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !run.CanTransition(from, to) {
		return fmt.Errorf("illegal %s -> %s", from, to)
	}
	r := s.runs[id]
	if r.State != from {
		return store.ErrConflict
	}
	r.State = to
	if to == run.StateFailed || to == run.StateSuperseded {
		delete(r.Actions, store.ActionCheckRunComplete)
	}
	if reason != "" {
		r.Error = reason
	}
	return nil
}

func (s *fakeStore) RecordFetchTiming(_ context.Context, id string, started, finished time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.runs[id].FetchTimed = true
	if s.fetchSeconds == nil {
		s.fetchSeconds = map[string]float64{}
	}
	s.fetchSeconds[id] = finished.Sub(started).Seconds()
	return nil
}

// fakeGitHub records every write.
type fakeGitHub struct {
	config      []byte
	configRefs  []string
	configErr   error
	writable    bool
	merged      bool
	closed      bool
	merges      []string
	mergeErr    error
	mu          sync.Mutex
	head        string
	reviews     []githubapp.Review
	dismissed   []int64
	posted      []githubapp.NewReview
	rejectLines bool
	checkRuns   map[int64][]githubapp.CheckRun
	tokens      []string
	writeErr    error
	deliveries  []githubapp.Delivery
	redelivered []int64
}

func (g *fakeGitHub) Client(int64) GitHubClient { return g }

func (g *fakeGitHub) FetchToken(_ context.Context, installationID int64, repo string) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	t := fmt.Sprintf("ro-%d-%s-%d", installationID, repo, len(g.tokens))
	g.tokens = append(g.tokens, t)
	return t, nil
}

func (g *fakeGitHub) GetPull(context.Context, string, int) (githubapp.Pull, error) {
	var p githubapp.Pull
	p.Head.SHA = g.head
	p.Base.SHA = oldSHA
	p.State = "open"
	if g.closed || g.merged {
		p.State = "closed"
	}
	p.Merged = g.merged
	if g.merged {
		p.MergeCommitSHA = "merged-sha"
	}
	return p, nil
}

func (g *fakeGitHub) ListReviews(context.Context, string, int) ([]githubapp.Review, error) {
	return g.reviews, nil
}

// refuse makes every later GitHub write fail with err.
func (g *fakeGitHub) refuse(err error) { g.writeErr = err }

func (g *fakeGitHub) DismissReview(_ context.Context, _ string, _ int, id int64, _ string) error {
	g.dismissed = append(g.dismissed, id)
	return nil
}

func (g *fakeGitHub) CreateReview(_ context.Context, _ string, _ int, r githubapp.NewReview) (int64, error) {
	if g.writeErr != nil {
		return 0, g.writeErr
	}
	if g.rejectLines && len(r.Comments) > 0 {
		return 0, &githubapp.APIError{Status: http.StatusUnprocessableEntity, Message: "Line could not be resolved"}
	}
	g.posted = append(g.posted, r)
	id := int64(900 + len(g.posted))
	state := "COMMENTED"
	if r.Event == "APPROVE" {
		state = "APPROVED"
	}
	rv := githubapp.Review{ID: id, State: state, CommitID: r.CommitID, Body: r.Body}
	rv.User.Login = bot
	g.reviews = append(g.reviews, rv)
	return id, nil
}

func (g *fakeGitHub) CreateCheckRun(_ context.Context, _ string, cr githubapp.CheckRun) (int64, error) {
	if g.checkRuns == nil {
		g.checkRuns = map[int64][]githubapp.CheckRun{}
	}
	id := int64(100 + len(g.checkRuns))
	g.checkRuns[id] = []githubapp.CheckRun{cr}
	return id, nil
}

func (g *fakeGitHub) FindCheckRun(_ context.Context, _ string, sha, name, externalID string) (int64, error) {
	for id, updates := range g.checkRuns {
		if cr := updates[0]; cr.HeadSHA == sha && cr.Name == name && cr.ExternalID == externalID {
			return id, nil
		}
	}
	return 0, nil
}

func (g *fakeGitHub) ListDeliveries(context.Context) ([]githubapp.Delivery, error) {
	return g.deliveries, nil
}

func (g *fakeGitHub) Redeliver(_ context.Context, id int64) error {
	g.redelivered = append(g.redelivered, id)
	return nil
}

func (g *fakeGitHub) UpdateCheckRun(_ context.Context, _ string, id int64, cr githubapp.CheckRun) error {
	g.checkRuns[id] = append(g.checkRuns[id], cr)
	return nil
}

// fakeKube holds Jobs and Secrets by name.
type fakeKube struct {
	jobs                    map[string]kube.Job
	secrets                 map[string]kube.Secret
	pods                    []kube.Pod
	createSecretResponseErr error
	deleted                 []string
}

func newFakeKube() *fakeKube {
	return &fakeKube{jobs: map[string]kube.Job{}, secrets: map[string]kube.Secret{}}
}

func (k *fakeKube) CreateJob(_ context.Context, job kube.Job) (kube.Job, error) {
	if _, ok := k.jobs[job.Metadata.Name]; ok {
		return kube.Job{}, kube.ErrAlreadyExists
	}
	job.Metadata.UID = "uid-" + job.Metadata.Name
	k.jobs[job.Metadata.Name] = job
	return job, nil
}

func (k *fakeKube) GetJob(_ context.Context, name string) (kube.Job, error) {
	job, ok := k.jobs[name]
	if !ok {
		return kube.Job{}, kube.ErrNotFound
	}
	return job, nil
}

// DeleteJob removes the Job and, as the owner reference would, its Secret.
func (k *fakeKube) DeleteJob(_ context.Context, name string) error {
	if _, ok := k.jobs[name]; !ok {
		return kube.ErrNotFound
	}
	delete(k.jobs, name)
	delete(k.secrets, runSecretName(name))
	k.deleted = append(k.deleted, name)
	return nil
}

func (k *fakeKube) CreateSecret(_ context.Context, s kube.Secret) error {
	if _, ok := k.secrets[s.Metadata.Name]; ok {
		return kube.ErrAlreadyExists
	}
	// The API server returns stringData as data.
	s.Data = map[string][]byte{}
	for key, v := range s.StringData {
		s.Data[key] = []byte(v)
	}
	k.secrets[s.Metadata.Name] = s
	return k.createSecretResponseErr
}

func (k *fakeKube) GetSecret(_ context.Context, name string) (kube.Secret, error) {
	s, ok := k.secrets[name]
	if !ok {
		return kube.Secret{}, kube.ErrNotFound
	}
	return s, nil
}

func (k *fakeKube) ListPods(context.Context, string) ([]kube.Pod, error) { return k.pods, nil }

func (k *fakeKube) finish(name, condition string) {
	k.finishWith(name, condition, "BackoffLimitExceeded")
}

func (k *fakeKube) finishWith(name, condition, reason string) {
	job := k.jobs[name]
	job.Status.Conditions = append(job.Status.Conditions, kube.JobCondition{Type: condition, Status: "True", Reason: reason})
	k.jobs[name] = job
}

// fakeKeys stands in for the LiteLLM proxy's key management.
type fakeKeys struct {
	mu     sync.Mutex
	issued []litellm.Key
	live   map[string]litellm.Key
}

func (f *fakeKeys) Generate(_ context.Context, k litellm.Key) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.live == nil {
		f.live = map[string]litellm.Key{}
	}
	key := fmt.Sprintf("sk-virtual-%d", len(f.issued))
	f.issued = append(f.issued, k)
	f.live[key] = k
	return key, nil
}

func (f *fakeKeys) Delete(_ context.Context, key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.live, key)
	return nil
}

func (s *fakeStore) SaveConfig(_ context.Context, id, sha string, cfg repoconfig.Config) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.runs[id]
	if r.Config == nil && r.State == run.StateRunning {
		r.Config = &cfg
		r.ConfigSHA = sha
		r.BudgetUSD = cfg.BudgetUSD
	}
	return nil
}
func (s *fakeStore) MergeSignals(_ context.Context, id string) ([]store.MergeSignal, error) {
	return s.signals[id], nil
}
func (s *fakeStore) IgnoreMergeSignal(_ context.Context, id string) error {
	for runID, signals := range s.signals {
		for i, sig := range signals {
			if sig.DeliveryID == id {
				s.signals[runID] = append(signals[:i], signals[i+1:]...)
				return nil
			}
		}
	}
	return nil
}
func (g *fakeGitHub) RepositoryConfig(_ context.Context, _ string, ref string) ([]byte, error) {
	g.configRefs = append(g.configRefs, ref)
	return g.config, g.configErr
}
func (g *fakeGitHub) CanWrite(context.Context, string, string) (bool, error) { return g.writable, nil }
func (g *fakeGitHub) GetReview(_ context.Context, _ string, _ int, id int64) (githubapp.Review, error) {
	for _, r := range g.reviews {
		if r.ID == id {
			return r, nil
		}
	}
	return githubapp.Review{}, &githubapp.APIError{Status: 404}
}
func (g *fakeGitHub) MergePull(_ context.Context, _ string, _ int, sha string) (string, error) {
	if g.mergeErr != nil {
		return "", g.mergeErr
	}
	g.merges = append(g.merges, sha)
	g.merged = true
	return "merged-sha", nil
}
