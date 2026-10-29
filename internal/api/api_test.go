package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"bookclub/internal/auth"
	"bookclub/internal/store"
)

// fakeAuth treats initData as a key into a fixed set of users.
type fakeAuth map[string]auth.User

func (f fakeAuth) Validate(initData string) (auth.User, error) {
	if u, ok := f[initData]; ok {
		return u, nil
	}
	return auth.User{}, auth.ErrBadHash
}

type fakeMembers map[int64]bool

func (f fakeMembers) IsMember(_ context.Context, id int64) (bool, error) {
	if id == 500 {
		return false, errors.New("telegram is down")
	}
	return f[id], nil
}

const (
	anna     = "anna"
	olga     = "olga"
	stranger = "stranger"
	broken   = "broken"
)

type harness struct {
	t   *testing.T
	srv *httptest.Server
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	st, err := store.OpenFile(filepath.Join(t.TempDir(), "data.json"))
	if err != nil {
		t.Fatal(err)
	}
	s := New(Config{
		Store: st,
		Auth: fakeAuth{
			anna:     {ID: 1, FirstName: "Анна"},
			olga:     {ID: 2, FirstName: "Ольга"},
			stranger: {ID: 3, FirstName: "Чужой"},
			broken:   {ID: 500, FirstName: "Сбой"},
		},
		Members: fakeMembers{1: true, 2: true},
		Static:  fstest.MapFS{"index.html": {Data: []byte("<!doctype html>")}},
		Logger:  discardLogger(),
	})
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return &harness{t: t, srv: srv}
}

// do sends a request as `who` and decodes the JSON answer into out (if non-nil).
func (h *harness) do(who, method, path, body string, out any) int {
	h.t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, _ := http.NewRequest(method, h.srv.URL+path, rd)
	if who != "" {
		req.Header.Set(AuthHeader, "tma "+who)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			h.t.Fatalf("%s %s: decode: %v", method, path, err)
		}
	}
	return resp.StatusCode
}

func TestAccess(t *testing.T) {
	h := newHarness(t)
	cases := []struct {
		who  string
		want int
	}{
		{"", http.StatusUnauthorized},
		{"forged", http.StatusUnauthorized},
		{stranger, http.StatusForbidden},
		{broken, http.StatusServiceUnavailable},
		{olga, http.StatusOK},
	}
	for _, c := range cases {
		if got := h.do(c.who, "GET", "/api/me", "", nil); got != c.want {
			t.Errorf("%q: got %d, want %d", c.who, got, c.want)
		}
	}

	var me meDTO
	h.do(anna, "GET", "/api/me", "", &me)
	if me.ID != 1 || me.Name != "Анна" {
		t.Fatalf("got %+v", me)
	}

	if got := h.do("", "GET", "/", "", nil); got != http.StatusOK {
		t.Fatalf("static files must not require auth, got %d", got)
	}
}

func TestDevModeIsOffByDefault(t *testing.T) {
	h := newHarness(t)
	req, _ := http.NewRequest("GET", h.srv.URL+"/api/me", nil)
	req.Header.Set("Authorization", "dev 1:Анна")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("dev auth must be rejected without DevMode, got %d", resp.StatusCode)
	}
}

func TestDevModeAcceptsPercentEncodedUser(t *testing.T) {
	st, err := store.OpenFile(filepath.Join(t.TempDir(), "data.json"))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(New(Config{Store: st, Auth: fakeAuth{}, DevMode: true, Logger: discardLogger()}).Handler())
	defer srv.Close()

	req, _ := http.NewRequest("GET", srv.URL+"/api/me", nil)
	req.Header.Set("Authorization", "dev 2%3A%D0%9C%D0%B0%D1%88%D0%B0") // "2:Маша"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var me meDTO
	json.NewDecoder(resp.Body).Decode(&me)
	if resp.StatusCode != http.StatusOK || me.ID != 2 || me.Name != "Маша" {
		t.Fatalf("got %d %+v", resp.StatusCode, me)
	}
}

func TestBookFlow(t *testing.T) {
	h := newHarness(t)

	var errBody map[string]string
	if code := h.do(olga, "POST", "/api/books", `{"title":"   "}`, &errBody); code != http.StatusUnprocessableEntity || errBody["error"] == "" {
		t.Fatalf("empty title: %d %v", code, errBody)
	}
	if code := h.do(olga, "POST", "/api/books", `{"title":"X","link":"javascript:alert(1)"}`, nil); code != http.StatusUnprocessableEntity {
		t.Fatalf("javascript link must be rejected, got %d", code)
	}
	if code := h.do(olga, "POST", "/api/books", `{"title":"X","extra":1}`, nil); code != http.StatusBadRequest {
		t.Fatalf("unknown fields must be rejected, got %d", code)
	}

	var book bookDTO
	if code := h.do(olga, "POST", "/api/books", `{"title":" Обломов ","author":"Гончаров","link":"litres.ru/book/oblomov"}`, &book); code != http.StatusCreated {
		t.Fatalf("create: %d", code)
	}
	if book.Title != "Обломов" || book.Link != "https://litres.ru/book/oblomov" || book.AddedBy != "Ольга" || !book.CanDelete {
		t.Fatalf("got %+v", book)
	}

	if code := h.do(olga, "PUT", "/api/books/"+book.ID+"/review", `{"rating":6}`, nil); code != http.StatusUnprocessableEntity {
		t.Fatalf("rating 6: %d", code)
	}
	h.do(olga, "PUT", "/api/books/"+book.ID+"/review", `{"rating":4,"text":"Неспешно, но здорово"}`, nil)
	var annaReview reviewDTO
	h.do(anna, "PUT", "/api/books/"+book.ID+"/review", `{"rating":5}`, &annaReview)

	var list struct{ Books []bookDTO }
	h.do(olga, "GET", "/api/books", "", &list)
	if len(list.Books) != 1 || list.Books[0].Rating != (ratingDTO{Average: 4.5, Count: 2}) || list.Books[0].MyRating != 4 {
		t.Fatalf("list: %+v", list.Books)
	}

	var detail bookDetailDTO
	h.do(olga, "GET", "/api/books/"+book.ID, "", &detail)
	if detail.MyReview == nil || detail.MyReview.Rating != 4 || len(detail.Reviews) != 1 || detail.Reviews[0].CanDelete {
		t.Fatalf("detail: %+v", detail)
	}

	// Nobody can delete someone else's review; everyone can delete their own.
	if code := h.do(olga, "DELETE", "/api/reviews/"+annaReview.ID, "", nil); code != http.StatusForbidden {
		t.Fatalf("Olga deleting Anna's review: %d", code)
	}
	if code := h.do(anna, "DELETE", "/api/reviews/"+detail.MyReview.ID, "", nil); code != http.StatusForbidden {
		t.Fatalf("Anna deleting Olga's review: %d", code)
	}
	if code := h.do(olga, "DELETE", "/api/reviews/"+detail.MyReview.ID, "", nil); code != http.StatusNoContent {
		t.Fatalf("Olga deleting her own review: %d", code)
	}

	// Only the person who added the book may delete it.
	var annaBook bookDTO
	h.do(anna, "POST", "/api/books", `{"title":"Анна Каренина"}`, &annaBook)
	if code := h.do(olga, "DELETE", "/api/books/"+annaBook.ID, "", nil); code != http.StatusForbidden {
		t.Fatalf("Olga deleting Anna's book: %d", code)
	}
	if code := h.do(anna, "DELETE", "/api/books/"+book.ID, "", nil); code != http.StatusForbidden {
		t.Fatalf("Anna deleting Olga's book: %d", code)
	}
	if code := h.do(olga, "DELETE", "/api/books/"+book.ID, "", nil); code != http.StatusNoContent {
		t.Fatalf("member deleting own book: %d", code)
	}
	if code := h.do(olga, "GET", "/api/books/"+book.ID, "", nil); code != http.StatusNotFound {
		t.Fatalf("deleted book: %d", code)
	}
}

func TestMeetingFlow(t *testing.T) {
	h := newHarness(t)
	var book bookDTO
	h.do(anna, "POST", "/api/books", `{"title":"Мы","author":"Замятин"}`, &book)

	body := `{"startsAt":"2026-11-15T18:00:00+05:00","place":"Кофейня на Ленина","bookId":"` + book.ID + `"}`
	if code := h.do(anna, "POST", "/api/meetings", `{"startsAt":"завтра"}`, nil); code != http.StatusUnprocessableEntity {
		t.Fatalf("bad date: %d", code)
	}
	if code := h.do(anna, "POST", "/api/meetings", `{"startsAt":"2026-11-15T18:00:00Z","bookId":"nope"}`, nil); code != http.StatusUnprocessableEntity {
		t.Fatalf("unknown book: %d", code)
	}

	var m meetingDTO
	if code := h.do(anna, "POST", "/api/meetings", body, &m); code != http.StatusCreated {
		t.Fatalf("create: %d", code)
	}
	if m.Book == nil || m.Book.Title != "Мы" || m.StartsAt.UTC().Hour() != 13 {
		t.Fatalf("got %+v", m)
	}

	// Any member can change a meeting someone else scheduled.
	var upd meetingDTO
	h.do(olga, "PUT", "/api/meetings/"+m.ID, `{"startsAt":"2026-11-16T18:00:00+05:00","place":"Онлайн"}`, &upd)
	if upd.Place != "Онлайн" || upd.Book != nil {
		t.Fatalf("update: %+v", upd)
	}

	var list struct{ Meetings []meetingDTO }
	h.do(olga, "GET", "/api/meetings", "", &list)
	if len(list.Meetings) != 1 || list.Meetings[0].Place != "Онлайн" {
		t.Fatalf("list: %+v", list.Meetings)
	}

	if code := h.do(anna, "DELETE", "/api/meetings/"+m.ID, "", nil); code != http.StatusNoContent {
		t.Fatalf("delete: %d", code)
	}
	if code := h.do(anna, "DELETE", "/api/meetings/"+m.ID, "", nil); code != http.StatusNotFound {
		t.Fatalf("second delete: %d", code)
	}
}

func TestLink(t *testing.T) {
	ok := map[string]string{
		"":                               "",
		"https://www.litres.ru/book/123": "https://www.litres.ru/book/123",
		"litres.ru/book":                 "https://litres.ru/book",
		"http://lib.ru/x.txt":            "http://lib.ru/x.txt",
	}
	for in, want := range ok {
		if got, err := link(in); err != nil || got != want {
			t.Errorf("link(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"javascript:alert(1)", "ftp://x.ru/a", "читать тут https://x.ru", "localhost:8080", "https://"} {
		if _, err := link(in); err == nil {
			t.Errorf("link(%q) must fail", in)
		}
	}
}

func TestAuthorizationHeaderFallback(t *testing.T) {
	h := newHarness(t)
	req, _ := http.NewRequest("GET", h.srv.URL+"/api/me", nil)
	req.Header.Set("Authorization", "tma "+olga)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("Authorization header must still work, got %d", resp.StatusCode)
	}
}
