package githubapp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRepositoryConfigAPI(t *testing.T) {
	for _, tt := range []struct {
		name   string
		status int
		body   any
		want   string
		bad    bool
	}{
		{"missing", 404, nil, "", false},
		{"file", 200, map[string]any{"type": "file", "size": 18, "encoding": "base64", "content": base64.StdEncoding.EncodeToString([]byte("approvalMode: auto"))}, "approvalMode: auto", false},
		{"directory", 200, []any{}, "", true},
		{"oversize", 200, map[string]any{"type": "file", "size": 65537, "encoding": "base64"}, "", true},
		{"denied", 403, nil, "", true}, {"server error", 503, nil, "", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/repos/o/r/contents/.ai-pr-review.yaml" || r.URL.Query().Get("ref") != "base-sha" {
					t.Errorf("wrong request %s", r.URL)
				}
				w.WriteHeader(tt.status)
				_ = json.NewEncoder(w).Encode(tt.body)
			}))
			defer server.Close()
			c := Client{BaseURL: server.URL, Token: StaticToken("test")}
			data, err := c.RepositoryConfig(context.Background(), "o/r", "base-sha")
			if (err != nil) != tt.bad || string(data) != tt.want {
				t.Fatalf("data %q err %v", data, err)
			}
		})
	}
}
func TestMergeAPIRequiresConfirmedSuccess(t *testing.T) {
	for _, tt := range []struct {
		status int
		merged bool
		bad    bool
	}{{200, true, false}, {200, false, true}, {405, false, true}, {409, false, true}, {503, false, true}} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			if r.Method != "PUT" || r.URL.Path != "/repos/o/r/pulls/7/merge" || body["sha"] != "reviewed-sha" || body["merge_method"] != "squash" {
				t.Errorf("merge request %s %+v", r.URL, body)
			}
			w.WriteHeader(tt.status)
			fmt.Fprintf(w, `{"merged":%v,"sha":"merged-sha","message":"response"}`, tt.merged)
		}))
		c := Client{BaseURL: server.URL, Token: StaticToken("test")}
		sha, err := c.MergePull(context.Background(), "o/r", 7, "reviewed-sha")
		server.Close()
		if (err != nil) != tt.bad || !tt.bad && sha != "merged-sha" {
			t.Fatalf("%+v: sha %q err %v", tt, sha, err)
		}
	}
}
func TestPermissionAPI(t *testing.T) {
	for _, permission := range []string{"read", "triage", "write", "maintain", "admin", "none"} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/repos/o/r/collaborators/human/permission" {
				t.Errorf("wrong path %s", r.URL)
			}
			fmt.Fprintf(w, `{"permission":%q}`, permission)
		}))
		c := Client{BaseURL: server.URL, Token: StaticToken("test")}
		allowed, err := c.CanWrite(context.Background(), "o/r", "human")
		server.Close()
		want := permission == "write" || permission == "maintain" || permission == "admin"
		if err != nil || allowed != want {
			t.Fatalf("%s: allowed %v err %v", permission, allowed, err)
		}
	}
}
