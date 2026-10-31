package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"bookclub/internal/tg"
)

// fakeAnnouncer records posts; with fail set it answers like an unreachable Telegram.
type fakeAnnouncer struct {
	mu    sync.Mutex
	posts []tg.Message
	fail  bool
}

func (f *fakeAnnouncer) Announce(_ context.Context, m tg.Message) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.posts = append(f.posts, m)
	if f.fail {
		return errors.New("telegram is down")
	}
	return nil
}

func (f *fakeAnnouncer) take() []tg.Message {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := f.posts
	f.posts = nil
	return out
}

func TestAnnouncements(t *testing.T) {
	h := newHarness(t)

	var book bookDTO
	h.do(olga, "POST", "/api/books", `{"title":"Обломов","author":"Гончаров","note":"Про диван"}`, &book)
	posts := h.posts.take()
	if len(posts) != 1 {
		t.Fatalf("new book: want 1 post, got %d", len(posts))
	}
	want := "📚 Новая книга\n«Обломов» — Гончаров\nПредлагает: Ольга\n\nПро диван"
	if posts[0].Text != want {
		t.Fatalf("book post:\n%q\nwant\n%q", posts[0].Text, want)
	}
	if posts[0].ButtonURL != "https://t.me/club_bot/club?startapp=book_"+book.ID || posts[0].ButtonText == "" {
		t.Fatalf("button: %+v", posts[0])
	}

	// A new review is posted together with its full text.
	h.do(anna, "PUT", "/api/books/"+book.ID+"/review", `{"rating":4,"text":"Неспешно,\nно здорово"}`, nil)
	posts = h.posts.take()
	if len(posts) != 1 {
		t.Fatalf("new review: want 1 post, got %d", len(posts))
	}
	want = "✍️ Отзыв на «Обломов» — Гончаров\nАнна: ★★★★☆ 4 из 5\n\nНеспешно,\nно здорово"
	if posts[0].Text != want {
		t.Fatalf("review post:\n%q\nwant\n%q", posts[0].Text, want)
	}

	// Editing the review is not posted again.
	h.do(anna, "PUT", "/api/books/"+book.ID+"/review", `{"rating":5,"text":"Передумала: шедевр"}`, nil)
	if posts := h.posts.take(); len(posts) != 0 {
		t.Fatalf("edited review must not be posted: %+v", posts)
	}

	// A rating without text still gets a post.
	h.do(olga, "PUT", "/api/books/"+book.ID+"/review", `{"rating":3}`, nil)
	posts = h.posts.take()
	if len(posts) != 1 || posts[0].Text != "✍️ Отзыв на «Обломов» — Гончаров\nОльга: ★★★☆☆ 3 из 5" {
		t.Fatalf("rating-only review: %+v", posts)
	}

	// Rejected input is not posted.
	h.do(olga, "POST", "/api/books", `{"title":""}`, nil)
	h.do(olga, "PUT", "/api/books/"+book.ID+"/review", `{"rating":9}`, nil)
	h.do(olga, "PUT", "/api/books/nope/review", `{"rating":5}`, nil)
	if posts := h.posts.take(); len(posts) != 0 {
		t.Fatalf("failed requests must not be posted: %+v", posts)
	}
}

func TestAnnouncementFailureKeepsTheChange(t *testing.T) {
	h := newHarness(t)
	h.posts.fail = true

	var book bookDTO
	if code := h.do(olga, "POST", "/api/books", `{"title":"Обломов"}`, &book); code != http.StatusCreated {
		t.Fatalf("book must be saved even if Telegram is down: %d", code)
	}
	if code := h.do(anna, "PUT", "/api/books/"+book.ID+"/review", `{"rating":5}`, nil); code != http.StatusOK {
		t.Fatalf("review must be saved even if Telegram is down: %d", code)
	}
	if code := h.do(anna, "GET", "/api/books/"+book.ID, "", nil); code != http.StatusOK {
		t.Fatalf("book lost: %d", code)
	}
}

func TestNoButtonWithoutAppURL(t *testing.T) {
	s := New(Config{Logger: discardLogger()})
	m := s.withOpenButton("x", "ab")
	if m.ButtonURL != "" || m.ButtonText != "" {
		t.Fatalf("no button expected: %+v", m)
	}
}

func TestFitText(t *testing.T) {
	if got := fitText("коротко", 100); got != "коротко" {
		t.Fatalf("short text changed: %q", got)
	}

	long := strings.Repeat("я", 5000)
	got := fitText(long, tgTextLimit)
	if utf16Len(got) > tgTextLimit || !strings.HasSuffix(got, "…") || !utf8.ValidString(got) {
		t.Fatalf("len %d, suffix ok %v", utf16Len(got), strings.HasSuffix(got, "…"))
	}

	// Emoji take two UTF-16 units: the result must still fit and never split one in half.
	emoji := strings.Repeat("📚", 3000)
	got = fitText(emoji, tgTextLimit)
	if utf16Len(got) > tgTextLimit || !utf8.ValidString(got) {
		t.Fatalf("emoji: len %d valid %v", utf16Len(got), utf8.ValidString(got))
	}
}

func TestStarLine(t *testing.T) {
	for rating, want := range map[int]string{1: "★☆☆☆☆ 1 из 5", 5: "★★★★★ 5 из 5"} {
		if got := starLine(rating); got != want {
			t.Errorf("%d: %q, want %q", rating, got, want)
		}
	}
}
