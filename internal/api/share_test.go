package api

import (
	"context"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"testing"
)

// public fetches a URL without member credentials, like Telegram's link-preview crawler.
func (h *coverHarness) public(path string) (int, string, http.Header) {
	h.t.Helper()
	resp, err := http.Get(h.srv.URL + "/" + strings.TrimPrefix(path, "/"))
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body), resp.Header
}

var ogImage = regexp.MustCompile(`<meta property="og:image" content="([^"]+)">`)

func TestSharedReviewPreview(t *testing.T) {
	h := newCoverHarness(t)
	var book bookDTO
	h.do(anna, "POST", "/api/books", `{"title":"Обломов","author":"Гончаров"}`, &book)
	var rv reviewDTO
	h.do(olga, "PUT", "/api/books/"+book.ID+"/review", `{"rating":4,"text":"Тайный текст отзыва"}`, &rv)

	// Neither the review nor the book has a photo: no preview link, the app shares as before.
	if rv.ShareURL != "" || h.myReview(olga, book.ID).ShareURL != "" {
		t.Fatalf("shareUrl without photos: %q", rv.ShareURL)
	}

	// The book's cover is used while the review has no photo of its own.
	h.do(anna, "PUT", "/api/books/"+book.ID+"/cover", pngPhoto, nil)
	share := h.myReview(olga, book.ID).ShareURL
	if !strings.HasPrefix(share, "s/"+rv.ID+"-") {
		t.Fatalf("shareUrl: %q", share)
	}
	var d bookDetailDTO
	h.do(anna, "GET", "/api/books/"+book.ID, "", &d)
	if d.Reviews[0].ShareURL != "" {
		t.Fatal("only the author gets the share link of a review")
	}

	code, page, hdr := h.public(share)
	if code != http.StatusOK || !strings.HasPrefix(hdr.Get("Content-Type"), "text/html") {
		t.Fatalf("page: %d %s", code, hdr.Get("Content-Type"))
	}
	for _, want := range []string{
		`content="«Обломов» — Гончаров"`, "★★★★☆ 4 из 5", "Открыть в приложении",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	if strings.Contains(page, "Тайный текст") || strings.Contains(page, "Ольга") {
		t.Error("the public page must not show the review text or the author")
	}
	if strings.Contains(page, "<script") {
		t.Error("the page must not send people on: from a browser the app refuses access")
	}
	if strings.HasSuffix(share, ".jpg") || strings.HasSuffix(share, ".png") {
		t.Errorf("a share link ending like an image may not be read as a page: %q", share)
	}
	m := ogImage.FindStringSubmatch(page)
	if m == nil {
		t.Fatalf("no og:image:\n%s", page)
	}
	img := html.UnescapeString(m[1])
	if !strings.HasPrefix(img, h.srv.URL+"/s/") {
		t.Fatalf("og:image must be absolute: %q", img)
	}
	imgPath := strings.TrimPrefix(img, h.srv.URL)
	if code, body, hdr := h.public(imgPath); code != http.StatusOK || body != pngPhoto || !strings.Contains(hdr.Get("Cache-Control"), "public") {
		t.Fatalf("cover image: %d %q", code, hdr.Get("Cache-Control"))
	}

	// With its own photo the review shows that one, under a new link (Telegram caches per URL).
	h.do(olga, "PUT", "/api/reviews/"+rv.ID+"/photo", jpegPhoto, nil)
	share2 := h.myReview(olga, book.ID).ShareURL
	if share2 == share {
		t.Fatal("the share link must change with the photo")
	}
	_, page, _ = h.public(share2)
	img = html.UnescapeString(ogImage.FindStringSubmatch(page)[1])
	if _, body, _ := h.public(strings.TrimPrefix(img, h.srv.URL)); body != jpegPhoto {
		t.Fatal("the review's own photo must win over the cover")
	}

	// Forged or stale links lead nowhere.
	for _, bad := range []string{
		"s/" + rv.ID + "-AAAAAAAAAAAAAAAAAAAAAA", "s/" + rv.ID, "s/ffff-" + strings.SplitN(share, "-", 2)[1],
		"s/" + rv.ID + "-AAAAAAAAAAAAAAAAAAAAAA/photo",
	} {
		if code, page, _ := h.public(bad); code != http.StatusNotFound || strings.Contains(page, "og:image") {
			t.Errorf("%s: %d", bad, code)
		}
	}
	h.do(olga, "DELETE", "/api/reviews/"+rv.ID, "", nil)
	if code, _, _ := h.public(share2); code != http.StatusNotFound {
		t.Fatalf("deleted review: %d", code)
	}
}

func TestShareLinksDependOnKey(t *testing.T) {
	a := New(Config{ShareKey: []byte("one"), Logger: discardLogger()})
	b := New(Config{ShareKey: []byte("two"), Logger: discardLogger()})
	tok := a.shareToken("0123456789abcdef")
	if _, ok := a.checkShareToken(tok); !ok {
		t.Fatal("own token rejected")
	}
	// Signatures are base64url and may contain "-" themselves: every token must check out.
	withDash := 0
	for i := range 2000 {
		id := fmt.Sprintf("%016x", i)
		tok := a.shareToken(id)
		if strings.Count(tok, "-") > 1 {
			withDash++
		}
		if got, ok := a.checkShareToken(tok); !ok || got != id {
			t.Fatalf("token %q rejected", tok)
		}
	}
	if withDash == 0 {
		t.Fatal("the test must cover signatures with a dash")
	}
	if _, ok := b.checkShareToken(tok); ok {
		t.Fatal("a token signed with another key must not work")
	}
}

func TestPublicBase(t *testing.T) {
	s := New(Config{Logger: discardLogger()})
	for host, want := range map[string]string{
		"bba123.containers.yandexcloud.net": "https://bba123.containers.yandexcloud.net",
		"localhost:8080":                    "http://localhost:8080",
		"127.0.0.1:8080":                    "http://127.0.0.1:8080",
	} {
		r, _ := http.NewRequest("GET", "/", nil)
		r.Host = host
		if got := s.publicBase(r); got != want {
			t.Errorf("%s: %s", host, got)
		}
	}
	s = New(Config{PublicURL: "https://club.example.org/", Logger: discardLogger()})
	r, _ := http.NewRequest("GET", "/", nil)
	if got := s.publicBase(r); got != "https://club.example.org" {
		t.Errorf("PUBLIC_URL: %s", got)
	}
}

// diskLinks imitates Yandex Disk publishing on top of the photo folder.
type diskLinks struct {
	*flakyCovers
	mu    sync.Mutex
	calls int
	fail  bool
}

func (d *diskLinks) Publish(_ context.Context, name string) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls++
	if d.fail {
		return "", errors.New("HTTP 403 ForbiddenError")
	}
	return "https://yadi.sk/i/" + name, nil
}

func (d *diskLinks) published() int { d.mu.Lock(); defer d.mu.Unlock(); return d.calls }

func TestShareLinkIsPublishedOnDisk(t *testing.T) {
	var disk *diskLinks
	h := newCoverHarnessWith(t, func(fc *flakyCovers) CoverStore { disk = &diskLinks{flakyCovers: fc}; return disk })
	var book bookDTO
	h.do(anna, "POST", "/api/books", `{"title":"Обломов"}`, &book)
	var rv reviewDTO
	h.do(olga, "PUT", "/api/books/"+book.ID+"/review", `{"rating":4}`, &rv)

	// The cover while the review has no photo; published once, then the saved link is reused.
	h.do(anna, "PUT", "/api/books/"+book.ID+"/cover", jpegPhoto, nil)
	cover := h.files()[0]
	for range 3 {
		if got := h.myReview(olga, book.ID).ShareURL; got != "https://yadi.sk/i/"+cover {
			t.Fatalf("cover link: %q", got)
		}
	}
	if n := disk.published(); n != 1 {
		t.Fatalf("published %d times, want once", n)
	}

	// The review's own photo wins and gets its own link.
	var up struct{ PhotoURL string }
	h.do(olga, "PUT", "/api/reviews/"+rv.ID+"/photo", jpegPhoto, &up)
	photo := up.PhotoURL[strings.Index(up.PhotoURL, "v=")+2:]
	if got := h.myReview(olga, book.ID).ShareURL; got != "https://yadi.sk/i/"+photo {
		t.Fatalf("photo link: %q (photo %s)", got, photo)
	}

	// Others never trigger publishing and never see the link.
	before := disk.published()
	var d bookDetailDTO
	h.do(anna, "GET", "/api/books/"+book.ID, "", &d)
	if d.Reviews[0].ShareURL != "" || disk.published() != before {
		t.Fatal("only the author's own page publishes and gets the link")
	}
}

func TestShareLinkWhenDiskRefuses(t *testing.T) {
	var disk *diskLinks
	h := newCoverHarnessWith(t, func(fc *flakyCovers) CoverStore { disk = &diskLinks{flakyCovers: fc, fail: true}; return disk })
	var book bookDTO
	h.do(anna, "POST", "/api/books", `{"title":"Обломов"}`, &book)
	var rv reviewDTO
	h.do(olga, "PUT", "/api/books/"+book.ID+"/review", `{"rating":4}`, &rv)
	h.do(olga, "PUT", "/api/reviews/"+rv.ID+"/photo", jpegPhoto, nil)

	// No link: the app shares the plain Mini App link. Our own page would be useless here:
	// Telegram can't reach the server, that's why Disk is used.
	for range 3 {
		if got := h.myReview(olga, book.ID).ShareURL; got != "" {
			t.Fatalf("shareUrl after a refused publish: %q", got)
		}
	}
	if n := disk.published(); n != 1 {
		t.Fatalf("after a failure publishing must pause, tried %d times", n)
	}
}
