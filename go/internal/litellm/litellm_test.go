package litellm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"
)

// fakeProxy answers the way LiteLLM v1.104 does.
func fakeProxy(t *testing.T, got *map[string]any) *httptest.Server {
	keys := map[string]bool{}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer sk-master" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"message":"Authentication Error","code":"401"}}`))
			return
		}
		var in map[string]any
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			t.Errorf("decode request: %v", err)
		}
		switch r.URL.Path {
		case "/key/generate":
			*got = in
			keys["sk-virtual"] = true
			_, _ = w.Write([]byte(`{"key":"sk-virtual","token":"hash","max_budget":0.5}`))
		case "/key/delete":
			key := in["keys"].([]any)[0].(string)
			if !keys[key] {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"error":{"message":"{'error': 'No keys found'}","type":"internal_server_error","code":"404"}}`))
				return
			}
			delete(keys, key)
			_, _ = w.Write([]byte(`{"deleted_keys":["` + key + `"]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

func TestGenerateAndDelete(t *testing.T) {
	var got map[string]any
	srv := fakeProxy(t, &got)
	defer srv.Close()
	c := &Client{BaseURL: srv.URL + "/", MasterKey: "sk-master"}
	ctx := context.Background()

	key, err := c.Generate(ctx, Key{
		Alias: "review-r-1", Models: []string{"deepseek-flash"}, MaxBudgetUSD: 0.5,
		Duration: 35 * time.Minute, Metadata: map[string]string{"run_id": "r"},
	})
	if err != nil || key != "sk-virtual" {
		t.Fatalf("Generate = %q, %v", key, err)
	}
	want := map[string]any{
		"key_alias": "review-r-1", "models": []any{"deepseek-flash"}, "max_budget": 0.5,
		"duration": "2100s", "metadata": map[string]any{"run_id": "r"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("generate request = %v, want %v", got, want)
	}

	if err := c.Delete(ctx, key); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := c.Delete(ctx, key); err != nil {
		t.Errorf("deleting a deleted key: %v", err)
	}
}

func TestErrors(t *testing.T) {
	var got map[string]any
	srv := fakeProxy(t, &got)
	defer srv.Close()
	c := &Client{BaseURL: srv.URL, MasterKey: "wrong"}
	_, err := c.Generate(context.Background(), Key{Alias: "a", MaxBudgetUSD: 1})
	e, ok := err.(*APIError)
	if !ok || e.Status != http.StatusUnauthorized || e.Message != "Authentication Error" {
		t.Errorf("Generate with a wrong master key: %v", err)
	}
	if err := c.Delete(context.Background(), "k"); err == nil {
		t.Error("Delete with a wrong master key succeeded")
	}
}
