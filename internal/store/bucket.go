package store

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
)

// BucketConfig describes where in Yandex Object Storage the document lives.
type BucketConfig struct {
	Bucket   string
	Key      string      // object name, default "bookclub.json"
	Endpoint string      // default "https://storage.yandexcloud.net"
	Token    TokenSource // IAM token for "Authorization: Bearer"; see MetadataToken
	Client   *http.Client
	Logger   *slog.Logger
}

// OpenBucket keeps the data as one object in an Object Storage bucket, read and written over the
// S3-compatible HTTP API with an IAM token (no request signing, no static keys).
// Several app instances may share the bucket; see the package comment for how writes are merged.
func OpenBucket(cfg BucketConfig) (*Store, error) {
	if cfg.Bucket == "" || cfg.Token == nil {
		return nil, fmt.Errorf("store: bucket name and token source are required")
	}
	if cfg.Key == "" {
		cfg.Key = "bookclub.json"
	}
	if cfg.Endpoint == "" {
		cfg.Endpoint = "https://storage.yandexcloud.net"
	}
	if cfg.Client == nil {
		cfg.Client = &http.Client{Timeout: ioTimeout}
	}
	return open(&bucketBackend{cfg: cfg}, cfg.Logger)
}

type bucketBackend struct{ cfg BucketConfig }

func (b *bucketBackend) shared() bool { return true }

func (b *bucketBackend) objectURL(key string) string {
	// Path-style addressing: https://storage.yandexcloud.net/<bucket>/<key>
	return strings.TrimRight(b.cfg.Endpoint, "/") + "/" + url.PathEscape(b.cfg.Bucket) + "/" + (&url.URL{Path: key}).EscapedPath()
}

func (b *bucketBackend) load(ctx context.Context, ifNoneMatch string) ([]byte, string, error) {
	resp, err := b.do(ctx, http.MethodGet, b.cfg.Key, nil, func(h http.Header) {
		if ifNoneMatch != "" {
			h.Set("If-None-Match", ifNoneMatch)
		}
	})
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
		raw, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
		if err != nil {
			return nil, "", fmt.Errorf("store: read object: %w", err)
		}
		return raw, resp.Header.Get("ETag"), nil
	case http.StatusNotModified:
		return nil, "", errNotModified
	case http.StatusNotFound:
		return nil, "", fs.ErrNotExist
	default:
		return nil, "", statusError("GET", resp)
	}
}

func (b *bucketBackend) save(ctx context.Context, data []byte, ifMatch string) (string, error) {
	resp, err := b.do(ctx, http.MethodPut, b.cfg.Key, data, func(h http.Header) {
		h.Set("Content-Type", "application/json")
		if ifMatch != "" {
			h.Set("If-Match", ifMatch)
		} else {
			h.Set("If-None-Match", "*") // first write: only if nobody created the object meanwhile
		}
	})
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
		return resp.Header.Get("ETag"), nil
	case http.StatusPreconditionFailed, http.StatusConflict:
		return "", errConflict
	default:
		return "", statusError("PUT", resp)
	}
}

func (b *bucketBackend) backup(ctx context.Context, name string, data []byte) error {
	resp, err := b.do(ctx, http.MethodPut, name, data, func(h http.Header) {
		h.Set("Content-Type", "application/json")
		h.Set("If-None-Match", "*")
	})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK, http.StatusPreconditionFailed, http.StatusConflict: // 412/409: another instance already made today's copy
		return nil
	default:
		return statusError("PUT backup", resp)
	}
}

func (b *bucketBackend) do(ctx context.Context, method, key string, body []byte, setHeaders func(http.Header)) (*http.Response, error) {
	token, err := b.cfg.Token.Token(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: IAM token: %w", err)
	}
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, b.objectURL(key), rd)
	if err != nil {
		return nil, fmt.Errorf("store: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	setHeaders(req.Header)

	resp, err := b.cfg.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("store: %s %s: %w", method, key, err)
	}
	if resp.StatusCode == http.StatusUnauthorized {
		b.cfg.Token.Invalidate() // expired or revoked: fetch a fresh one next time
	}
	return resp, nil
}

// statusError keeps the S3 error code from the XML body (e.g. AccessDenied, NoSuchBucket).
func statusError(op string, resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	code := ""
	if i := bytes.Index(body, []byte("<Code>")); i >= 0 {
		rest := body[i+len("<Code>"):]
		if j := bytes.Index(rest, []byte("</Code>")); j >= 0 {
			code = string(rest[:j])
		}
	}
	return fmt.Errorf("store: object storage %s: HTTP %d %s", op, resp.StatusCode, code)
}
