package controller

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/RooobinYe/ai-pr-review-cloud/go/internal/kube"
	"github.com/RooobinYe/ai-pr-review-cloud/go/internal/store"
)

// Labels on every runner Job and Pod.
const (
	labelName  = "app.kubernetes.io/name"
	labelPart  = "app.kubernetes.io/component"
	labelRunID = "ai-pr-review.dev/run-id"
)

// runnerUID is the node user of the runner image. fetch runs as the same user
// so git accepts the checkout as safe in the runner container.
const runnerUID = 1000

// JobConfig holds everything about runner Jobs that does not vary per run.
type JobConfig struct {
	FetchImage      string
	RunnerImage     string
	ImagePullPolicy string
	// ControllerURL is where the runner posts its result.
	ControllerURL string
	// LLMSecret names the Secret holding the model API key under LLM_API_KEY.
	LLMSecret string
	// LLMEnv is passed to the runner as is (LLM_PROVIDER, LLM_MODEL, ...).
	LLMEnv                map[string]string
	Language              string
	ActiveDeadlineSeconds int64
	Resources             kube.Resources
}

// ParseResources decodes the runner's resources from JSON such as Helm's
// toJson output. Quantities may be strings ("250m") or numbers (1).
func ParseResources(s string) (kube.Resources, error) {
	var res kube.Resources
	if s == "" {
		return res, nil
	}
	var raw map[string]map[string]any
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber() // keeps 1000000000 from turning into 1e+09
	if err := dec.Decode(&raw); err != nil {
		return res, fmt.Errorf("runner resources: %w", err)
	}
	quantities := func(m map[string]any) map[string]string {
		if m == nil {
			return nil
		}
		out := make(map[string]string, len(m))
		for k, v := range m {
			out[k] = fmt.Sprint(v)
		}
		return out
	}
	res.Requests, res.Limits = quantities(raw["requests"]), quantities(raw["limits"])
	return res, nil
}

// JobName names the Job for one attempt of a run.
func JobName(runID string, attempt int) string {
	return fmt.Sprintf("review-%s-%d", runID, attempt)
}

// runSecretName names the per-run Secret holding fetch's GitHub token and the
// runner's callback token.
func runSecretName(jobName string) string { return jobName + "-credentials" }

// BuildJob renders the runner Job for a run (docs/architecture.md §4.1): the
// fetch init container clones the PR with a read-only token, then the runner
// container reviews the workspace without any GitHub credential.
func BuildJob(cfg JobConfig, r store.Run) kube.Job {
	labels := map[string]string{labelName: "ai-pr-review-cloud", labelPart: "runner", labelRunID: r.ID}
	mounts := []kube.VolumeMount{{Name: "workspace", MountPath: "/workspace"}, {Name: "tmp", MountPath: "/tmp"}}
	hardened := &kube.SecurityContext{
		AllowPrivilegeEscalation: kube.Ptr(false),
		ReadOnlyRootFilesystem:   kube.Ptr(true),
		Capabilities:             &kube.Capabilities{Drop: []string{"ALL"}},
	}

	runnerEnv := []kube.EnvVar{
		{Name: "RUN_ID", Value: r.ID},
		{Name: "CONTROLLER_URL", Value: cfg.ControllerURL},
		{Name: "WORKSPACE", Value: "/workspace"},
		{Name: "REVIEW_LANGUAGE", Value: cfg.Language},
		{Name: "HOME", Value: "/tmp"},
		{Name: "RUN_TOKEN", ValueFrom: &kube.EnvVarSource{SecretKeyRef: &kube.SecretKeySelector{Name: runSecretName(r.JobName), Key: "RUN_TOKEN"}}},
		{Name: "LLM_API_KEY", ValueFrom: &kube.EnvVarSource{SecretKeyRef: &kube.SecretKeySelector{Name: cfg.LLMSecret, Key: "LLM_API_KEY"}}},
	}
	for _, name := range []string{"LLM_PROVIDER", "LLM_MODEL", "LLM_BASE_URL", "LLM_THINKING"} {
		if v := cfg.LLMEnv[name]; v != "" {
			runnerEnv = append(runnerEnv, kube.EnvVar{Name: name, Value: v})
		}
	}
	if r.BudgetUSD > 0 {
		// The runner asks the model to wrap up at 80% of it (§7.3).
		runnerEnv = append(runnerEnv, kube.EnvVar{Name: "REVIEW_BUDGET_USD", Value: strconv.FormatFloat(r.BudgetUSD, 'f', -1, 64)})
	}

	return kube.Job{
		Metadata: kube.ObjectMeta{Name: r.JobName, Labels: labels},
		Spec: kube.JobSpec{
			// Retries are the controller's call, so they can resume from a session snapshot (M3).
			BackoffLimit:            kube.Ptr(int32(0)),
			ActiveDeadlineSeconds:   kube.Ptr(cfg.ActiveDeadlineSeconds),
			TTLSecondsAfterFinished: kube.Ptr(int32(3600)),
			Template: kube.PodTemplateSpec{
				Metadata: kube.ObjectMeta{Labels: labels},
				Spec: kube.PodSpec{
					RestartPolicy:                "Never",
					AutomountServiceAccountToken: kube.Ptr(false),
					// The runner only needs CONTROLLER_URL, not every Service in the namespace.
					EnableServiceLinks: kube.Ptr(false),
					SecurityContext: &kube.PodSecurityContext{
						RunAsNonRoot:   kube.Ptr(true),
						RunAsUser:      kube.Ptr(int64(runnerUID)),
						RunAsGroup:     kube.Ptr(int64(runnerUID)),
						FSGroup:        kube.Ptr(int64(runnerUID)),
						SeccompProfile: &kube.SeccompProfile{Type: "RuntimeDefault"},
					},
					InitContainers: []kube.Container{{
						Name:            "fetch",
						Image:           cfg.FetchImage,
						ImagePullPolicy: cfg.ImagePullPolicy,
						Env: []kube.EnvVar{
							{Name: "GITHUB_TOKEN", ValueFrom: &kube.EnvVarSource{SecretKeyRef: &kube.SecretKeySelector{Name: runSecretName(r.JobName), Key: "GITHUB_TOKEN"}}},
							{Name: "REPO", Value: r.Repo},
							{Name: "PR_NUMBER", Value: strconv.Itoa(r.PRNumber)},
							{Name: "HEAD_SHA", Value: r.HeadSHA},
							{Name: "WORKSPACE", Value: "/workspace"},
							{Name: "HOME", Value: "/tmp"},
						},
						VolumeMounts:             mounts,
						SecurityContext:          hardened,
						TerminationMessagePolicy: "FallbackToLogsOnError",
					}},
					Containers: []kube.Container{{
						Name:                     "runner",
						Image:                    cfg.RunnerImage,
						ImagePullPolicy:          cfg.ImagePullPolicy,
						Env:                      runnerEnv,
						VolumeMounts:             mounts,
						Resources:                &cfg.Resources,
						SecurityContext:          hardened,
						TerminationMessagePolicy: "FallbackToLogsOnError",
					}},
					Volumes: []kube.Volume{
						{Name: "workspace", EmptyDir: &kube.EmptyDirVolumeSource{SizeLimit: "2Gi"}},
						{Name: "tmp", EmptyDir: &kube.EmptyDirVolumeSource{SizeLimit: "1Gi"}},
					},
				},
			},
		},
	}
}

// BuildRunSecret holds fetch's GitHub token and the runner's callback token,
// each exposed to one container only. It is owned by the Job, so Kubernetes
// deletes it together with the Job.
func BuildRunSecret(job kube.Job, githubToken, runToken string) kube.Secret {
	return kube.Secret{
		Metadata: kube.ObjectMeta{
			Name:            runSecretName(job.Metadata.Name),
			Labels:          job.Metadata.Labels,
			OwnerReferences: []kube.OwnerReference{{APIVersion: "batch/v1", Kind: "Job", Name: job.Metadata.Name, UID: job.Metadata.UID}},
		},
		Type:       "Opaque",
		StringData: map[string]string{"GITHUB_TOKEN": githubToken, "RUN_TOKEN": runToken},
	}
}

// FetchTiming returns when the fetch init container of a runner Pod ran, and
// false until it has finished successfully.
func FetchTiming(pod kube.Pod) (started, finished time.Time, ok bool) {
	for _, cs := range pod.Status.InitContainerStatuses {
		if t := cs.State.Terminated; cs.Name == "fetch" && t != nil && t.ExitCode == 0 && !t.StartedAt.IsZero() && !t.FinishedAt.IsZero() {
			return t.StartedAt, t.FinishedAt, true
		}
	}
	return time.Time{}, time.Time{}, false
}

// PodFailure explains why a runner Pod failed, from its container states.
// It returns "" when no container has failed.
func PodFailure(pod kube.Pod) string {
	for _, group := range [][]kube.ContainerStatus{pod.Status.InitContainerStatuses, pod.Status.ContainerStatuses} {
		for _, cs := range group {
			if t := cs.State.Terminated; t != nil && t.ExitCode != 0 {
				msg := t.Message
				if msg == "" {
					msg = t.Reason
				}
				// The message is the tail of the log; its last line is the fatal error.
				return fmt.Sprintf("%s exited with code %d: %s", cs.Name, t.ExitCode, truncateFront(strings.TrimSpace(msg), 1500))
			}
			if w := cs.State.Waiting; w != nil && w.Reason != "" && w.Reason != "PodInitializing" && w.Reason != "ContainerCreating" {
				return fmt.Sprintf("%s is stuck: %s %s", cs.Name, w.Reason, truncate(w.Message, 500))
			}
		}
	}
	return ""
}

// truncate cuts s to at most n bytes without splitting a UTF-8 sequence.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "…"
}

// truncateFront keeps the last n bytes of s, without splitting a UTF-8 sequence.
func truncateFront(s string, n int) string {
	if len(s) <= n {
		return s
	}
	i := len(s) - n
	for i < len(s) && !utf8.RuneStart(s[i]) {
		i++
	}
	return "…" + s[i:]
}
