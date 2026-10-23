package githubapp

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
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
		return []byte(`{"action":"` + action + `","pull_request":{"number":7,"head":{"sha":"abababababababababababababababababababab"}},` +
			`"repository":{"full_name":"octo/demo"},"installation":{"id":42}}`)
	}
	const sha = "abababababababababababababababababababab"
	tests := []struct {
		name      string
		eventType string
		body      []byte
		want      Trigger
		wantOK    bool
		wantErr   bool
	}{
		{"opened", "pull_request", event("opened"), Trigger{"octo/demo", 7, sha, 42}, true, false},
		{"synchronize", "pull_request", event("synchronize"), Trigger{"octo/demo", 7, sha, 42}, true, false},
		{"reopened", "pull_request", event("reopened"), Trigger{"octo/demo", 7, sha, 42}, true, false},
		{"ready for review", "pull_request", event("ready_for_review"), Trigger{"octo/demo", 7, sha, 42}, true, false},
		{"converted to draft is ignored", "pull_request", event("converted_to_draft"), Trigger{}, false, false},
		{"closed is ignored", "pull_request", event("closed"), Trigger{}, false, false},
		{"other event is ignored", "push", event("opened"), Trigger{}, false, false},
		{"malformed json", "pull_request", []byte(`{`), Trigger{}, false, true},
		{"missing fields", "pull_request", []byte(`{"action":"opened"}`), Trigger{}, false, true},
		{"short sha", "pull_request", []byte(`{"action":"opened","pull_request":{"number":7,"head":{"sha":"abc"}},` +
			`"repository":{"full_name":"octo/demo"},"installation":{"id":42}}`), Trigger{}, false, true},
	}
	for _, action := range []string{"opened", "synchronize", "reopened", "ready_for_review"} {
		for _, draft := range []bool{false, true} {
			body := strings.Replace(string(event(action)), `"number":7`, fmt.Sprintf(`"number":7,"draft":%t`, draft), 1)
			want := Trigger{"octo/demo", 7, sha, 42}
			if draft {
				want = Trigger{}
			}
			tests = append(tests, struct {
				name      string
				eventType string
				body      []byte
				want      Trigger
				wantOK    bool
				wantErr   bool
			}{fmt.Sprintf("%s draft=%t", action, draft), "pull_request", []byte(body), want, !draft, false})
		}
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

func TestApprovalFromEvent(t *testing.T) {
	const comment = `{"action":"created","repository":{"full_name":"o/r"},"installation":{"id":42},"issue":{"number":7,"pull_request":{}},"comment":{"body":"/merge","user":{"login":"human","type":"User"},"created_at":"2026-10-24T00:00:00Z"}}`
	const approval = `{"action":"submitted","repository":{"full_name":"o/r"},"installation":{"id":42},"pull_request":{"number":7},"review":{"id":123,"state":"approved","commit_id":"2222222222222222222222222222222222222222","user":{"login":"human","type":"User"},"submitted_at":"2026-10-24T00:00:00Z"}}`
	for _, tt := range []struct {
		name, event, body string
		ok, bad           bool
	}{
		{"comment", "issue_comment", comment, true, false}, {"review", "pull_request_review", approval, true, false},
		{"edited", "issue_comment", strings.Replace(comment, "created", "edited", 1), false, false},
		{"bot", "issue_comment", strings.Replace(comment, "User", "Bot", 1), false, false},
		{"ordinary issue", "issue_comment", strings.Replace(comment, `,"pull_request":{}`, "", 1), false, false},
		{"quoted command", "issue_comment", strings.Replace(comment, "/merge", "> /merge", 1), false, false},
		{"arguments", "issue_comment", strings.Replace(comment, "/merge", "/merge now", 1), false, false},
		{"non approval", "pull_request_review", strings.Replace(approval, "approved", "commented", 1), false, false},
		{"missing timestamp", "issue_comment", strings.Replace(comment, "2026-10-24T00:00:00Z", "", 1), false, true},
		{"invalid json", "issue_comment", "{", false, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			a, ok, err := ApprovalFromEvent(tt.event, []byte(tt.body))
			if ok != tt.ok || (err != nil) != tt.bad {
				t.Fatalf("%+v ok %v err %v", a, ok, err)
			}
			if ok && (a.Actor != "human" || a.PRNumber != 7 || a.InstallationID != 42) {
				t.Fatal(a)
			}
		})
	}
}
