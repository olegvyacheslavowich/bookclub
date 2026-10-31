package covers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// DiskConfig: where on Yandex Disk the covers live.
type DiskConfig struct {
	// Token is an OAuth token of the Disk owner. With only the "app folder" permission
	// (cloud_api:disk.app_folder) it can't see anything else on the owner's Disk.
	Token string
	// Dir is the folder for covers: "app:/covers" (inside the app's own folder, created by Disk
	// as «Приложения/<app name>») or "disk:/Книжный клуб/Обложки" with full Disk access.
	Dir     string
	BaseURL string // default "https://cloud-api.yandex.net/v1/disk"; tests point it elsewhere
	Client  *http.Client
}

// Disk stores covers on Yandex Disk through its REST API.
//
// Upload and download take two steps each: the API hands out a one-time link to the storage
// server, and the file goes over that link. The token is sent only to the API, never with the file.
type Disk struct {
	cfg DiskConfig

	mu       sync.Mutex
	dirReady bool // Dir exists (checked once per process)
}

func NewDisk(cfg DiskConfig) (*Disk, error) {
	if cfg.Token == "" {
		return nil, errors.New("covers: Yandex Disk token is required")
	}
	if cfg.Dir == "" {
		cfg.Dir = "app:/covers"
	}
	cfg.Dir = strings.TrimRight(cfg.Dir, "/")
	if !strings.HasPrefix(cfg.Dir, "app:/") && !strings.HasPrefix(cfg.Dir, "disk:/") {
		return nil, errors.New(`covers: Yandex Disk folder must start with "app:/" or "disk:/"`)
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = "https://cloud-api.yandex.net/v1/disk"
	}
	if cfg.Client == nil {
		cfg.Client = &http.Client{Timeout: 20 * time.Second}
	}
	return &Disk{cfg: cfg}, nil
}

func (d *Disk) path(name string) string { return d.cfg.Dir + "/" + name }

// Put uploads a cover. An existing file with the same name is replaced.
func (d *Disk) Put(ctx context.Context, name string, data []byte) error {
	if err := checkName(name); err != nil {
		return err
	}
	if err := d.ensureDir(ctx); err != nil {
		return err
	}

	var link struct{ Href, Method string }
	q := url.Values{"path": {d.path(name)}, "overwrite": {"true"}}
	if err := d.call(ctx, http.MethodGet, "/resources/upload", q, &link); err != nil {
		return err
	}
	if link.Method == "" {
		link.Method = http.MethodPut
	}

	req, err := http.NewRequestWithContext(ctx, link.Method, link.Href, bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("covers: upload link: %w", err)
	}
	req.Header.Set("Content-Type", ContentType(name))
	resp, err := d.cfg.Client.Do(req) // no token: the link itself grants the upload
	if err != nil {
		return fmt.Errorf("covers: upload: %w", err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusCreated, http.StatusAccepted, http.StatusOK:
		return nil
	case http.StatusInsufficientStorage:
		return errors.New("covers: upload: the Yandex Disk is full")
	default:
		return fmt.Errorf("covers: upload: HTTP %d", resp.StatusCode)
	}
}

// Get downloads a cover.
func (d *Disk) Get(ctx context.Context, name string) ([]byte, error) {
	if err := checkName(name); err != nil {
		return nil, err
	}
	var link struct{ Href string }
	if err := d.call(ctx, http.MethodGet, "/resources/download", url.Values{"path": {d.path(name)}}, &link); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, link.Href, nil)
	if err != nil {
		return nil, fmt.Errorf("covers: download link: %w", err)
	}
	resp, err := d.cfg.Client.Do(req) // the storage server redirects; Go follows it
	if err != nil {
		return nil, fmt.Errorf("covers: download: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("covers: download: HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, MaxSize+1))
	if err != nil {
		return nil, fmt.Errorf("covers: download: %w", err)
	}
	if len(data) > MaxSize {
		return nil, errors.New("covers: download: file is larger than a cover can be")
	}
	return data, nil
}

// Delete removes a cover for good, bypassing the Disk's trash. A missing file is not an error.
func (d *Disk) Delete(ctx context.Context, name string) error {
	if err := checkName(name); err != nil {
		return err
	}
	q := url.Values{"path": {d.path(name)}, "permanently": {"true"}}
	err := d.call(ctx, http.MethodDelete, "/resources", q, nil)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

// Publish opens public access to a photo and returns its public link (https://yadi.sk/i/…). Yandex
// Disk's own page behind that link shows the photo, so Telegram can build a link preview from it
// even when it can't reach our server. Publishing an already public file is harmless.
func (d *Disk) Publish(ctx context.Context, name string) (string, error) {
	if err := checkName(name); err != nil {
		return "", err
	}
	q := url.Values{"path": {d.path(name)}}
	if err := d.call(ctx, http.MethodPut, "/resources/publish", q, nil); err != nil {
		return "", err
	}
	var res struct {
		PublicURL string `json:"public_url"`
	}
	q.Set("fields", "public_url")
	if err := d.call(ctx, http.MethodGet, "/resources", q, &res); err != nil {
		return "", err
	}
	if !strings.HasPrefix(res.PublicURL, "https://") {
		return "", fmt.Errorf("covers: Yandex Disk published %s but gave no public link", name)
	}
	return res.PublicURL, nil
}

// ensureDir creates Dir and its parents. Disk refuses to upload into a folder that doesn't exist.
func (d *Disk) ensureDir(ctx context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.dirReady {
		return nil
	}
	// "app:/a/b" -> create "app:/a", then "app:/a/b". The root itself always exists.
	scheme, rest, _ := strings.Cut(d.cfg.Dir, ":/")
	cur := scheme + ":"
	for _, part := range strings.Split(rest, "/") {
		if part == "" {
			continue
		}
		cur += "/" + part
		err := d.call(ctx, http.MethodPut, "/resources", url.Values{"path": {cur}}, nil)
		if err != nil && !errors.Is(err, errExists) {
			return err
		}
	}
	d.dirReady = true
	return nil
}

var errExists = errors.New("covers: already exists")

// apiError is Disk's error answer: {"error": "DiskNotFoundError", "message": "..."}.
type apiError struct {
	Error       string `json:"error"`
	Message     string `json:"message"`
	Description string `json:"description"`
}

// call makes one request to the Disk API with the token. On a 2xx answer the JSON body is decoded
// into out, if set.
func (d *Disk) call(ctx context.Context, method, path string, q url.Values, out any) error {
	u := strings.TrimRight(d.cfg.BaseURL, "/") + path + "?" + q.Encode()
	req, err := http.NewRequestWithContext(ctx, method, u, nil)
	if err != nil {
		return fmt.Errorf("covers: %w", err)
	}
	req.Header.Set("Authorization", "OAuth "+d.cfg.Token)
	req.Header.Set("Accept", "application/json")

	resp, err := d.cfg.Client.Do(req)
	if err != nil {
		// *url.Error carries the URL, which has only the path of a cover, never the token.
		return fmt.Errorf("covers: Yandex Disk %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		if out != nil {
			if err := json.Unmarshal(body, out); err != nil {
				return fmt.Errorf("covers: Yandex Disk %s %s: bad answer: %w", method, path, err)
			}
		}
		return nil
	}

	var ae apiError
	_ = json.Unmarshal(body, &ae)
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return ErrNotFound
	case resp.StatusCode == http.StatusConflict && method == http.MethodPut && path == "/resources":
		return errExists
	case resp.StatusCode == http.StatusUnauthorized:
		return fmt.Errorf("covers: Yandex Disk rejected YADISK_TOKEN (expired or revoked?): %s %s", ae.Error, ae.Message)
	}
	return fmt.Errorf("covers: Yandex Disk %s %s: HTTP %d %s %s", method, path, resp.StatusCode, ae.Error, ae.Message)
}
