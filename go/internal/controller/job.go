package controller

import (
	"fmt"
	"strconv"
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

// JobName names the Job for one attempt of a run.
func JobName(runID string, attempt int) string {
	return fmt.Sprintf("review-%s-%d", runID, attempt)
}

// tokenSecretName names the per-run Secret holding fetch's GitHub token.
func tokenSecretName(jobName string) string { return jobName + "-github" }

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
		{Name: "LLM_API_KEY", ValueFrom: &kube.EnvVarSource{SecretKeyRef: &kube.SecretKeySelector{Name: cfg.LLMSecret, Key: "LLM_API_KEY"}}},
	}
	for _, name := range []string{"LLM_PROVIDER", "LLM_MODEL", "LLM_BASE_URL", "LLM_THINKING"} {
		if v := cfg.LLMEnv[name]; v != "" {
			runnerEnv = append(runnerEnv, kube.EnvVar{Name: name, Value: v})
		}
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
							{Name: "GITHUB_TOKEN", ValueFrom: &kube.EnvVarSource{SecretKeyRef: &kube.SecretKeySelector{Name: tokenSecretName(r.JobName), Key: "GITHUB_TOKEN"}}},
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

// BuildTokenSecret holds fetch's GitHub token. It is owned by the Job, so
// Kubernetes deletes it together with the Job.
func BuildTokenSecret(job kube.Job, token string) kube.Secret {
	return kube.Secret{
		Metadata: kube.ObjectMeta{
			Name:            tokenSecretName(job.Metadata.Name),
			Labels:          job.Metadata.Labels,
			OwnerReferences: []kube.OwnerReference{{APIVersion: "batch/v1", Kind: "Job", Name: job.Metadata.Name, UID: job.Metadata.UID}},
		},
		Type:       "Opaque",
		StringData: map[string]string{"GITHUB_TOKEN": token},
	}
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
				return fmt.Sprintf("%s exited with code %d: %s", cs.Name, t.ExitCode, truncate(msg, 1500))
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
