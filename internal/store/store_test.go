package store

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

var (
	anna = Person{ID: 1, Name: "Анна"}
	olga = Person{ID: 2, Name: "Ольга"}
)

func openTemp(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "data.json")
	s, err := OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	clock := time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)
	s.now = func() time.Time { clock = clock.Add(time.Minute); return clock }
	return s, path
}

func must[T any](t *testing.T) func(T, error) T {
	return func(v T, err error) T {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
}

func TestBooksAndReviews(t *testing.T) {
	s, path := openTemp(t)

	first := must[Book](t)(s.AddBook(Book{Title: "Мастер и Маргарита", AddedBy: anna}))
	second := must[Book](t)(s.AddBook(Book{Title: "Обломов", AddedBy: olga}))

	if got := s.Books(); len(got) != 2 || got[0].ID != second.ID {
		t.Fatalf("want newest first, got %+v", got)
	}

	r1 := must[Review](t)(s.UpsertReview(first.ID, anna, 4, "хорошо"))
	r2 := must[Review](t)(s.UpsertReview(first.ID, anna, 5, "перечитала — отлично"))
	if r1.ID != r2.ID || r2.Rating != 5 || !r2.UpdatedAt.After(r1.CreatedAt) {
		t.Fatalf("upsert must update the same review: %+v -> %+v", r1, r2)
	}
	must[Review](t)(s.UpsertReview(first.ID, olga, 3, ""))
	if n := len(s.Reviews(first.ID)); n != 2 {
		t.Fatalf("want 2 reviews, got %d", n)
	}

	if _, err := s.UpsertReview("nope", anna, 5, ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}

	// Reopen: everything must survive a restart.
	reopened, err := OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(reopened.Books()) != 2 || len(reopened.AllReviews()) != 2 {
		t.Fatalf("data lost after reopen: %d books, %d reviews", len(reopened.Books()), len(reopened.AllReviews()))
	}
}

func TestDeleteBookCascades(t *testing.T) {
	s, _ := openTemp(t)
	b := must[Book](t)(s.AddBook(Book{Title: "Война и мир", AddedBy: anna}))
	must[Review](t)(s.UpsertReview(b.ID, olga, 5, ""))
	m := must[Meeting](t)(s.AddMeeting(Meeting{StartsAt: time.Now(), BookID: b.ID, CreatedBy: anna}))

	if err := s.DeleteBook(b.ID); err != nil {
		t.Fatal(err)
	}
	if len(s.AllReviews()) != 0 {
		t.Fatal("reviews of a deleted book must go too")
	}
	if got := must[Meeting](t)(s.Meeting(m.ID)); got.BookID != "" {
		t.Fatal("meeting must keep existing but lose the book link")
	}
	if err := s.DeleteBook(b.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestMeetings(t *testing.T) {
	s, _ := openTemp(t)
	late := must[Meeting](t)(s.AddMeeting(Meeting{StartsAt: time.Date(2026, 12, 1, 18, 0, 0, 0, time.UTC), CreatedBy: anna}))
	early := must[Meeting](t)(s.AddMeeting(Meeting{StartsAt: time.Date(2026, 11, 1, 18, 0, 0, 0, time.UTC), CreatedBy: anna}))

	if got := s.Meetings(); got[0].ID != early.ID || got[1].ID != late.ID {
		t.Fatalf("want chronological order, got %+v", got)
	}

	if _, err := s.AddMeeting(Meeting{BookID: "missing"}); !errors.Is(err, ErrUnknownBook) {
		t.Fatalf("want ErrUnknownBook, got %v", err)
	}

	upd := must[Meeting](t)(s.UpdateMeeting(Meeting{ID: late.ID, StartsAt: late.StartsAt, Place: "Кофейня", CreatedBy: olga}))
	if upd.Place != "Кофейня" || upd.CreatedBy != anna || !upd.CreatedAt.Equal(late.CreatedAt) {
		t.Fatalf("update must keep author and creation time: %+v", upd)
	}
	if _, err := s.UpdateMeeting(Meeting{ID: "missing"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestFailedWriteLeavesStateUntouched(t *testing.T) {
	s, path := openTemp(t)
	must[Book](t)(s.AddBook(Book{Title: "Первая", AddedBy: anna}))

	if os.Getuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	// Make the directory read-only so the temp file can't be created.
	dir := filepath.Dir(path)
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })

	if _, err := s.AddBook(Book{Title: "Вторая", AddedBy: anna}); err == nil {
		t.Fatal("want write error")
	}
	if n := len(s.Books()); n != 1 {
		t.Fatalf("memory must roll back on failed write, have %d books", n)
	}
}

func TestOpenRejectsNewerVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data.json")
	if err := os.WriteFile(path, []byte(`{"version": 99}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenFile(path); err == nil {
		t.Fatal("want error for unsupported version")
	}
}

func TestSetCover(t *testing.T) {
	s, path := openTemp(t)
	b := must[Book](t)(s.AddBook(Book{Title: "Обломов"}))

	if old := must[string](t)(s.SetCover(b.ID, "a.jpg")); old != "" {
		t.Fatalf("old = %q", old)
	}
	if old := must[string](t)(s.SetCover(b.ID, "b.jpg")); old != "a.jpg" {
		t.Fatalf("old = %q", old)
	}
	if _, err := s.SetCover("missing", "c.jpg"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("got %v", err)
	}

	reopened, err := OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := must[Book](t)(reopened.Book(b.ID)); got.Cover != "b.jpg" {
		t.Fatalf("cover after reopen: %q", got.Cover)
	}
}

func TestSetReviewPhoto(t *testing.T) {
	s, path := openTemp(t)
	b := must[Book](t)(s.AddBook(Book{Title: "Обломов", AddedBy: anna}))
	r := must[Review](t)(s.UpsertReview(b.ID, olga, 4, "Хорошо"))

	if old := must[string](t)(s.SetReviewPhoto(r.ID, "a.jpg")); old != "" {
		t.Fatalf("first photo: old = %q", old)
	}
	got := must[Review](t)(s.Review(r.ID))
	if got.Photo != "a.jpg" || !got.UpdatedAt.Equal(r.UpdatedAt) {
		t.Fatalf("a photo must not count as an edit: %+v", got)
	}

	// Editing the text keeps the photo.
	edited := must[Review](t)(s.UpsertReview(b.ID, olga, 5, "Отлично"))
	if edited.Photo != "a.jpg" {
		t.Fatalf("photo lost on edit: %+v", edited)
	}

	if old := must[string](t)(s.SetReviewPhoto(r.ID, "b.jpg")); old != "a.jpg" {
		t.Fatalf("replace: old = %q", old)
	}
	reopened, err := OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if p := must[Review](t)(reopened.Review(r.ID)).Photo; p != "b.jpg" {
		t.Fatalf("after reopen: %q", p)
	}
	if _, err := s.SetReviewPhoto("nope", "c.jpg"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown review: %v", err)
	}
}

func TestSetPhotoLink(t *testing.T) {
	s, _ := openTemp(t)
	b := must[Book](t)(s.AddBook(Book{Title: "Обломов", AddedBy: anna}))
	r := must[Review](t)(s.UpsertReview(b.ID, olga, 4, ""))
	must[string](t)(s.SetCover(b.ID, "cover.jpg"))
	must[string](t)(s.SetReviewPhoto(r.ID, "photo.jpg"))

	if err := s.SetPhotoLink("cover.jpg", "https://yadi.sk/i/c"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetPhotoLink("photo.jpg", "https://yadi.sk/i/p"); err != nil {
		t.Fatal(err)
	}
	if got := must[Book](t)(s.Book(b.ID)).CoverLink; got != "https://yadi.sk/i/c" {
		t.Fatalf("cover link: %q", got)
	}
	if got := must[Review](t)(s.Review(r.ID)).PhotoLink; got != "https://yadi.sk/i/p" {
		t.Fatalf("photo link: %q", got)
	}

	// A new photo forgets the old link: it pointed at the old file.
	must[string](t)(s.SetCover(b.ID, "cover2.jpg"))
	must[string](t)(s.SetReviewPhoto(r.ID, ""))
	if got := must[Book](t)(s.Book(b.ID)).CoverLink; got != "" {
		t.Fatalf("stale cover link: %q", got)
	}
	if got := must[Review](t)(s.Review(r.ID)).PhotoLink; got != "" {
		t.Fatalf("stale photo link: %q", got)
	}
	if err := s.SetPhotoLink("cover.jpg", "x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("replaced file: %v", err)
	}
}
