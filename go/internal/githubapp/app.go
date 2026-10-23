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
	"net/http"
	"strings"
	"sync"
	"time"
)

// App authenticates as a GitHub App: it signs App JWTs and exchanges them for
// installation tokens.
type App struct {
	// ID is the App ID or client ID, used as the JWT issuer.
	ID      string
	BaseURL string
	HTTP    *http.Client

	key *rsa.PrivateKey
	now func() time.Time

	mu     sync.Mutex
	tokens map[int64]Token
}

// Token is an installation access token.
type Token struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

// TokenScope narrows an installation token. Empty fields keep the
// installation's full access.
type TokenScope struct {
	// Repositories are repository names without the owner.
	Repositories []string          `json:"repositories,omitempty"`
	Permissions  map[string]string `json:"permissions,omitempty"`
}

// NewApp parses the App's PEM private key (PKCS#1 as GitHub issues it, or PKCS#8).
func NewApp(id string, privateKeyPEM []byte, baseURL string) (*App, error) {
	if id == "" {
		return nil, errors.New("github app: ID is required")
	}
	block, _ := pem.Decode(privateKeyPEM)
	if block == nil {
		return nil, errors.New("github app: private key is not PEM")
	}
	var key *rsa.PrivateKey
	if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		key = k
	} else if k8, err8 := x509.ParsePKCS8PrivateKey(block.Bytes); err8 == nil {
		var ok bool
		if key, ok = k8.(*rsa.PrivateKey); !ok {
			return nil, errors.New("github app: private key is not RSA")
		}
	} else {
		return nil, fmt.Errorf("github app: parse private key: %w", err)
	}
	if baseURL == "" {
		baseURL = DefaultAPIURL
	}
	return &App{ID: id, BaseURL: baseURL, key: key, now: time.Now, tokens: map[int64]Token{}}, nil
}

// JWT returns a fresh App JWT, valid for 9 minutes and backdated by one to
// tolerate clock skew (GitHub allows at most 10 minutes).
func (a *App) JWT() (string, error) {
	now := a.now()
	enc := base64.RawURLEncoding
	header := enc.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	claims, err := json.Marshal(map[string]any{
		"iat": now.Add(-time.Minute).Unix(),
		"exp": now.Add(9 * time.Minute).Unix(),
		"iss": a.ID,
	})
	if err != nil {
		return "", err
	}
	signed := header + "." + enc.EncodeToString(claims)
	digest := sha256.Sum256([]byte(signed))
	sig, err := rsa.SignPKCS1v15(rand.Reader, a.key, crypto.SHA256, digest[:])
	if err != nil {
		return "", fmt.Errorf("sign app JWT: %w", err)
	}
	return signed + "." + enc.EncodeToString(sig), nil
}

func (a *App) jwtClient() *Client {
	return &Client{BaseURL: a.BaseURL, HTTP: a.HTTP, Token: func(context.Context) (string, error) { return a.JWT() }}
}

// Slug returns the App's slug; its bot user is "<slug>[bot]".
func (a *App) Slug(ctx context.Context) (string, error) {
	var out struct {
		Slug string `json:"slug"`
	}
	if err := a.jwtClient().do(ctx, http.MethodGet, "/app", nil, &out); err != nil {
		return "", err
	}
	return out.Slug, nil
}

// InstallationToken mints a new installation token, optionally narrowed by scope.
func (a *App) InstallationToken(ctx context.Context, installationID int64, scope *TokenScope) (Token, error) {
	var t Token
	var body any
	if scope != nil {
		body = scope
	}
	err := a.jwtClient().do(ctx, http.MethodPost, fmt.Sprintf("/app/installations/%d/access_tokens", installationID), body, &t)
	if err != nil {
		return Token{}, err
	}
	return t, nil
}

// tokenRefreshMargin renews cached tokens this long before they expire.
const tokenRefreshMargin = 5 * time.Minute

// Installation returns a client acting as the installation with the App's full
// permissions. Tokens are cached and renewed shortly before they expire.
func (a *App) Installation(installationID int64) *Client {
	return &Client{BaseURL: a.BaseURL, HTTP: a.HTTP, Token: func(ctx context.Context) (string, error) {
		a.mu.Lock()
		defer a.mu.Unlock()
		if t, ok := a.tokens[installationID]; ok && a.now().Add(tokenRefreshMargin).Before(t.ExpiresAt) {
			return t.Token, nil
		}
		t, err := a.InstallationToken(ctx, installationID, nil)
		if err != nil {
			return "", err
		}
		a.tokens[installationID] = t
		return t.Token, nil
	}}
}

// RepoName returns the name part of owner/name.
func RepoName(repo string) string {
	_, name, _ := strings.Cut(repo, "/")
	return name
}
