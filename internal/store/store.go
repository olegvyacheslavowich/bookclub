// Package store keeps club data in a single JSON file.
//
// A book club produces a few hundred records per year, so the whole dataset lives in memory
// and every change rewrites the file atomically (temp file + fsync + rename). If the app ever
// outgrows this, swap FileStore for a SQLite implementation of api.Store; nothing else changes.
package store

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"
)

var (
	ErrNotFound    = errors.New("not found")
	ErrUnknownBook = errors.New("unknown book")
)

const currentVersion = 1

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

type FileStore struct {
	path string
	now  func() time.Time

	mu   sync.RWMutex
	data snapshot
}

// OpenFile loads path, or starts empty if it does not exist yet. The directory must exist.
func OpenFile(path string) (*FileStore, error) {
	s := &FileStore{path: path, now: time.Now, data: snapshot{Version: currentVersion}}

	raw, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return s, nil
	case err != nil:
		return nil, fmt.Errorf("store: read %s: %w", path, err)
	}
	if err := json.Unmarshal(raw, &s.data); err != nil {
		return nil, fmt.Errorf("store: parse %s: %w", path, err)
	}
	if s.data.Version > currentVersion {
		return nil, fmt.Errorf("store: %s has data version %d, this build supports %d", path, s.data.Version, currentVersion)
	}
	s.data.Version = currentVersion
	return s, nil
}

// ---- books ----

// Books returns all books, newest first.
func (s *FileStore) Books() []Book {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := slices.Clone(s.data.Books)
	slices.SortStableFunc(out, func(a, b Book) int { return b.CreatedAt.Compare(a.CreatedAt) })
	return out
}

func (s *FileStore) Book(id string) (Book, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	i := slices.IndexFunc(s.data.Books, func(b Book) bool { return b.ID == id })
	if i < 0 {
		return Book{}, ErrNotFound
	}
	return s.data.Books[i], nil
}

// AddBook assigns ID and CreatedAt and saves the book.
func (s *FileStore) AddBook(b Book) (Book, error) {
	b.ID = newID()
	b.CreatedAt = s.now().UTC()
	err := s.mutate(func(d *snapshot) error {
		d.Books = append(d.Books, b)
		return nil
	})
	return b, err
}

// DeleteBook removes the book with its reviews and unlinks it from meetings.
func (s *FileStore) DeleteBook(id string) error {
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

func (s *FileStore) AllReviews() []Review {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return slices.Clone(s.data.Reviews)
}

// Reviews returns reviews of one book, most recently updated first.
func (s *FileStore) Reviews(bookID string) []Review {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []Review
	for _, r := range s.data.Reviews {
		if r.BookID == bookID {
			out = append(out, r)
		}
	}
	slices.SortStableFunc(out, func(a, b Review) int { return b.UpdatedAt.Compare(a.UpdatedAt) })
	return out
}

func (s *FileStore) Review(id string) (Review, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	i := slices.IndexFunc(s.data.Reviews, func(r Review) bool { return r.ID == id })
	if i < 0 {
		return Review{}, ErrNotFound
	}
	return s.data.Reviews[i], nil
}

// UpsertReview creates the author's review of a book or updates the existing one.
// Returns ErrNotFound if the book does not exist.
func (s *FileStore) UpsertReview(bookID string, author Person, rating int, text string) (Review, error) {
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

func (s *FileStore) DeleteReview(id string) error {
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
func (s *FileStore) Meetings() []Meeting {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := slices.Clone(s.data.Meetings)
	slices.SortStableFunc(out, func(a, b Meeting) int { return a.StartsAt.Compare(b.StartsAt) })
	return out
}

func (s *FileStore) Meeting(id string) (Meeting, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	i := slices.IndexFunc(s.data.Meetings, func(m Meeting) bool { return m.ID == id })
	if i < 0 {
		return Meeting{}, ErrNotFound
	}
	return s.data.Meetings[i], nil
}

// AddMeeting assigns ID and timestamps. Returns ErrUnknownBook if m.BookID is set but missing.
func (s *FileStore) AddMeeting(m Meeting) (Meeting, error) {
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
func (s *FileStore) UpdateMeeting(m Meeting) (Meeting, error) {
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

func (s *FileStore) DeleteMeeting(id string) error {
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

// ---- persistence ----

// mutate applies fn to a copy, writes the copy to disk and only then makes it current.
// A failed fn or a failed write leaves both memory and file untouched.
func (s *FileStore) mutate(fn func(d *snapshot) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	next := s.data.clone()
	if err := fn(&next); err != nil {
		return err
	}
	if err := s.persist(next); err != nil {
		return err
	}
	s.data = next
	return nil
}

func (s *FileStore) persist(d snapshot) error {
	raw, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return fmt.Errorf("store: encode: %w", err)
	}

	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".bookclub-*.tmp") // created with 0600
	if err != nil {
		return fmt.Errorf("store: persist: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename has succeeded

	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return fmt.Errorf("store: persist: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("store: persist: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("store: persist: %w", err)
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		return fmt.Errorf("store: persist: %w", err)
	}
	return nil
}

func newID() string {
	var b [8]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read never fails since Go 1.24
	return hex.EncodeToString(b[:])
}
