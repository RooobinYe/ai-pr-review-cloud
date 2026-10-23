package controller

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"sort"
	"sync"

	"github.com/RooobinYe/ai-pr-review-cloud/go/internal/githubapp"
	"github.com/RooobinYe/ai-pr-review-cloud/go/internal/kube"
	"github.com/RooobinYe/ai-pr-review-cloud/go/internal/review"
	"github.com/RooobinYe/ai-pr-review-cloud/go/internal/run"
	"github.com/RooobinYe/ai-pr-review-cloud/go/internal/store"
)

// fakeStore mirrors the semantics of *store.Store in memory.
type fakeStore struct {
	mu   sync.Mutex
	runs map[string]*store.Run
	seq  int
}

func newFakeStore(runs ...store.Run) *fakeStore {
	s := &fakeStore{runs: map[string]*store.Run{}}
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
		unreported := r.State == run.StateFailed && !r.Done(store.ActionCheckRunComplete)
		if r.State == run.StateRunning || r.State == run.StateReviewed || unreported {
			out = append(out, s.copyOf(r))
		}
	}
	return out, nil
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
	if reason != "" {
		r.Error = reason
	}
	return nil
}

// fakeGitHub records every write.
type fakeGitHub struct {
	mu          sync.Mutex
	head        string
	reviews     []githubapp.Review
	dismissed   []int64
	posted      []githubapp.NewReview
	rejectLines bool
	checkRuns   map[int64][]githubapp.CheckRun
	tokens      []string
	writeErr    error
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
	return int64(900 + len(g.posted)), nil
}

func (g *fakeGitHub) CreateCheckRun(_ context.Context, _ string, cr githubapp.CheckRun) (int64, error) {
	if g.checkRuns == nil {
		g.checkRuns = map[int64][]githubapp.CheckRun{}
	}
	id := int64(100 + len(g.checkRuns))
	g.checkRuns[id] = []githubapp.CheckRun{cr}
	return id, nil
}

func (g *fakeGitHub) UpdateCheckRun(_ context.Context, _ string, id int64, cr githubapp.CheckRun) error {
	g.checkRuns[id] = append(g.checkRuns[id], cr)
	return nil
}

// fakeKube holds Jobs and Secrets by name.
type fakeKube struct {
	jobs    map[string]kube.Job
	secrets map[string]kube.Secret
	pods    []kube.Pod
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
	return nil
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
	job := k.jobs[name]
	job.Status.Conditions = append(job.Status.Conditions, kube.JobCondition{Type: condition, Status: "True", Reason: "BackoffLimitExceeded"})
	k.jobs[name] = job
}
