package kube

// The subset of the Kubernetes API types the controller reads and writes.
// Field names and JSON tags follow k8s.io/api.

// Ptr returns a pointer to v, for optional API fields.
func Ptr[T any](v T) *T { return &v }

type ObjectMeta struct {
	Name            string            `json:"name,omitempty"`
	UID             string            `json:"uid,omitempty"`
	Labels          map[string]string `json:"labels,omitempty"`
	OwnerReferences []OwnerReference  `json:"ownerReferences,omitempty"`
}

type OwnerReference struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Name       string `json:"name"`
	UID        string `json:"uid"`
}

type Job struct {
	APIVersion string     `json:"apiVersion,omitempty"`
	Kind       string     `json:"kind,omitempty"`
	Metadata   ObjectMeta `json:"metadata"`
	Spec       JobSpec    `json:"spec"`
	Status     JobStatus  `json:"status,omitzero"`
}

type JobSpec struct {
	BackoffLimit            *int32          `json:"backoffLimit,omitempty"`
	ActiveDeadlineSeconds   *int64          `json:"activeDeadlineSeconds,omitempty"`
	TTLSecondsAfterFinished *int32          `json:"ttlSecondsAfterFinished,omitempty"`
	Template                PodTemplateSpec `json:"template"`
}

type JobStatus struct {
	Active     int32          `json:"active,omitempty"`
	Succeeded  int32          `json:"succeeded,omitempty"`
	Failed     int32          `json:"failed,omitempty"`
	Conditions []JobCondition `json:"conditions,omitempty"`
}

// JobCondition types that mark a finished Job.
const (
	JobComplete = "Complete"
	JobFailed   = "Failed"
)

type JobCondition struct {
	Type    string `json:"type"`
	Status  string `json:"status"`
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`
}

// Condition returns the Job's condition of type t if it is true.
func (s JobStatus) Condition(t string) (JobCondition, bool) {
	for _, c := range s.Conditions {
		if c.Type == t && c.Status == "True" {
			return c, true
		}
	}
	return JobCondition{}, false
}

type PodTemplateSpec struct {
	Metadata ObjectMeta `json:"metadata,omitzero"`
	Spec     PodSpec    `json:"spec"`
}

type PodSpec struct {
	RestartPolicy                string              `json:"restartPolicy,omitempty"`
	AutomountServiceAccountToken *bool               `json:"automountServiceAccountToken,omitempty"`
	EnableServiceLinks           *bool               `json:"enableServiceLinks,omitempty"`
	SecurityContext              *PodSecurityContext `json:"securityContext,omitempty"`
	InitContainers               []Container         `json:"initContainers,omitempty"`
	Containers                   []Container         `json:"containers"`
	Volumes                      []Volume            `json:"volumes,omitempty"`
}

type PodSecurityContext struct {
	RunAsNonRoot   *bool           `json:"runAsNonRoot,omitempty"`
	RunAsUser      *int64          `json:"runAsUser,omitempty"`
	RunAsGroup     *int64          `json:"runAsGroup,omitempty"`
	FSGroup        *int64          `json:"fsGroup,omitempty"`
	SeccompProfile *SeccompProfile `json:"seccompProfile,omitempty"`
}

type SeccompProfile struct {
	Type string `json:"type"`
}

type Container struct {
	Name                     string           `json:"name"`
	Image                    string           `json:"image"`
	ImagePullPolicy          string           `json:"imagePullPolicy,omitempty"`
	Args                     []string         `json:"args,omitempty"`
	Env                      []EnvVar         `json:"env,omitempty"`
	VolumeMounts             []VolumeMount    `json:"volumeMounts,omitempty"`
	Resources                *Resources       `json:"resources,omitempty"`
	SecurityContext          *SecurityContext `json:"securityContext,omitempty"`
	TerminationMessagePolicy string           `json:"terminationMessagePolicy,omitempty"`
}

type EnvVar struct {
	Name      string        `json:"name"`
	Value     string        `json:"value,omitempty"`
	ValueFrom *EnvVarSource `json:"valueFrom,omitempty"`
}

type EnvVarSource struct {
	SecretKeyRef *SecretKeySelector `json:"secretKeyRef,omitempty"`
}

type SecretKeySelector struct {
	Name string `json:"name"`
	Key  string `json:"key"`
}

type VolumeMount struct {
	Name      string `json:"name"`
	MountPath string `json:"mountPath"`
}

type Volume struct {
	Name     string                `json:"name"`
	EmptyDir *EmptyDirVolumeSource `json:"emptyDir,omitempty"`
}

type EmptyDirVolumeSource struct {
	SizeLimit string `json:"sizeLimit,omitempty"`
}

type Resources struct {
	Requests map[string]string `json:"requests,omitempty"`
	Limits   map[string]string `json:"limits,omitempty"`
}

type SecurityContext struct {
	AllowPrivilegeEscalation *bool         `json:"allowPrivilegeEscalation,omitempty"`
	ReadOnlyRootFilesystem   *bool         `json:"readOnlyRootFilesystem,omitempty"`
	Capabilities             *Capabilities `json:"capabilities,omitempty"`
}

type Capabilities struct {
	Drop []string `json:"drop,omitempty"`
}

type Secret struct {
	APIVersion string            `json:"apiVersion,omitempty"`
	Kind       string            `json:"kind,omitempty"`
	Metadata   ObjectMeta        `json:"metadata"`
	Type       string            `json:"type,omitempty"`
	StringData map[string]string `json:"stringData,omitempty"`
	// Data is what the API server returns; encoding/json decodes its base64.
	Data map[string][]byte `json:"data,omitempty"`
}

type Pod struct {
	Metadata ObjectMeta `json:"metadata"`
	Status   PodStatus  `json:"status"`
}

type PodStatus struct {
	Phase                 string            `json:"phase,omitempty"`
	InitContainerStatuses []ContainerStatus `json:"initContainerStatuses,omitempty"`
	ContainerStatuses     []ContainerStatus `json:"containerStatuses,omitempty"`
}

type ContainerStatus struct {
	Name  string         `json:"name"`
	State ContainerState `json:"state"`
}

type ContainerState struct {
	Waiting    *ContainerStateWaiting    `json:"waiting,omitempty"`
	Terminated *ContainerStateTerminated `json:"terminated,omitempty"`
}

type ContainerStateWaiting struct {
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`
}

type ContainerStateTerminated struct {
	ExitCode int32  `json:"exitCode"`
	Reason   string `json:"reason,omitempty"`
	Message  string `json:"message,omitempty"`
}
