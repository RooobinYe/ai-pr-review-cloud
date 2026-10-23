package preview

import (
	"context"
	"errors"
	"os/exec"
	"testing"
)

func renderBundle(image string) Bundle {
	return Bundle{Files: map[string]string{
		".ai-pr-review.yaml":          "preview:\n  enabled: true\n  helm:\n    chart: chart\n",
		"chart/Chart.yaml":            "apiVersion: v2\nname: test\nversion: 0.1.0\n",
		"chart/templates/deploy.yaml": "apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: test\nspec:\n  template:\n    spec:\n      containers:\n        - name: app\n          image: \"" + image + "\"\n",
	}}
}

func TestRenderPinsImageRepositoryAndTag(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm required")
	}
	for _, tt := range []struct {
		name, image string
		valid       bool
	}{
		{"template", "{{ .Values.image.repository }}:{{ .Values.image.tag }}", true},
		{"wrong repository", "registry.example/other:abc", false},
		{"wrong tag", "registry.example/approved:old", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			objects, _, err := Render(context.Background(), renderBundle(tt.image), "abc", "pr-test", "registry.example/approved")
			if tt.valid {
				if err != nil || len(objects) != 1 {
					t.Fatalf("render: %v", err)
				}
			} else if !errors.Is(err, ErrInput) {
				t.Fatalf("wrong error category: %v", err)
			}
		})
	}
}

func TestRenderInfrastructureErrorsRemainRetryable(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	_, _, err := Render(context.Background(), renderBundle("{{ .Values.image.repository }}:{{ .Values.image.tag }}"), "abc", "pr-test", "registry.example/approved")
	if err == nil || errors.Is(err, ErrInput) {
		t.Fatalf("missing helm must be infrastructure error: %v", err)
	}
}
