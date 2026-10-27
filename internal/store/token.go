package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

// TokenSource supplies IAM tokens for Object Storage.
type TokenSource interface {
	Token(ctx context.Context) (string, error)
	Invalidate()
}

// StaticToken is a fixed IAM token, e.g. from `yc iam create-token`, for running locally
// against a real bucket. It lives up to 12 hours.
type StaticToken string

func (t StaticToken) Token(context.Context) (string, error) { return string(t), nil }
func (t StaticToken) Invalidate()                           {}

// MetadataToken gets the service account's IAM token from the metadata service, which is how a
// Serverless Container authenticates as the service account set in its revision.
// Tokens are cached until five minutes before they expire.
type MetadataToken struct {
	URL    string // default: the documented metadata endpoint
	Client *http.Client
	now    func() time.Time

	mu      sync.Mutex
	token   string
	expires time.Time
}

const metadataTokenURL = "http://169.254.169.254/computeMetadata/v1/instance/service-accounts/default/token"

func (m *MetadataToken) Token(ctx context.Context) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now
	if m.now != nil {
		now = m.now
	}
	if m.token != "" && now().Before(m.expires.Add(-5*time.Minute)) {
		return m.token, nil
	}

	endpoint, client := m.URL, m.Client
	if endpoint == "" {
		endpoint = metadataTokenURL
	}
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Metadata-Flavor", "Google")
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("metadata service: %w (is a service account set on the container revision?)", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("metadata service: HTTP %d", resp.StatusCode)
	}

	var body struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&body); err != nil {
		return "", fmt.Errorf("metadata service: decode: %w", err)
	}
	if body.AccessToken == "" {
		return "", errors.New("metadata service: empty token")
	}
	m.token = body.AccessToken
	m.expires = now().Add(time.Duration(body.ExpiresIn) * time.Second)
	return m.token, nil
}

func (m *MetadataToken) Invalidate() {
	m.mu.Lock()
	m.token = ""
	m.mu.Unlock()
}
