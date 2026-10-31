package store

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeS3 implements the slice of the Object Storage API the bucket backend uses,
// including conditional GET/PUT, so concurrency handling can be tested without the cloud.
type fakeS3 struct {
	t         *testing.T
	token     string
	mu        sync.Mutex
	objects   map[string]fakeObject
	gets      atomic.Int32
	notMod    atomic.Int32
	puts      atomic.Int32
	failAll   int    // HTTP status to return for every request, 0 = behave
	beforePut func() // runs before each PUT is handled, e.g. to simulate a concurrent writer
}

type fakeObject struct {
	data []byte
	etag string
}

func newFakeS3(t *testing.T) (*fakeS3, *httptest.Server) {
	f := &fakeS3{t: t, token: "tok-secret", objects: map[string]fakeObject{}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, srv
}

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if f.failAll != 0 {
		w.WriteHeader(f.failAll)
		fmt.Fprint(w, `<?xml version="1.0"?><Error><Code>AccessDenied</Code><Message>no</Message></Error>`)
		return
	}
	if r.Header.Get("Authorization") != "Bearer "+f.token {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	bucket, key, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
	if bucket != "club-bucket" {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `<Error><Code>NoSuchBucket</Code></Error>`)
		return
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	obj, exists := f.objects[key]

	switch r.Method {
	case http.MethodGet:
		f.gets.Add(1)
		if !exists {
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `<Error><Code>NoSuchKey</Code></Error>`)
			return
		}
		if inm := r.Header.Get("If-None-Match"); inm != "" && inm == obj.etag {
			f.notMod.Add(1)
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", obj.etag)
		w.Write(obj.data)

	case http.MethodPut:
		f.puts.Add(1)
		if f.beforePut != nil {
			f.beforePut()
			obj, exists = f.objects[key]
		}
		if im := r.Header.Get("If-Match"); im != "" && (!exists || im != obj.etag) {
			w.WriteHeader(http.StatusPreconditionFailed)
			return
		}
		if r.Header.Get("If-None-Match") == "*" && exists {
			w.WriteHeader(http.StatusPreconditionFailed)
			return
		}
		data, _ := io.ReadAll(r.Body)
		sum := md5.Sum(data)
		etag := `"` + hex.EncodeToString(sum[:]) + `"`
		f.objects[key] = fakeObject{data: data, etag: etag}
		w.Header().Set("ETag", etag)

	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func openFakeBucket(t *testing.T, srv *httptest.Server, token string) *Store {
	t.Helper()
	s, err := OpenBucket(BucketConfig{Bucket: "club-bucket", Endpoint: srv.URL, Token: StaticToken(token)})
	if err != nil {
		t.Fatal(err)
	}
	s.refreshEvery = 0 // always check for other instances' writes
	return s
}

func TestBucketTwoInstancesDoNotLoseWrites(t *testing.T) {
	f, srv := newFakeS3(t)
	a := openFakeBucket(t, srv, f.token)
	b := openFakeBucket(t, srv, f.token)

	// Both instances start from the same (empty) state and write without seeing each other.
	book := must[Book](t)(a.AddBook(Book{Title: "Лавр", AddedBy: anna}))
	must[Book](t)(b.AddBook(Book{Title: "Обломов", AddedBy: olga}))

	// Two members review the same book through different instances, each with stale data.
	b.refreshEvery, a.refreshEvery = time.Hour, time.Hour
	must[Review](t)(a.UpsertReview(book.ID, anna, 5, ""))
	must[Review](t)(b.UpsertReview(book.ID, olga, 4, ""))

	a.refreshEvery = 0
	if got := len(a.Books()); got != 2 {
		t.Fatalf("instance A sees %d books, want 2", got)
	}
	if got := len(a.Reviews(book.ID)); got != 2 {
		t.Fatalf("instance A sees %d reviews, want 2", got)
	}
}

func TestBucketValidationUsesFreshData(t *testing.T) {
	f, srv := newFakeS3(t)
	a := openFakeBucket(t, srv, f.token)
	book := must[Book](t)(a.AddBook(Book{Title: "Лавр", AddedBy: anna}))

	b := openFakeBucket(t, srv, f.token)
	b.refreshEvery = time.Hour // b still believes the book exists
	if err := a.DeleteBook(book.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := b.UpsertReview(book.ID, olga, 5, ""); err != ErrNotFound {
		t.Fatalf("review of a book deleted by another instance: want ErrNotFound, got %v", err)
	}
}

func TestBucketSeesItemsJustAddedByAnotherInstance(t *testing.T) {
	f, srv := newFakeS3(t)
	a := openFakeBucket(t, srv, f.token)
	b := openFakeBucket(t, srv, f.token)
	b.refreshEvery = time.Hour // b has just checked and won't look again on its own

	// A member adds a book on instance A; the request for its page lands on B.
	book := must[Book](t)(a.AddBook(Book{Title: "Лавр", AddedBy: anna}))
	if _, err := b.Book(book.ID); err != nil {
		t.Fatalf("B doesn't see the book added through A: %v", err)
	}

	// The same for writes that refer to something B hasn't seen yet.
	book2 := must[Book](t)(a.AddBook(Book{Title: "Обломов", AddedBy: anna}))
	rv := must[Review](t)(b.UpsertReview(book2.ID, olga, 5, ""))
	must[Meeting](t)(b.AddMeeting(Meeting{StartsAt: time.Now(), BookID: book2.ID}))
	if _, err := a.Review(rv.ID); err != nil {
		t.Fatalf("A doesn't see the review written through B: %v", err)
	}

	// Something that really doesn't exist is still "not found".
	if _, err := b.Book("nope"); err != ErrNotFound {
		t.Fatalf("missing book: %v", err)
	}
	if _, err := b.UpsertReview("nope", olga, 5, ""); err != ErrNotFound {
		t.Fatalf("review of a missing book: %v", err)
	}
}

func TestBucketReadsUseConditionalGet(t *testing.T) {
	f, srv := newFakeS3(t)
	s := openFakeBucket(t, srv, f.token)
	must[Book](t)(s.AddBook(Book{Title: "Лавр", AddedBy: anna}))

	before := f.gets.Load()
	for range 3 {
		s.Books()
	}
	if got := f.notMod.Load(); got != 3 || f.gets.Load()-before != 3 {
		t.Fatalf("want 3 conditional GETs answered 304, got %d GETs / %d not-modified", f.gets.Load()-before, got)
	}

	s.refreshEvery = time.Hour
	s.Books()
	if f.gets.Load()-before != 3 {
		t.Fatal("reads within refreshEvery must not hit the bucket")
	}
}

func TestBucketDailyBackup(t *testing.T) {
	f, srv := newFakeS3(t)
	s := openFakeBucket(t, srv, f.token)
	day := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return day }

	must[Book](t)(s.AddBook(Book{Title: "Первая", AddedBy: anna}))
	must[Book](t)(s.AddBook(Book{Title: "Вторая", AddedBy: anna}))
	day = day.Add(24 * time.Hour)
	must[Book](t)(s.AddBook(Book{Title: "Третья", AddedBy: anna}))

	f.mu.Lock()
	defer f.mu.Unlock()
	first, ok1 := f.objects["backups/2026-11-01.json"]
	second, ok2 := f.objects["backups/2026-11-02.json"]
	if !ok1 || !ok2 {
		t.Fatalf("want one backup per day, have %v", keys(f.objects))
	}
	if strings.Contains(string(first.data), "Первая") {
		t.Fatal("a day's backup must hold the state before that day's first change")
	}
	if !strings.Contains(string(second.data), "Вторая") || strings.Contains(string(second.data), "Третья") {
		t.Fatalf("second backup has wrong content: %s", second.data)
	}
}

func TestBucketErrorsAreReadableAndHideToken(t *testing.T) {
	f, srv := newFakeS3(t)
	f.failAll = http.StatusForbidden
	_, err := OpenBucket(BucketConfig{Bucket: "club-bucket", Endpoint: srv.URL, Token: StaticToken(f.token)})
	if err == nil || !strings.Contains(err.Error(), "AccessDenied") {
		t.Fatalf("want AccessDenied in error, got %v", err)
	}
	if strings.Contains(err.Error(), f.token) {
		t.Fatal("error leaks the IAM token")
	}
}

func TestBucketGivesUpAfterRepeatedConflicts(t *testing.T) {
	f, srv := newFakeS3(t)
	s := openFakeBucket(t, srv, f.token)
	// Another writer changes the object right before every one of our saves (f.mu is held here).
	n := 0
	f.beforePut = func() {
		n++
		f.objects["bookclub.json"] = fakeObject{data: []byte(`{"version":1}`), etag: fmt.Sprintf(`"other-%d"`, n)}
	}
	if _, err := s.AddBook(Book{Title: "X", AddedBy: anna}); err == nil {
		t.Fatal("want an error after maxSaveAttempts conflicts")
	}
}

func TestMetadataToken(t *testing.T) {
	var calls atomic.Int32
	md := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Metadata-Flavor") != "Google" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		n := calls.Add(1)
		fmt.Fprintf(w, `{"access_token":"t%d","expires_in":3600,"token_type":"Bearer"}`, n)
	}))
	defer md.Close()

	now := time.Now()
	m := &MetadataToken{URL: md.URL, now: func() time.Time { return now }}
	ctx := context.Background()

	if tok, _ := m.Token(ctx); tok != "t1" {
		t.Fatalf("got %q", tok)
	}
	if tok, _ := m.Token(ctx); tok != "t1" || calls.Load() != 1 {
		t.Fatal("token must be cached")
	}
	now = now.Add(56 * time.Minute) // within 5 minutes of expiry
	if tok, _ := m.Token(ctx); tok != "t2" {
		t.Fatalf("token close to expiry must be refreshed, got %q", tok)
	}
	m.Invalidate()
	if tok, _ := m.Token(ctx); tok != "t3" {
		t.Fatalf("invalidated token must be refetched, got %q", tok)
	}
}

func TestBucketUnauthorizedInvalidatesToken(t *testing.T) {
	f, srv := newFakeS3(t)
	src := &countingToken{tokens: []string{"expired", f.token}}
	s, err := OpenBucket(BucketConfig{Bucket: "club-bucket", Endpoint: srv.URL, Token: src})
	if err == nil {
		t.Fatal("first request with an expired token must fail")
	}
	if src.invalidated != 1 {
		t.Fatalf("401 must invalidate the token, invalidated=%d", src.invalidated)
	}
	if s, err = OpenBucket(BucketConfig{Bucket: "club-bucket", Endpoint: srv.URL, Token: src}); err != nil || s == nil {
		t.Fatalf("retry with fresh token: %v", err)
	}
}

type countingToken struct {
	tokens      []string
	i           int
	invalidated int
}

func (c *countingToken) Token(context.Context) (string, error) { return c.tokens[c.i], nil }
func (c *countingToken) Invalidate() {
	c.invalidated++
	if c.i < len(c.tokens)-1 {
		c.i++
	}
}

func keys(m map[string]fakeObject) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestBucketFailedSaveLeavesStateUntouched(t *testing.T) {
	f, srv := newFakeS3(t)
	s := openFakeBucket(t, srv, f.token)
	must[Book](t)(s.AddBook(Book{Title: "Первая", AddedBy: anna}))

	s.refreshEvery = time.Hour
	f.failAll = http.StatusInternalServerError
	if _, err := s.AddBook(Book{Title: "Вторая", AddedBy: anna}); err == nil {
		t.Fatal("want save error")
	}
	if n := len(s.Books()); n != 1 {
		t.Fatalf("memory must roll back on failed save, have %d books", n)
	}
}
