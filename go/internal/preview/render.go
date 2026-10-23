package preview

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

type Bundle struct {
	Files map[string]string `json:"files"`
}

type Object = map[string]any

// Render never executes PR programs or Helm plugins. Dependencies must be vendored.
var ErrInput = errors.New("invalid preview input")
var errInfrastructure = errors.New("preview renderer infrastructure")

func Render(ctx context.Context, bundle Bundle, sha, namespace, repository string) ([]Object, Config, error) {
	objects, cfg, err := render(ctx, bundle, sha, namespace, repository)
	if err != nil && !errors.Is(err, errInfrastructure) {
		err = fmt.Errorf("%w: %w", ErrInput, err)
	}
	return objects, cfg, err
}

func render(ctx context.Context, bundle Bundle, sha, namespace, repository string) ([]Object, Config, error) {
	if repository == "" {
		return nil, Config{}, fmt.Errorf("expected image repository is required")
	}

	cfg, err := Parse([]byte(bundle.Files[".ai-pr-review.yaml"]))
	if err != nil || !cfg.Enabled {
		return nil, cfg, err
	}
	if len(bundle.Files) > 100 {
		return nil, cfg, fmt.Errorf("preview bundle exceeds 100 files")
	}
	dir, err := os.MkdirTemp("", "preview-")
	if err != nil {
		return nil, cfg, fmt.Errorf("%w: %w", errInfrastructure, err)
	}
	defer os.RemoveAll(dir)
	total := 0
	for name, content := range bundle.Files {
		total += len(content)
		if total > 1<<20 || !SafePath(name) {
			return nil, cfg, fmt.Errorf("invalid preview bundle path or size")
		}
		if name != ".ai-pr-review.yaml" && name != cfg.Helm.ValuesFile && !strings.HasPrefix(name, cfg.Helm.Chart+"/") {
			return nil, cfg, fmt.Errorf("file outside preview chart: %s", name)
		}
		target := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
			return nil, cfg, fmt.Errorf("%w: %w", errInfrastructure, err)
		}
		if err := os.WriteFile(target, []byte(content), 0600); err != nil {
			return nil, cfg, fmt.Errorf("%w: %w", errInfrastructure, err)
		}
	}
	args := []string{"template", "preview", filepath.Join(dir, cfg.Helm.Chart), "--namespace", namespace, "--skip-tests", "--skip-schema-validation"}
	if cfg.Helm.ValuesFile != "" {
		args = append(args, "--values", filepath.Join(dir, cfg.Helm.ValuesFile))
	}
	args = append(args, "--set-string", "image.tag="+sha, "--set-string", "image.repository="+repository)
	cmd := exec.CommandContext(ctx, "helm", args...)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + dir, "HELM_PLUGINS=" + filepath.Join(dir, "no-plugins")}
	var stdout, stderr cappedBuffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if ctx.Err() != nil || !errors.As(err, &exitErr) {
			return nil, cfg, fmt.Errorf("%w: %v", errInfrastructure, err)
		}
		return nil, cfg, fmt.Errorf("helm template: %w: %s", err, stderr.String())
	}
	dec := yaml.NewDecoder(bytes.NewReader(stdout.Bytes()))
	var objects []Object
	for {
		var obj Object
		if err := dec.Decode(&obj); err == io.EOF {
			break
		} else if err != nil {
			return nil, cfg, err
		}
		if len(obj) == 0 {
			continue
		}
		if len(objects) >= 30 {
			return nil, cfg, fmt.Errorf("preview exceeds 30 resources")
		}
		if obj["kind"] == "Deployment" {
			pod := mapping(mapping(mapping(obj, "spec"), "template"), "spec")
			containers, _ := pod["containers"].([]any)
			for _, v := range containers {
				c, _ := v.(map[string]any)
				image, _ := c["image"].(string)
				if image != repository+":"+sha {
					return nil, cfg, fmt.Errorf("preview image must use the operator-approved repository and PR head SHA tag")
				}
			}
		}
		if err := Validate(obj, namespace); err != nil {
			return nil, cfg, err
		}
		objects = append(objects, obj)
	}
	deployments := 0
	for _, o := range objects {
		if o["kind"] == "Deployment" {
			deployments++
		}
	}
	if deployments == 0 {
		return nil, cfg, fmt.Errorf("preview chart must include a Deployment")
	}
	if len(objects) == 0 {
		return nil, cfg, fmt.Errorf("preview chart contains no resources")
	}
	return objects, cfg, nil
}

type cappedBuffer struct{ bytes.Buffer }

func (b *cappedBuffer) Write(p []byte) (int, error) {
	if b.Len()+len(p) > 2<<20 {
		return 0, fmt.Errorf("helm output exceeds 2 MiB")
	}
	return b.Buffer.Write(p)
}

func mapping(m Object, key string) Object {
	v, _ := m[key].(map[string]any)
	return v
}

// Validate rejects cluster resources and credential-bearing workloads before applying anything.
// Restricted Pod Security admission and namespace-wide default-deny traffic provide independent enforcement.
func Validate(obj Object, namespace string) error {
	api, _ := obj["apiVersion"].(string)
	kind, _ := obj["kind"].(string)
	if !((api == "apps/v1" && kind == "Deployment") || (api == "v1" && (kind == "Service" || kind == "ConfigMap"))) {
		return fmt.Errorf("unsupported preview resource %s/%s", api, kind)
	}
	meta := mapping(obj, "metadata")
	name, _ := meta["name"].(string)
	if name == "" || len(name) > 63 || invalidName.MatchString(name) || strings.HasPrefix(name, "-") || strings.HasSuffix(name, "-") {
		return fmt.Errorf("invalid preview resource name")
	}
	if ns, _ := meta["namespace"].(string); ns != "" && ns != namespace {
		return fmt.Errorf("resource targets another namespace")
	}
	if len(mapping(meta, "annotations")) != 0 {
		return fmt.Errorf("preview resource annotations are not supported")
	}
	if meta["finalizers"] != nil {
		return fmt.Errorf("preview finalizers are not supported")
	}
	delete(meta, "ownerReferences")
	meta["namespace"] = namespace
	if kind == "Service" {
		spec := mapping(obj, "spec")
		if t, _ := spec["type"].(string); t != "" && t != "ClusterIP" {
			return fmt.Errorf("preview Services must be ClusterIP")
		}
		for _, key := range []string{"externalIPs", "externalName", "loadBalancerIP", "loadBalancerClass"} {
			if spec[key] != nil {
				return fmt.Errorf("unsupported Service field %s", key)
			}
		}
	}
	if kind == "Deployment" {
		template := mapping(mapping(obj, "spec"), "template")
		if len(mapping(mapping(template, "metadata"), "annotations")) > 0 {
			return fmt.Errorf("preview pod annotations are not supported")
		}
		pod := mapping(template, "spec")
		if pod == nil {
			return fmt.Errorf("Deployment has no pod spec")
		}
		for _, key := range []string{"serviceAccount", "serviceAccountName", "imagePullSecrets", "volumes", "initContainers", "hostAliases", "nodeName", "nodeSelector", "tolerations", "priorityClassName"} {
			if pod[key] != nil {
				return fmt.Errorf("unsupported preview pod field %s", key)
			}
		}
		pod["automountServiceAccountToken"] = false
		raw, _ := json.Marshal(pod)
		var p struct {
			Containers []struct {
				EnvFrom []any `json:"envFrom"`
				Env     []struct {
					ValueFrom any `json:"valueFrom"`
				} `json:"env"`
			} `json:"containers"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return err
		}
		if len(p.Containers) == 0 {
			return fmt.Errorf("Deployment must contain a container")
		}
		containers, _ := pod["containers"].([]any)
		for _, value := range containers {
			container, _ := value.(map[string]any)
			for _, key := range []string{"readinessProbe", "livenessProbe", "startupProbe"} {
				probe := mapping(container, key)
				for _, kind := range []string{"httpGet", "tcpSocket"} {
					if host, _ := mapping(probe, kind)["host"].(string); host != "" {
						return fmt.Errorf("preview probe host overrides are not supported")
					}
				}
			}
		}
		for _, c := range p.Containers {
			if len(c.EnvFrom) > 0 {
				return fmt.Errorf("preview envFrom is not supported")
			}
			for _, env := range c.Env {
				if env.ValueFrom != nil {
					return fmt.Errorf("preview valueFrom is not supported")
				}
			}
		}
	}
	return nil
}
