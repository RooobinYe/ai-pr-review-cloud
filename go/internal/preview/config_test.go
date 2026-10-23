package preview

import (
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	for _, tt := range []struct {
		name, source   string
		valid, enabled bool
	}{
		{"missing", "", true, false},
		{"valid", "preview:\n  enabled: true\n  helm:\n    chart: demo/chart\n", true, true},
		{"escape", "preview:\n  enabled: true\n  helm:\n    chart: ../chart\n", false, true},
		{"timeout", "preview:\n  enabled: true\n  helm:\n    chart: chart\n  readyTimeout: 1h\n", false, true},
		{"malformed", "preview: [", false, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c, err := Parse([]byte(tt.source))
			if (err == nil) != tt.valid {
				t.Fatalf("Parse: %v", err)
			}
			if err == nil && c.Enabled != tt.enabled {
				t.Fatal(c)
			}
		})
	}
}
func TestNamespace(t *testing.T) {
	a := Namespace(strings.Repeat("a", 100)+"/repo", 1)
	b := Namespace(strings.Repeat("a", 100)+"/repo", 2)
	if len(a) > 63 || a == b || invalidName.MatchString(a) {
		t.Fatalf("bad names %s %s", a, b)
	}
}
func TestValidateIsolation(t *testing.T) {
	for _, tt := range []struct {
		name  string
		obj   Object
		valid bool
	}{
		{"cluster role", Object{"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "ClusterRole"}, false},
		{"secret", Object{"apiVersion": "v1", "kind": "Secret"}, false},
		{"other namespace", Object{"apiVersion": "v1", "kind": "ConfigMap", "metadata": Object{"name": "example", "namespace": "system"}}, false},
		{"external service", Object{"apiVersion": "v1", "kind": "Service", "metadata": Object{"name": "example"}, "spec": Object{"type": "ExternalName"}}, false},
		{"safe config", Object{"apiVersion": "v1", "kind": "ConfigMap", "metadata": Object{"name": "example"}}, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := Validate(tt.obj, "pr-demo-1")
			if (err == nil) != tt.valid {
				t.Fatalf("Validate: %v", err)
			}
		})
	}
}

func TestDeploymentCredentialIsolation(t *testing.T) {
	for _, tt := range []struct {
		name   string
		change func(Object, Object)
		valid  bool
	}{
		{"plain container", func(_, _ Object) {}, true},
		{"token mount requested", func(p, _ Object) { p["automountServiceAccountToken"] = true }, true},
		{"service account", func(p, _ Object) { p["serviceAccountName"] = "controller" }, false},
		{"secret volume", func(p, _ Object) {
			p["volumes"] = []any{Object{"name": "secret", "secret": Object{"secretName": "credentials"}}}
		}, false},
		{"environment source", func(_, c Object) { c["envFrom"] = []any{Object{"secretRef": Object{"name": "credentials"}}} }, false},
		{"environment value", func(_, c Object) {
			c["env"] = []any{Object{"name": "TOKEN", "valueFrom": Object{"secretKeyRef": Object{"name": "credentials", "key": "token"}}}}
		}, false},
		{"probe destination", func(_, c Object) { c["readinessProbe"] = Object{"httpGet": Object{"host": "127.0.0.1", "port": 10250}} }, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			container := Object{"name": "app", "image": "example:head"}
			pod := Object{"containers": []any{container}}
			deployment := Object{"apiVersion": "apps/v1", "kind": "Deployment", "metadata": Object{"name": "example"}, "spec": Object{"template": Object{"spec": pod}}}
			tt.change(pod, container)
			err := Validate(deployment, "pr-demo-1")
			if (err == nil) != tt.valid {
				t.Fatalf("Validate() = %v, valid=%v", err, tt.valid)
			}
			if err == nil && (pod["automountServiceAccountToken"] != false || mapping(deployment, "metadata")["namespace"] != "pr-demo-1") {
				t.Fatal("accepted deployment lacks namespace or token isolation")
			}
		})
	}
}
