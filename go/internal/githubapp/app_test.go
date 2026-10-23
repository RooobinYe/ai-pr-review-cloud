package githubapp

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func testKey(t *testing.T) (*rsa.PrivateKey, []byte) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return key, pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
}

// verifyJWT checks an RS256 JWT against key and returns its claims.
func verifyJWT(t *testing.T, key *rsa.PublicKey, token string) map[string]any {
	t.Helper()
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("JWT has %d parts", len(parts))
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(key, crypto.SHA256, digest[:], sig); err != nil {
		t.Fatalf("JWT signature: %v", err)
	}
	payload, _ := base64.RawURLEncoding.DecodeString(parts[1])
	var claims map[string]any
	if err := json.Unmarshal(payload, &claims); err != nil {
		t.Fatal(err)
	}
	return claims
}

func TestNewAppAcceptsPKCS8(t *testing.T) {
	key, _ := testKey(t)
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewApp("1", pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), ""); err != nil {
		t.Fatal(err)
	}
	if _, err := NewApp("1", []byte("not a key"), ""); err == nil {
		t.Error("garbage key accepted")
	}
}

func TestJWTClaims(t *testing.T) {
	key, keyPEM := testKey(t)
	app, err := NewApp("12345", keyPEM, "")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_700_000_000, 0)
	app.now = func() time.Time { return now }
	token, err := app.JWT()
	if err != nil {
		t.Fatal(err)
	}
	claims := verifyJWT(t, &key.PublicKey, token)
	if claims["iss"] != "12345" || claims["iat"] != float64(now.Unix()-60) || claims["exp"] != float64(now.Unix()+540) {
		t.Errorf("claims %v", claims)
	}
}

func TestInstallationTokenCaching(t *testing.T) {
	key, keyPEM := testKey(t)
	now := time.Unix(1_700_000_000, 0)
	minted := 0
	var lastScope string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/app/installations/9/access_tokens":
			verifyJWT(t, &key.PublicKey, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
			body, _ := io.ReadAll(r.Body)
			lastScope = string(body)
			minted++
			fmt.Fprintf(w, `{"token":"tok-%d","expires_at":%q}`, minted, now.Add(time.Hour).Format(time.RFC3339))
		case r.URL.Path == "/repos/o/r/pulls/1":
			fmt.Fprintf(w, `{"number":1,"head":{"sha":%q}}`, r.Header.Get("Authorization"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	app, err := NewApp("1", keyPEM, srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	app.now = func() time.Time { return now }
	ctx := context.Background()
	client := app.Installation(9)
	for range 2 {
		pull, err := client.GetPull(ctx, "o/r", 1)
		if err != nil {
			t.Fatal(err)
		}
		if pull.Head.SHA != "Bearer tok-1" {
			t.Errorf("request used %q", pull.Head.SHA)
		}
	}
	if minted != 1 || lastScope != "" {
		t.Errorf("minted %d tokens with scope %q, want one unscoped", minted, lastScope)
	}

	now = now.Add(56 * time.Minute) // inside the refresh margin
	if pull, _ := client.GetPull(ctx, "o/r", 1); pull.Head.SHA != "Bearer tok-2" {
		t.Errorf("expiring token not renewed: %q", pull.Head.SHA)
	}

	scoped, err := app.InstallationToken(ctx, 9, &TokenScope{Repositories: []string{"r"}, Permissions: map[string]string{"contents": "read"}})
	if err != nil || scoped.Token != "tok-3" {
		t.Fatalf("scoped token %+v, err %v", scoped, err)
	}
	if lastScope != `{"repositories":["r"],"permissions":{"contents":"read"}}` {
		t.Errorf("scope body %s", lastScope)
	}
}

func TestAPIErrorAndPagination(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/o/r/pulls/1/files":
			page := r.URL.Query().Get("page")
			n := 100
			if page == "2" {
				n = 3
			}
			files := make([]PullFile, n)
			for i := range files {
				files[i].Filename = fmt.Sprintf("p%s-%d", page, i)
			}
			_ = json.NewEncoder(w).Encode(files)
		case "/repos/o/r/pulls/1/reviews":
			w.WriteHeader(http.StatusUnprocessableEntity)
			fmt.Fprint(w, `{"message":"Validation Failed","errors":["Line could not be resolved"]}`)
		}
	}))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL, Token: StaticToken("t")}
	ctx := context.Background()

	files, err := c.ListPullFiles(ctx, "o/r", 1)
	if err != nil || len(files) != 103 || files[102].Filename != "p2-2" {
		t.Errorf("got %d files, err %v", len(files), err)
	}
	_, err = c.CreateReview(ctx, "o/r", 1, NewReview{Event: "COMMENT"})
	if StatusOf(err) != http.StatusUnprocessableEntity || !strings.Contains(err.Error(), "Line could not be resolved") {
		t.Errorf("err %v", err)
	}
	if StatusOf(fmt.Errorf("wrapped: %w", err)) != http.StatusUnprocessableEntity {
		t.Error("StatusOf does not unwrap")
	}
}

func TestAPIErrorMessageStaysValidUTF8(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		fmt.Fprintf(w, `{"message":"%s"}`, strings.Repeat("评", 200)) // 600 bytes; 500 splits a rune
	}))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL, Token: StaticToken("t")}
	err := c.UpdateCheckRun(context.Background(), "o/r", 1, CheckRun{})
	var apiErr *APIError
	if !errors.As(err, &apiErr) || !utf8.ValidString(apiErr.Message) || len(apiErr.Message) > 500 {
		t.Errorf("message %q is not valid, truncated UTF-8", apiErr.Message)
	}
}

func TestFindCheckRun(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/o/r/commits/abc/check-runs" || r.URL.Query().Get("check_name") != "AI Review" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, `{"total_count":2,"check_runs":[{"id":1,"external_id":"other"},{"id":2,"external_id":"run-1"}]}`)
	}))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL, Token: StaticToken("t")}
	for ext, want := range map[string]int64{"run-1": 2, "run-2": 0} {
		if id, err := c.FindCheckRun(context.Background(), "o/r", "abc", "AI Review", ext); err != nil || id != want {
			t.Errorf("FindCheckRun(%s) = %d, %v; want %d", ext, id, err, want)
		}
	}
}

func TestListReviewThreads(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		var req struct {
			Variables map[string]any `json:"variables"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Variables["owner"] != "o" || req.Variables["name"] != "r" || req.Variables["number"] != float64(7) {
			fmt.Fprint(w, `{"errors":[{"message":"bad variables"}]}`)
			return
		}
		fmt.Fprint(w, `{"data":{"repository":{"pullRequest":{"reviewThreads":{"nodes":[
			{"isResolved":true,"isOutdated":false,"path":"a.go","line":3,"originalLine":2,"comments":{"nodes":[{"author":{"login":"bot"},"body":"fixed?"}]}},
			{"isResolved":false,"isOutdated":true,"path":"b.go","line":null,"originalLine":9,"comments":{"nodes":[{"author":null,"body":"ghost"}]}},
			{"isResolved":false,"isOutdated":false,"path":"c.go","line":1,"originalLine":1,"comments":{"nodes":[]}}
		]}}}}}`)
	}))
	defer srv.Close()
	ctx := context.Background()

	threads, err := (&Client{BaseURL: srv.URL, Token: StaticToken("t")}).ListReviewThreads(ctx, "o/r", 7)
	if err != nil || gotPath != "/graphql" {
		t.Fatalf("threads %v, err %v, path %s", threads, err, gotPath)
	}
	want := []ReviewThread{
		{Path: "a.go", Line: 3, Resolved: true, AuthorLogin: "bot", Body: "fixed?"},
		{Path: "b.go", Line: 9, Outdated: true, Body: "ghost"},
	}
	if fmt.Sprint(threads) != fmt.Sprint(want) {
		t.Errorf("threads = %+v, want %+v", threads, want)
	}

	if _, err := (&Client{BaseURL: srv.URL + "/api/v3", Token: StaticToken("t")}).ListReviewThreads(ctx, "o/r", 7); err != nil || gotPath != "/api/graphql" {
		t.Errorf("GitHub Enterprise Server: path %s, err %v", gotPath, err)
	}
	if _, err := (&Client{BaseURL: srv.URL, Token: StaticToken("t")}).ListReviewThreads(ctx, "o/r", 8); err == nil || !strings.Contains(err.Error(), "bad variables") {
		t.Errorf("GraphQL error not reported: %v", err)
	}
}
