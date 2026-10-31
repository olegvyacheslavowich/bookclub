package covers

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
)

const name = "0123abcd-0011223344556677.jpg"

// fakeDisk imitates the parts of the Yandex Disk REST API the app uses: folders, upload and
// download links, and deletion. Files live under their full path ("app:/covers/x.jpg").
type fakeDisk struct {
	mu        sync.Mutex
	dirs      map[string]bool
	files     map[string][]byte
	published map[string]bool
	noPublish bool     // answer publish like a token without the right to do it
	tokenSeen []string // Authorization headers on storage (non-API) requests: must stay empty
	srv       *httptest.Server
}

func newFakeDisk(t *testing.T) *fakeDisk {
	f := &fakeDisk{dirs: map[string]bool{}, files: map[string][]byte{}, published: map[string]bool{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeDisk) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	path := r.URL.Query().Get("path")

	if strings.HasPrefix(r.URL.Path, "/storage/") { // the one-time links: no token expected
		if a := r.Header.Get("Authorization"); a != "" {
			f.tokenSeen = append(f.tokenSeen, a)
		}
		p := r.URL.Query().Get("p")
		switch r.Method {
		case http.MethodPut:
			buf := new(bytes.Buffer)
			buf.ReadFrom(r.Body)
			f.files[p] = buf.Bytes()
			w.WriteHeader(http.StatusCreated)
		case http.MethodGet:
			w.Write(f.files[p])
		}
		return
	}

	if r.Header.Get("Authorization") != "OAuth good" {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":"UnauthorizedError","message":"Не авторизован."}`))
		return
	}
	parent := path[:strings.LastIndex(path, "/")]
	parentOK := strings.HasSuffix(parent, ":") || f.dirs[parent]

	switch {
	case r.Method == http.MethodPut && r.URL.Path == "/resources":
		switch {
		case f.dirs[path]:
			w.WriteHeader(http.StatusConflict)
			w.Write([]byte(`{"error":"DiskPathPointsToExistentDirectoryError"}`))
		case !parentOK:
			w.WriteHeader(http.StatusConflict)
			w.Write([]byte(`{"error":"DiskPathDoesntExistsError"}`))
		default:
			f.dirs[path] = true
			w.WriteHeader(http.StatusCreated)
			w.Write([]byte(`{"href":"x"}`))
		}
	case r.Method == http.MethodGet && r.URL.Path == "/resources/upload":
		if !parentOK {
			w.WriteHeader(http.StatusConflict)
			w.Write([]byte(`{"error":"DiskPathDoesntExistsError"}`))
			return
		}
		w.Write([]byte(`{"href":"` + f.srv.URL + `/storage/up?p=` + url.QueryEscape(path) + `","method":"PUT"}`))
	case r.Method == http.MethodGet && r.URL.Path == "/resources/download":
		if _, ok := f.files[path]; !ok {
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"error":"DiskNotFoundError"}`))
			return
		}
		w.Write([]byte(`{"href":"` + f.srv.URL + `/storage/down?p=` + url.QueryEscape(path) + `","method":"GET"}`))
	case r.Method == http.MethodPut && r.URL.Path == "/resources/publish":
		if f.noPublish {
			w.WriteHeader(http.StatusForbidden)
			w.Write([]byte(`{"error":"ForbiddenError","message":"Нет доступа."}`))
			return
		}
		if _, ok := f.files[path]; !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		f.published[path] = true
		w.Write([]byte(`{"href":"x","method":"GET"}`))
	case r.Method == http.MethodGet && r.URL.Path == "/resources":
		if _, ok := f.files[path]; !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.URL.Query().Get("fields") != "public_url" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if f.published[path] {
			w.Write([]byte(`{"public_url":"https://yadi.sk/i/` + url.PathEscape(path[strings.LastIndex(path, "/")+1:]) + `"}`))
		} else {
			w.Write([]byte(`{}`))
		}
	case r.Method == http.MethodDelete && r.URL.Path == "/resources":
		if _, ok := f.files[path]; !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.URL.Query().Get("permanently") != "true" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		delete(f.files, path)
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func newDisk(t *testing.T, f *fakeDisk, token, dir string) *Disk {
	t.Helper()
	d, err := NewDisk(DiskConfig{Token: token, Dir: dir, BaseURL: f.srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestDiskRoundTrip(t *testing.T) {
	f := newFakeDisk(t)
	d := newDisk(t, f, "good", "disk:/Книжный клуб/Обложки")
	ctx := context.Background()

	if err := d.Put(ctx, name, []byte("photo")); err != nil {
		t.Fatal(err)
	}
	if !f.dirs["disk:/Книжный клуб"] || !f.dirs["disk:/Книжный клуб/Обложки"] {
		t.Fatalf("folders not created: %v", f.dirs)
	}
	got, err := d.Get(ctx, name)
	if err != nil || string(got) != "photo" {
		t.Fatalf("got %q, %v", got, err)
	}

	// A second process finds the folder already there (409) and carries on.
	if err := newDisk(t, f, "good", "disk:/Книжный клуб/Обложки").Put(ctx, name, []byte("v2")); err != nil {
		t.Fatal(err)
	}

	if err := d.Delete(ctx, name); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Get(ctx, name); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after delete: %v", err)
	}
	if err := d.Delete(ctx, name); err != nil {
		t.Fatalf("deleting a missing file must succeed, got %v", err)
	}
	if len(f.tokenSeen) > 0 {
		t.Fatalf("token sent to the storage links: %v", f.tokenSeen)
	}
}

func TestDiskDefaultsToAppFolder(t *testing.T) {
	f := newFakeDisk(t)
	d := newDisk(t, f, "good", "")
	if err := d.Put(context.Background(), name, []byte("x")); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.files["app:/covers/"+name]; !ok {
		t.Fatalf("files: %v", f.files)
	}
}

func TestDiskBadToken(t *testing.T) {
	f := newFakeDisk(t)
	err := newDisk(t, f, "secret-token-123", "app:/covers").Put(context.Background(), name, []byte("x"))
	if err == nil || !strings.Contains(err.Error(), "YADISK_TOKEN") {
		t.Fatalf("got %v", err)
	}
	if strings.Contains(err.Error(), "secret-token-123") {
		t.Fatal("the token leaked into the error")
	}
}

func TestBadNamesNeverReachStorage(t *testing.T) {
	f := newFakeDisk(t)
	d := newDisk(t, f, "good", "app:/covers")
	dir, err := NewDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range []Backend{d, dir} {
		for _, n := range []string{"../x.jpg", "a/b.jpg", "x.exe", "", "abc-0011223344556677.gif"} {
			if err := b.Put(context.Background(), n, []byte("x")); err == nil {
				t.Errorf("%T accepted %q", b, n)
			}
		}
	}
}

func TestNewDiskChecksFolder(t *testing.T) {
	if _, err := NewDisk(DiskConfig{Token: "t", Dir: "/covers"}); err == nil {
		t.Fatal("a folder without app:/ or disk:/ must be rejected")
	}
	if _, err := NewDisk(DiskConfig{Dir: "app:/covers"}); err == nil {
		t.Fatal("a missing token must be rejected")
	}
}

func TestDir(t *testing.T) {
	d, err := NewDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := d.Get(ctx, name); !errors.Is(err, ErrNotFound) {
		t.Fatalf("got %v", err)
	}
	if err := d.Put(ctx, name, []byte("photo")); err != nil {
		t.Fatal(err)
	}
	if got, _ := d.Get(ctx, name); string(got) != "photo" {
		t.Fatalf("got %q", got)
	}
	if err := d.Delete(ctx, name); err != nil {
		t.Fatal(err)
	}
	if err := d.Delete(ctx, name); err != nil {
		t.Fatal(err)
	}
}

// countingBackend counts Get calls that reach the real storage.
type countingBackend struct {
	Backend
	gets int
}

func (c *countingBackend) Get(ctx context.Context, n string) ([]byte, error) {
	c.gets++
	return c.Backend.Get(ctx, n)
}

func TestCachedKeepsRecentWithinLimit(t *testing.T) {
	dir, _ := NewDir(t.TempDir())
	inner := &countingBackend{Backend: dir}
	c := NewCached(inner, 10)
	ctx := context.Background()
	a, b, big := "aa-0000000000000001.jpg", "bb-0000000000000002.jpg", "cc-0000000000000003.jpg"

	_ = dir.Put(ctx, a, []byte("aaaaa"))
	_ = dir.Put(ctx, b, []byte("bbbbb"))
	_ = dir.Put(ctx, big, bytes.Repeat([]byte("c"), 11))

	c.Get(ctx, a)
	c.Get(ctx, a)
	if inner.gets != 1 {
		t.Fatalf("second read must come from memory, storage reads: %d", inner.gets)
	}
	c.Get(ctx, b) // 10 bytes: both fit
	c.Get(ctx, a) // a is now the most recent
	if inner.gets != 2 {
		t.Fatalf("storage reads: %d", inner.gets)
	}
	_ = c.Put(ctx, "dd-0000000000000004.jpg", []byte("ddddd")) // pushes out b, the least recent
	c.Get(ctx, a)
	c.Get(ctx, b)
	if inner.gets != 3 {
		t.Fatalf("expected only b to be re-read, storage reads: %d", inner.gets)
	}
	c.Get(ctx, big) // larger than the whole cache: served, never kept
	c.Get(ctx, big)
	if inner.gets != 5 || c.size > 10 {
		t.Fatalf("storage reads %d, cached bytes %d", inner.gets, c.size)
	}

	if err := c.Delete(ctx, a); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Get(ctx, a); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted cover still served: %v", err)
	}
}

func TestExt(t *testing.T) {
	jpeg := []byte{0xFF, 0xD8, 0xFF, 0xE0, 0, 0x10, 'J', 'F', 'I', 'F'}
	png := []byte("\x89PNG\r\n\x1a\n0000")
	webp := []byte("RIFF\x00\x00\x00\x00WEBPVP8 ")
	for in, want := range map[string]string{string(jpeg): ".jpg", string(png): ".png", string(webp): ".webp"} {
		if got, ok := Ext([]byte(in)); !ok || got != want {
			t.Errorf("got %q %v, want %q", got, ok, want)
		}
	}
	for _, in := range []string{"<svg xmlns='http://www.w3.org/2000/svg'/>", "GIF89a", "hello"} {
		if _, ok := Ext([]byte(in)); ok {
			t.Errorf("%q accepted", in)
		}
	}
}

func TestDiskPublish(t *testing.T) {
	f := newFakeDisk(t)
	d := newDisk(t, f, "good", "app:/covers")
	ctx := context.Background()
	if err := d.Put(ctx, name, []byte("jpeg")); err != nil {
		t.Fatal(err)
	}
	link, err := d.Publish(ctx, name)
	if err != nil || link != "https://yadi.sk/i/"+name {
		t.Fatalf("publish: %q %v", link, err)
	}
	if _, err := d.Publish(ctx, "0123abcd-ffffffffffffffff.jpg"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing file: %v", err)
	}

	f.mu.Lock()
	f.noPublish = true
	f.mu.Unlock()
	if _, err := d.Publish(ctx, name); err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("no right to publish: %v", err)
	}

	// Through the cache; a local folder can't publish.
	if _, err := NewCached(d, 1<<20).Publish(ctx, name); err == nil {
		t.Fatal("Cached must pass the error through")
	}
	dir, _ := NewDir(t.TempDir())
	if _, err := NewCached(dir, 1<<20).Publish(ctx, name); !errors.Is(err, ErrCannotPublish) {
		t.Fatalf("Dir: %v", err)
	}
}
