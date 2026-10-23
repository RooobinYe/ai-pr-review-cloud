package controller

import (
	"context"
	"testing"
)

func TestRecreatedSecretGetsNewKey(t *testing.T) {
	h := newHarness()
	h.reconcile()
	name := runSecretName(h.state().JobName)
	first := string(h.kube.secrets[name].Data["LLM_API_KEY"])
	clear(h.kube.secrets)
	h.reconcile()
	second := string(h.kube.secrets[name].Data["LLM_API_KEY"])
	if second == "" || second == first {
		t.Errorf("recreated secret key %q, first %q", second, first)
	}
}

func TestLostSecretRaceDeletesUnusedKey(t *testing.T) {
	h := newHarness()
	h.reconcile()
	r := h.state()
	kept := string(h.kube.secrets[runSecretName(r.JobName)].Data["LLM_API_KEY"])
	// Another pass created the Secret first; this one's key must not linger.
	if err := h.c.createRunSecret(context.Background(), r, h.kube.jobs[r.JobName]); err != nil {
		t.Fatal(err)
	}
	if _, ok := h.keys.live[kept]; !ok || len(h.keys.live) != 1 || len(h.keys.issued) != 2 {
		t.Errorf("live keys %v after %d issued, want only %s", h.keys.live, len(h.keys.issued), kept)
	}
}
