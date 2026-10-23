package githubapp

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

func sign(secret, body string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(body))
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func TestVerifySignature(t *testing.T) {
	body := `{"action":"opened"}`
	tests := []struct {
		name   string
		secret string
		sig    string
		want   bool
	}{
		{"valid", "s3cret", sign("s3cret", body), true},
		{"wrong secret", "s3cret", sign("other", body), false},
		{"missing prefix", "s3cret", sign("s3cret", body)[len("sha256="):], false},
		{"not hex", "s3cret", "sha256=zz", false},
		{"empty secret", "", sign("", body), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := VerifySignature([]byte(tt.secret), []byte(body), tt.sig); got != tt.want {
				t.Fatalf("VerifySignature() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestTriggerFromEvent(t *testing.T) {
	event := func(action string) []byte {
		return []byte(`{"action":"` + action + `","pull_request":{"number":7,"head":{"sha":"abc123"}},` +
			`"repository":{"full_name":"octo/demo"},"installation":{"id":42}}`)
	}
	tests := []struct {
		name      string
		eventType string
		body      []byte
		want      Trigger
		wantOK    bool
		wantErr   bool
	}{
		{"opened", "pull_request", event("opened"), Trigger{"octo/demo", 7, "abc123", 42}, true, false},
		{"synchronize", "pull_request", event("synchronize"), Trigger{"octo/demo", 7, "abc123", 42}, true, false},
		{"closed is ignored", "pull_request", event("closed"), Trigger{}, false, false},
		{"other event is ignored", "push", event("opened"), Trigger{}, false, false},
		{"malformed json", "pull_request", []byte(`{`), Trigger{}, false, true},
		{"missing fields", "pull_request", []byte(`{"action":"opened"}`), Trigger{}, false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok, err := TriggerFromEvent(tt.eventType, tt.body)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if ok != tt.wantOK || got != tt.want {
				t.Fatalf("got (%+v, %v), want (%+v, %v)", got, ok, tt.want, tt.wantOK)
			}
		})
	}
}
