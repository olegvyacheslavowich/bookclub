// Package store keeps all club data as one JSON document.
//
// A book club produces a few hundred records per year, so the whole dataset lives in memory and
// every change writes the document back as a whole. Where it is written is up to the backend:
//
//   - a local file (OpenFile): for a VPS and for development;
//   - an Object Storage bucket (OpenBucket): for Yandex Serverless Containers, where the local
//     disk disappears with the instance and several instances may run at once.
//
// Concurrency between instances is optimistic: each save says which version it was based on
// (If-Match on the object's ETag). If another instance saved first, the store reloads the latest
// document, re-applies the change and tries again, so no member's review is silently lost.
package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"slices"
	"sync"
	"time"
)

var (
	ErrNotFound    = errors.New("not found")
	ErrUnknownBook = errors.New("unknown book")

	// errConflict: the stored document changed since we loaded it.
	errConflict = errors.New("store: document changed concurrently")
	// errNotModified: a conditional load found the document unchanged.
	errNotModified = errors.New("store: not modified")
)

const (
	currentVersion  = 1
	maxSaveAttempts = 5
	ioTimeout       = 15 * time.Second
)

type Person struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

type Book struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	Author    string    `json:"author,omitempty"`
	Link      string    `json:"link,omitempty"`
	Note      string    `json:"note,omitempty"`
	AddedBy   Person    `json:"addedBy"`
	CreatedAt time.Time `json:"createdAt"`
}

// Review: one per (book, author); posting again updates it.
type Review struct {
	ID        string    `json:"id"`
	BookID    string    `json:"bookId"`
	Author    Person    `json:"author"`
	Rating    int       `json:"rating"`
	Text      string    `json:"text,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

type Meeting struct {
	ID        string    `json:"id"`
	StartsAt  time.Time `json:"startsAt"`
	Place     string    `json:"place,omitempty"`
	BookID    string    `json:"bookId,omitempty"`
	Note      string    `json:"note,omitempty"`
	CreatedBy Person    `json:"createdBy"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

type snapshot struct {
	Version  int       `json:"version"`
	Books    []Book    `json:"books"`
	Reviews  []Review  `json:"reviews"`
	Meetings []Meeting `json:"meetings"`
}

// clone is a full copy: the record structs hold only values and strings.
func (s snapshot) clone() snapshot {
	return snapshot{
		Version:  s.Version,
		Books:    slices.Clone(s.Books),
		Reviews:  slices.Clone(s.Reviews),
		Meetings: slices.Clone(s.Meetings),
	}
}

// backend persists the encoded document. version is an opaque token (an ETag for a bucket).
type backend interface {
	// load returns the document and its version. With ifNoneMatch set it may return
	// errNotModified. A document that does not exist yet is fs.ErrNotExist.
	load(ctx context.Context, ifNoneMatch string) (data []byte, version string, err error)
	// save writes data if the stored version is still ifMatch ("" = must not exist yet),
	// otherwise returns errConflict.
	save(ctx context.Context, data []byte, ifMatch string) (version string, err error)
	// shared reports whether other processes may write the same document.
	shared() bool
	// backup stores a copy of data under name if no copy with that name exists yet.
	backup(ctx context.Context, name string, data []byte) error
}

type Store struct {
	be  backend
	log *slog.Logger
	now func() time.Time
	// refreshEvery: for a shared backend, how often reads check for changes made by others.
	refreshEvery time.Duration

	mu        sync.Mutex
	data      snapshot
	version   string
	checkedAt time.Time
	backupDay string // date of the last daily backup made by this process
}

func open(be backend, log *slog.Logger) (*Store, error) {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	s := &Store{be: be, log: log, now: time.Now, refreshEvery: 3 * time.Second, data: emptySnapshot()}
	ctx, cancel := context.WithTimeout(context.Background(), ioTimeout)
	defer cancel()
	if err := s.reload(ctx, false); err != nil {
		return nil, err
	}
	return s, nil
}

func emptySnapshot() snapshot { return snapshot{Version: currentVersion} }

// reload replaces the in-memory data with the stored document. Caller holds s.mu (or owns s).
func (s *Store) reload(ctx context.Context, conditional bool) error {
	ifNoneMatch := ""
	if conditional {
		ifNoneMatch = s.version
	}
	raw, version, err := s.be.load(ctx, ifNoneMatch)
	switch {
	case errors.Is(err, errNotModified):
		s.checkedAt = s.now()
		return nil
	case errors.Is(err, fs.ErrNotExist):
		s.data, s.version, s.checkedAt = emptySnapshot(), "", s.now()
		return nil
	case err != nil:
		return err
	}

	var d snapshot
	if err := json.Unmarshal(raw, &d); err != nil {
		return fmt.Errorf("store: parse: %w", err)
	}
	if d.Version > currentVersion {
		return fmt.Errorf("store: data version %d is newer than this build supports (%d)", d.Version, currentVersion)
	}
	d.Version = currentVersion
	s.data, s.version, s.checkedAt = d, version, s.now()
	return nil
}

// current returns the latest data. For a shared backend it first checks, at most every
// refreshEvery, whether another instance changed the document.
func (s *Store) current() snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.be.shared() && s.now().Sub(s.checkedAt) >= s.refreshEvery {
		ctx, cancel := context.WithTimeout(context.Background(), ioTimeout)
		defer cancel()
		if err := s.reload(ctx, true); err != nil {
			// Showing data a few seconds old beats showing an error page.
			s.log.Warn("store: refresh failed, serving cached data", "err", err)
			s.checkedAt = s.now()
		}
	}
	// Safe to share: mutate never changes these slices in place, it swaps in new ones.
	return s.data
}

// mutate applies fn to a copy of the data and saves it. A failed fn or a failed save leaves the
// store as it was. If another instance saved first, fn is re-applied to the fresh data.
func (s *Store) mutate(fn func(d *snapshot) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), ioTimeout)
	defer cancel()

	for attempt := 1; ; attempt++ {
		next := s.data.clone()
		if err := fn(&next); err != nil {
			return err
		}
		raw, err := json.MarshalIndent(next, "", "  ")
		if err != nil {
			return fmt.Errorf("store: encode: %w", err)
		}

		version, err := s.be.save(ctx, raw, s.version)
		if errors.Is(err, errConflict) && attempt < maxSaveAttempts {
			if err := s.reload(ctx, false); err != nil {
				return err
			}
			continue
		}
		if err != nil {
			return err
		}

		prev := s.data
		s.data, s.version, s.checkedAt = next, version, s.now()
		s.dailyBackup(ctx, prev)
		return nil
	}
}

// dailyBackup keeps the state as it was before the first change of each day (UTC).
// Failures are logged and never fail the user's change.
func (s *Store) dailyBackup(ctx context.Context, before snapshot) {
	day := s.now().UTC().Format(time.DateOnly)
	if s.backupDay == day {
		return
	}
	raw, err := json.MarshalIndent(before, "", "  ")
	if err == nil {
		err = s.be.backup(ctx, "backups/"+day+".json", raw)
	}
	if err != nil {
		s.log.Warn("store: daily backup failed", "day", day, "err", err)
		return
	}
	s.backupDay = day
}

// ---- books ----

// Books returns all books, newest first.
func (s *Store) Books() []Book {
	out := slices.Clone(s.current().Books)
	slices.SortStableFunc(out, func(a, b Book) int { return b.CreatedAt.Compare(a.CreatedAt) })
	return out
}

func (s *Store) Book(id string) (Book, error) {
	books := s.current().Books
	i := slices.IndexFunc(books, func(b Book) bool { return b.ID == id })
	if i < 0 {
		return Book{}, ErrNotFound
	}
	return books[i], nil
}

// AddBook assigns ID and CreatedAt and saves the book.
func (s *Store) AddBook(b Book) (Book, error) {
	b.ID = newID()
	b.CreatedAt = s.now().UTC()
	err := s.mutate(func(d *snapshot) error {
		d.Books = append(d.Books, b)
		return nil
	})
	return b, err
}

// DeleteBook removes the book with its reviews and unlinks it from meetings.
func (s *Store) DeleteBook(id string) error {
	return s.mutate(func(d *snapshot) error {
		i := slices.IndexFunc(d.Books, func(b Book) bool { return b.ID == id })
		if i < 0 {
			return ErrNotFound
		}
		d.Books = slices.Delete(d.Books, i, i+1)
		d.Reviews = slices.DeleteFunc(d.Reviews, func(r Review) bool { return r.BookID == id })
		for j := range d.Meetings {
			if d.Meetings[j].BookID == id {
				d.Meetings[j].BookID = ""
			}
		}
		return nil
	})
}

// ---- reviews ----

func (s *Store) AllReviews() []Review {
	return slices.Clone(s.current().Reviews)
}

// Reviews returns reviews of one book, most recently updated first.
func (s *Store) Reviews(bookID string) []Review {
	var out []Review
	for _, r := range s.current().Reviews {
		if r.BookID == bookID {
			out = append(out, r)
		}
	}
	slices.SortStableFunc(out, func(a, b Review) int { return b.UpdatedAt.Compare(a.UpdatedAt) })
	return out
}

func (s *Store) Review(id string) (Review, error) {
	reviews := s.current().Reviews
	i := slices.IndexFunc(reviews, func(r Review) bool { return r.ID == id })
	if i < 0 {
		return Review{}, ErrNotFound
	}
	return reviews[i], nil
}

// UpsertReview creates the author's review of a book or updates the existing one.
// Returns ErrNotFound if the book does not exist.
func (s *Store) UpsertReview(bookID string, author Person, rating int, text string) (Review, error) {
	var out Review
	err := s.mutate(func(d *snapshot) error {
		if !slices.ContainsFunc(d.Books, func(b Book) bool { return b.ID == bookID }) {
			return ErrNotFound
		}
		now := s.now().UTC()
		i := slices.IndexFunc(d.Reviews, func(r Review) bool { return r.BookID == bookID && r.Author.ID == author.ID })
		if i >= 0 {
			r := &d.Reviews[i]
			r.Rating, r.Text, r.Author.Name, r.UpdatedAt = rating, text, author.Name, now
			out = *r
			return nil
		}
		out = Review{ID: newID(), BookID: bookID, Author: author, Rating: rating, Text: text, CreatedAt: now, UpdatedAt: now}
		d.Reviews = append(d.Reviews, out)
		return nil
	})
	return out, err
}

func (s *Store) DeleteReview(id string) error {
	return s.mutate(func(d *snapshot) error {
		i := slices.IndexFunc(d.Reviews, func(r Review) bool { return r.ID == id })
		if i < 0 {
			return ErrNotFound
		}
		d.Reviews = slices.Delete(d.Reviews, i, i+1)
		return nil
	})
}

// ---- meetings ----

// Meetings returns all meetings in chronological order.
func (s *Store) Meetings() []Meeting {
	out := slices.Clone(s.current().Meetings)
	slices.SortStableFunc(out, func(a, b Meeting) int { return a.StartsAt.Compare(b.StartsAt) })
	return out
}

func (s *Store) Meeting(id string) (Meeting, error) {
	meetings := s.current().Meetings
	i := slices.IndexFunc(meetings, func(m Meeting) bool { return m.ID == id })
	if i < 0 {
		return Meeting{}, ErrNotFound
	}
	return meetings[i], nil
}

// AddMeeting assigns ID and timestamps. Returns ErrUnknownBook if m.BookID is set but missing.
func (s *Store) AddMeeting(m Meeting) (Meeting, error) {
	now := s.now().UTC()
	m.ID, m.CreatedAt, m.UpdatedAt = newID(), now, now
	err := s.mutate(func(d *snapshot) error {
		if err := checkBook(d, m.BookID); err != nil {
			return err
		}
		d.Meetings = append(d.Meetings, m)
		return nil
	})
	return m, err
}

// UpdateMeeting replaces the editable fields of meeting m.ID; author and creation time are kept.
func (s *Store) UpdateMeeting(m Meeting) (Meeting, error) {
	var out Meeting
	err := s.mutate(func(d *snapshot) error {
		i := slices.IndexFunc(d.Meetings, func(x Meeting) bool { return x.ID == m.ID })
		if i < 0 {
			return ErrNotFound
		}
		if err := checkBook(d, m.BookID); err != nil {
			return err
		}
		cur := &d.Meetings[i]
		cur.StartsAt, cur.Place, cur.BookID, cur.Note = m.StartsAt, m.Place, m.BookID, m.Note
		cur.UpdatedAt = s.now().UTC()
		out = *cur
		return nil
	})
	return out, err
}

func (s *Store) DeleteMeeting(id string) error {
	return s.mutate(func(d *snapshot) error {
		i := slices.IndexFunc(d.Meetings, func(m Meeting) bool { return m.ID == id })
		if i < 0 {
			return ErrNotFound
		}
		d.Meetings = slices.Delete(d.Meetings, i, i+1)
		return nil
	})
}

func checkBook(d *snapshot, bookID string) error {
	if bookID != "" && !slices.ContainsFunc(d.Books, func(b Book) bool { return b.ID == bookID }) {
		return ErrUnknownBook
	}
	return nil
}

func newID() string {
	var b [8]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read never fails since Go 1.24
	return hex.EncodeToString(b[:])
}
