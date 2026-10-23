package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/RooobinYe/ai-pr-review-cloud/go/internal/review"
	"github.com/RooobinYe/ai-pr-review-cloud/go/internal/run"
)

func TestActionKindsMatchMigration(t *testing.T) {
	sql, err := os.ReadFile("migrations/0001_init.sql")
	if err != nil {
		t.Fatal(err)
	}
	quoted := make([]string, len(ActionKinds))
	for i, k := range ActionKinds {
		quoted[i] = "'" + string(k) + "'"
	}
	want := "kind IN (\n                  " + strings.Join(quoted, ", ") + ")"
	if !strings.Contains(string(sql), want) {
		t.Errorf("migration does not contain %q", want)
	}
}

func TestStatesMatchMigration(t *testing.T) {
	sql, err := os.ReadFile("migrations/0001_init.sql")
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range run.States {
		if !strings.Contains(string(sql), "'"+string(s)+"'") {
			t.Errorf("state CHECK constraint is missing %q", s)
		}
	}
}

// testStore migrates a fresh schema in the database named by TEST_DATABASE_URL.
func testStore(t *testing.T) *Store {
	t.Helper()
	base := os.Getenv("TEST_DATABASE_URL")
	if base == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	buf := make([]byte, 6)
	_, _ = rand.Read(buf)
	schema := "test_" + hex.EncodeToString(buf)

	admin, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
		_ = admin.Close(context.Background())
	})

	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	s, err := Open(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
	return s
}

func newRun(sha string) NewRun {
	return NewRun{Repo: "o/r", PRNumber: 7, HeadSHA: sha, InstallationID: 42, BudgetUSD: 0.5}
}

func jobName(id string, attempt int) string { return id + "-job" }

func TestRecordTriggerDeduplicates(t *testing.T) {
	s, ctx := testStore(t), context.Background()
	id, created, err := s.RecordTrigger(ctx, "d1", newRun("a"))
	if err != nil || !created || id == "" {
		t.Fatalf("first trigger: id=%q created=%v err=%v", id, created, err)
	}
	if _, created, err := s.RecordTrigger(ctx, "d1", newRun("b")); err != nil || created {
		t.Errorf("redelivery: created=%v err=%v, want ignored", created, err)
	}
	if _, created, err := s.RecordTrigger(ctx, "d2", newRun("a")); err != nil || created {
		t.Errorf("same revision: created=%v err=%v, want ignored", created, err)
	}
	r, err := s.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if r.State != run.StateQueued || r.InstallationID != 42 || r.Attempt != 0 || len(r.Actions) != 0 {
		t.Errorf("unexpected run %+v", r)
	}
}

func TestClaimRespectsMaxRunning(t *testing.T) {
	s, ctx := testStore(t), context.Background()
	for i, sha := range []string{"a", "b", "c"} {
		if _, _, err := s.RecordTrigger(ctx, "d"+sha, newRun(sha)); err != nil {
			t.Fatal(i, err)
		}
	}
	claimed, err := s.Claim(ctx, 2, jobName)
	if err != nil {
		t.Fatal(err)
	}
	if len(claimed) != 2 || claimed[0].HeadSHA != "a" || claimed[1].HeadSHA != "b" {
		t.Fatalf("claimed %+v, want the two oldest", claimed)
	}
	if c := claimed[0]; c.State != run.StateRunning || c.Attempt != 1 || c.JobName != c.ID+"-job" {
		t.Errorf("claimed run %+v", c)
	}
	if more, err := s.Claim(ctx, 2, jobName); err != nil || len(more) != 0 {
		t.Errorf("claim at capacity: %d runs, err %v", len(more), err)
	}
	stored, err := s.Get(ctx, claimed[0].ID)
	if err != nil || stored.State != run.StateRunning || stored.JobName != claimed[0].JobName {
		t.Errorf("stored run %+v, err %v", stored, err)
	}
}

func TestResultAndActions(t *testing.T) {
	s, ctx := testStore(t), context.Background()
	id, _, _ := s.RecordTrigger(ctx, "d1", newRun("a"))
	if err := s.SubmitResult(ctx, id, review.Result{Verdict: review.VerdictApprove}); !errors.Is(err, ErrConflict) {
		t.Errorf("result for a queued run: err %v, want ErrConflict", err)
	}
	if _, err := s.Claim(ctx, 1, jobName); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordAction(ctx, id, ActionCheckRunCreate, "123"); err != nil {
		t.Fatal(err)
	}
	res := review.Result{SchemaVersion: 3, Verdict: review.VerdictApprove, Body: "ok", Comments: []review.Comment{}}
	if err := s.SubmitResult(ctx, id, res); err != nil {
		t.Fatal(err)
	}
	r, err := s.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if r.State != run.StateReviewed || r.Verdict != review.VerdictApprove || r.Result == nil || r.Result.Body != "ok" {
		t.Errorf("reviewed run %+v", r)
	}
	if !r.Done(ActionCheckRunCreate) || r.Actions[ActionCheckRunCreate] != "123" || r.Done(ActionReview) {
		t.Errorf("actions %v", r.Actions)
	}
	if err := s.Transition(ctx, id, run.StateRunning, run.StateFailed, "late"); !errors.Is(err, ErrConflict) {
		t.Errorf("stale transition: err %v, want ErrConflict", err)
	}
	if err := s.Transition(ctx, id, run.StateReviewed, run.StateQueued, ""); err == nil {
		t.Error("illegal transition accepted")
	}
	if err := s.Transition(ctx, id, run.StateReviewed, run.StateDone, ""); err != nil {
		t.Fatal(err)
	}
}

func TestActiveIncludesUnreportedFailures(t *testing.T) {
	s, ctx := testStore(t), context.Background()
	reported, _, _ := s.RecordTrigger(ctx, "d1", newRun("a"))
	unreported, _, _ := s.RecordTrigger(ctx, "d2", newRun("b"))
	silent, _, _ := s.RecordTrigger(ctx, "d3", newRun("c"))
	queued, _, _ := s.RecordTrigger(ctx, "d4", newRun("d"))
	if _, err := s.Claim(ctx, 3, jobName); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{reported, unreported, silent} {
		if err := s.Transition(ctx, id, run.StateRunning, run.StateFailed, "boom"); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []string{reported, unreported} {
		if err := s.RecordAction(ctx, id, ActionCheckRunCreate, "1"); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.RecordAction(ctx, reported, ActionCheckRunComplete, "1"); err != nil {
		t.Fatal(err)
	}
	active, err := s.Active(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 1 || active[0].ID != unreported || active[0].Error != "boom" {
		t.Errorf("active = %+v, want only the unreported failure (queued run %s excluded)", active, queued)
	}
}
