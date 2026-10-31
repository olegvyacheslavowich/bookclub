package api

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"bookclub/internal/covers"
	"bookclub/internal/store"
)

// A minimal valid JPEG header is enough: the server checks the format, not the picture.
var (
	jpegPhoto = "\xFF\xD8\xFF\xE0\x00\x10JFIF\x00" + strings.Repeat("x", 100)
	pngPhoto  = "\x89PNG\r\n\x1a\n" + strings.Repeat("y", 100)
)

// flakyCovers wraps a real folder and can be told to fail uploads, like an unreachable Disk.
type flakyCovers struct {
	*covers.Dir
	mu       sync.Mutex
	failPuts bool
}

func (f *flakyCovers) Put(ctx context.Context, name string, data []byte) error {
	f.mu.Lock()
	fail := f.failPuts
	f.mu.Unlock()
	if fail {
		return errors.New("disk is down")
	}
	return f.Dir.Put(ctx, name, data)
}

type coverHarness struct {
	*harness
	dir    string
	covers *flakyCovers
}

// newCoverHarness: like newAdminHarness (Nina is the administrator), with photos in a temp folder.
func newCoverHarness(t *testing.T) *coverHarness {
	t.Helper()
	return newCoverHarnessWith(t, nil)
}

// newCoverHarnessWith: wrap, if set, puts something around the photo folder (e.g. publishing).
func newCoverHarnessWith(t *testing.T, wrap func(*flakyCovers) CoverStore) *coverHarness {
	t.Helper()
	st, err := store.OpenFile(filepath.Join(t.TempDir(), "data.json"))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	d, err := covers.NewDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	fc := &flakyCovers{Dir: d}
	var cs CoverStore = fc
	if wrap != nil {
		cs = wrap(fc)
	}
	s := New(Config{
		Store:  st,
		Covers: cs,
		Auth: fakeAuth{
			anna: {ID: 1, FirstName: "Анна"},
			olga: {ID: 2, FirstName: "Ольга"},
			nina: {ID: 4, FirstName: "Нина"},
		},
		AdminIDs: []int64{4},
		AppURL:   "https://t.me/club_bot/club",
		ShareKey: []byte("test share key"),
		Logger:   discardLogger(),
	})
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return &coverHarness{harness: &harness{t: t, srv: srv}, dir: dir, covers: fc}
}

func (h *coverHarness) files() []string {
	h.t.Helper()
	entries, err := os.ReadDir(h.dir)
	if err != nil {
		h.t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// get fetches a cover and returns status, body and Cache-Control.
func (h *coverHarness) get(who, path string) (int, string, string) {
	h.t.Helper()
	req, _ := http.NewRequest("GET", h.srv.URL+"/"+path, nil)
	req.Header.Set(AuthHeader, "tma "+who)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body), resp.Header.Get("Cache-Control")
}

func (h *coverHarness) book(who, id string) bookDTO {
	h.t.Helper()
	var d bookDetailDTO
	h.do(who, "GET", "/api/books/"+id, "", &d)
	return d.Book
}

func TestCoverFlow(t *testing.T) {
	h := newCoverHarness(t)

	var me meDTO
	h.do(olga, "GET", "/api/me", "", &me)
	if !me.Covers {
		t.Fatal("me.covers must be true when photo storage is configured")
	}

	var book bookDTO
	h.do(olga, "POST", "/api/books", `{"title":"Обломов"}`, &book)
	if book.CoverURL != "" || !book.CanEdit {
		t.Fatalf("new book: %+v", book)
	}

	// Only the one who added the book (or an administrator) sets the photo.
	if code := h.do(anna, "PUT", "/api/books/"+book.ID+"/cover", jpegPhoto, nil); code != http.StatusForbidden {
		t.Fatalf("Anna on Olga's book: %d", code)
	}
	if h.book(anna, book.ID).CanEdit {
		t.Fatal("Anna must not see the photo buttons on Olga's book")
	}

	var up struct{ CoverURL string }
	if code := h.do(olga, "PUT", "/api/books/"+book.ID+"/cover", jpegPhoto, &up); code != http.StatusOK {
		t.Fatalf("upload: %d", code)
	}
	if !strings.HasPrefix(up.CoverURL, "api/books/"+book.ID+"/cover?v=") || !strings.HasSuffix(up.CoverURL, ".jpg") {
		t.Fatalf("coverUrl: %q", up.CoverURL)
	}
	if got := h.book(anna, book.ID).CoverURL; got != up.CoverURL {
		t.Fatalf("everyone sees the cover: got %q", got)
	}
	var list struct{ Books []bookDTO }
	h.do(anna, "GET", "/api/books", "", &list)
	if list.Books[0].CoverURL != up.CoverURL {
		t.Fatalf("list: %+v", list.Books[0])
	}

	code, body, cache := h.get(anna, up.CoverURL)
	if code != http.StatusOK || body != jpegPhoto || !strings.Contains(cache, "immutable") || !strings.Contains(cache, "private") {
		t.Fatalf("get: %d %q %q", code, cache, body[:min(10, len(body))])
	}
	if code, _, cache := h.get(anna, "api/books/"+book.ID+"/cover"); code != http.StatusOK || strings.Contains(cache, "immutable") {
		t.Fatalf("without ?v= the answer must not be cached forever: %d %q", code, cache)
	}

	// Replacing: a new file name, the old file is gone.
	first := h.files()
	h.do(olga, "PUT", "/api/books/"+book.ID+"/cover", pngPhoto, &up)
	if files := h.files(); len(files) != 1 || files[0] == first[0] || !strings.HasSuffix(files[0], ".png") {
		t.Fatalf("after replace: %v (before %v)", files, first)
	}
	if code, body, _ := h.get(olga, up.CoverURL); code != http.StatusOK || body != pngPhoto {
		t.Fatalf("replaced cover: %d", code)
	}

	// The administrator may remove it.
	if code := h.do(nina, "DELETE", "/api/books/"+book.ID+"/cover", "", nil); code != http.StatusNoContent {
		t.Fatalf("admin delete: %d", code)
	}
	if h.book(olga, book.ID).CoverURL != "" || len(h.files()) != 0 {
		t.Fatalf("cover still there: %v", h.files())
	}
	if code, _, _ := h.get(olga, "api/books/"+book.ID+"/cover"); code != http.StatusNotFound {
		t.Fatalf("no cover: %d", code)
	}

	// Deleting the book deletes its photo.
	h.do(nina, "PUT", "/api/books/"+book.ID+"/cover", jpegPhoto, nil)
	h.do(olga, "DELETE", "/api/books/"+book.ID, "", nil)
	if files := h.files(); len(files) != 0 {
		t.Fatalf("photo of a deleted book: %v", files)
	}
}

func TestCoverRejectsBadUploads(t *testing.T) {
	h := newCoverHarness(t)
	var book bookDTO
	h.do(olga, "POST", "/api/books", `{"title":"Обломов"}`, &book)
	path := "/api/books/" + book.ID + "/cover"

	for _, body := range []string{"", "<svg xmlns='http://www.w3.org/2000/svg'><script>alert(1)</script></svg>", "GIF89a......"} {
		if code := h.do(olga, "PUT", path, body, nil); code != http.StatusUnsupportedMediaType {
			t.Errorf("%.10q: got %d", body, code)
		}
	}
	big := "\xFF\xD8\xFF\xE0" + strings.Repeat("x", covers.MaxSize)
	if code := h.do(olga, "PUT", path, big, nil); code != http.StatusRequestEntityTooLarge {
		t.Fatalf("too big: %d", code)
	}
	if code := h.do(olga, "PUT", "/api/books/ffff/cover", jpegPhoto, nil); code != http.StatusNotFound {
		t.Fatalf("unknown book: %d", code)
	}
	if len(h.files()) != 0 || h.book(olga, book.ID).CoverURL != "" {
		t.Fatal("a rejected upload left traces")
	}
}

func TestCoverUploadFailureKeepsOldPhoto(t *testing.T) {
	h := newCoverHarness(t)
	var book bookDTO
	h.do(olga, "POST", "/api/books", `{"title":"Обломов"}`, &book)
	var up struct{ CoverURL string }
	h.do(olga, "PUT", "/api/books/"+book.ID+"/cover", jpegPhoto, &up)

	h.covers.mu.Lock()
	h.covers.failPuts = true
	h.covers.mu.Unlock()
	var e map[string]string
	if code := h.do(olga, "PUT", "/api/books/"+book.ID+"/cover", pngPhoto, &e); code != http.StatusBadGateway || e["error"] == "" {
		t.Fatalf("got %d %v", code, e)
	}
	if got := h.book(olga, book.ID).CoverURL; got != up.CoverURL {
		t.Fatalf("the old photo must stay: %q", got)
	}
	if code, body, _ := h.get(olga, up.CoverURL); code != http.StatusOK || body != jpegPhoto {
		t.Fatalf("old photo: %d", code)
	}
}

func TestNoCoversWithoutStorage(t *testing.T) {
	h := newHarness(t)
	var me meDTO
	h.do(olga, "GET", "/api/me", "", &me)
	if me.Covers {
		t.Fatal("me.covers must be false without photo storage")
	}
	var book bookDTO
	h.do(olga, "POST", "/api/books", `{"title":"Обломов"}`, &book)
	if code := h.do(olga, "PUT", "/api/books/"+book.ID+"/cover", jpegPhoto, nil); code != http.StatusNotFound {
		t.Fatalf("upload without storage: %d", code)
	}
}
