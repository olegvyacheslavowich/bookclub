package api

import (
	"net/http"
	"strings"
	"testing"
)

func (h *coverHarness) myReview(who, bookID string) *reviewDTO {
	h.t.Helper()
	var d bookDetailDTO
	h.do(who, "GET", "/api/books/"+bookID, "", &d)
	return d.MyReview
}

func TestReviewPhotoFlow(t *testing.T) {
	h := newCoverHarness(t)
	var book bookDTO
	h.do(anna, "POST", "/api/books", `{"title":"Обломов"}`, &book)
	var rv reviewDTO
	h.do(olga, "PUT", "/api/books/"+book.ID+"/review", `{"rating":4,"text":"Хорошо"}`, &rv)
	path := "/api/reviews/" + rv.ID + "/photo"

	// Only the author adds the photo — not another member, not the administrator.
	for _, who := range []string{anna, nina} {
		if code := h.do(who, "PUT", path, jpegPhoto, nil); code != http.StatusForbidden {
			t.Fatalf("%s: %d", who, code)
		}
	}
	var up struct{ PhotoURL string }
	if code := h.do(olga, "PUT", path, jpegPhoto, &up); code != http.StatusOK {
		t.Fatalf("upload: %d", code)
	}
	if !strings.HasPrefix(up.PhotoURL, "api/reviews/"+rv.ID+"/photo?v=") {
		t.Fatalf("photoUrl: %q", up.PhotoURL)
	}

	// Everyone sees it; the review doesn't turn "edited".
	var d bookDetailDTO
	h.do(anna, "GET", "/api/books/"+book.ID, "", &d)
	if len(d.Reviews) != 1 || d.Reviews[0].PhotoURL != up.PhotoURL || !d.Reviews[0].UpdatedAt.Equal(rv.UpdatedAt) {
		t.Fatalf("Anna sees: %+v", d.Reviews)
	}
	if code, body, cache := h.get(anna, up.PhotoURL); code != http.StatusOK || body != jpegPhoto || !strings.Contains(cache, "private") {
		t.Fatalf("get: %d %q", code, cache)
	}

	// Editing the text keeps the photo; replacing it leaves one file.
	h.do(olga, "PUT", "/api/books/"+book.ID+"/review", `{"rating":5,"text":"Отлично"}`, nil)
	if got := h.myReview(olga, book.ID).PhotoURL; got != up.PhotoURL {
		t.Fatalf("photo after edit: %q", got)
	}
	h.do(olga, "PUT", path, pngPhoto, &up)
	if files := h.files(); len(files) != 1 || !strings.HasSuffix(files[0], ".png") {
		t.Fatalf("after replace: %v", files)
	}

	// Another member can't remove it; the administrator can.
	if code := h.do(anna, "DELETE", path, "", nil); code != http.StatusForbidden {
		t.Fatalf("Anna removes: %d", code)
	}
	if code := h.do(nina, "DELETE", path, "", nil); code != http.StatusNoContent {
		t.Fatalf("admin removes: %d", code)
	}
	if h.myReview(olga, book.ID).PhotoURL != "" || len(h.files()) != 0 {
		t.Fatalf("photo still there: %v", h.files())
	}

	// Deleting the review, or the whole book, deletes the photo.
	h.do(olga, "PUT", path, jpegPhoto, nil)
	h.do(olga, "DELETE", "/api/reviews/"+rv.ID, "", nil)
	if files := h.files(); len(files) != 0 {
		t.Fatalf("photo of a deleted review: %v", files)
	}
	h.do(olga, "PUT", "/api/books/"+book.ID+"/review", `{"rating":3}`, &rv)
	h.do(olga, "PUT", "/api/reviews/"+rv.ID+"/photo", jpegPhoto, nil)
	h.do(anna, "PUT", "/api/books/"+book.ID+"/cover", jpegPhoto, nil)
	if len(h.files()) != 2 {
		t.Fatalf("setup: %v", h.files())
	}
	h.do(anna, "DELETE", "/api/books/"+book.ID, "", nil)
	if files := h.files(); len(files) != 0 {
		t.Fatalf("photos of a deleted book: %v", files)
	}
}

func TestReviewPhotoRejectsBadUploads(t *testing.T) {
	h := newCoverHarness(t)
	var book bookDTO
	h.do(anna, "POST", "/api/books", `{"title":"Обломов"}`, &book)
	var rv reviewDTO
	h.do(olga, "PUT", "/api/books/"+book.ID+"/review", `{"rating":4}`, &rv)

	if code := h.do(olga, "PUT", "/api/reviews/"+rv.ID+"/photo", "GIF89a......", nil); code != http.StatusUnsupportedMediaType {
		t.Fatalf("gif: %d", code)
	}
	if code := h.do(olga, "PUT", "/api/reviews/ffff/photo", jpegPhoto, nil); code != http.StatusNotFound {
		t.Fatalf("unknown review: %d", code)
	}
	if len(h.files()) != 0 {
		t.Fatalf("a rejected upload left traces: %v", h.files())
	}
}
