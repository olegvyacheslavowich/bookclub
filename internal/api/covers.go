package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"bookclub/internal/covers"
	"bookclub/internal/store"
)

// CoverStore keeps the photo files — book covers and review photos; the club's document only stores
// their names.
// covers.Disk (Yandex Disk) and covers.Dir implement it, usually wrapped in covers.Cached.
type CoverStore interface {
	Put(ctx context.Context, name string, data []byte) error
	Get(ctx context.Context, name string) ([]byte, error)
	Delete(ctx context.Context, name string) error
}

// coverTimeout bounds a round trip to Yandex Disk: two requests, a few hundred KB.
const coverTimeout = 20 * time.Second

// canEditBook: the member who added the book, or an administrator.
func (s *Server) canEditBook(b store.Book, u store.Person) bool {
	return b.AddedBy.ID == u.ID || s.isAdmin(u.ID)
}

// coverURL is where the front end loads the cover from. The file name in ?v= changes with every
// new photo, so the browser may cache each URL forever.
func coverURL(b store.Book) string {
	if b.Cover == "" {
		return ""
	}
	return "api/books/" + b.ID + "/cover?v=" + b.Cover
}

// requireCovers writes 404 when photo storage isn't configured.
func (s *Server) requireCovers(w http.ResponseWriter) bool {
	if s.covers != nil {
		return true
	}
	writeError(w, http.StatusNotFound, "Фото обложек в этом клубе не настроены.")
	return false
}

// putCover: PUT /api/books/{id}/cover, the body is the photo itself (JPEG, PNG or WebP).
//
// Order matters: the file goes to the storage first and the book points at it only after that, so
// the book never shows a photo that isn't there. A new photo gets a new file name; the old file is
// deleted last, and failing to delete it only leaves a stray file on the Disk.
func (s *Server) putCover(w http.ResponseWriter, r *http.Request, u store.Person) {
	if !s.requireCovers(w) {
		return
	}
	b, err := s.store.Book(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusNotFound, "Книга не найдена — возможно, её удалили.")
		return
	}
	if !s.canEditBook(b, u) {
		writeError(w, http.StatusForbidden, "Фото обложки меняет тот, кто добавил книгу, или администратор клуба.")
		return
	}

	name, ok := s.uploadPhoto(w, r, b.ID)
	if !ok {
		return
	}

	old, err := s.store.SetCover(b.ID, name)
	if err != nil {
		s.dropCover(name)
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "Книга не найдена — возможно, её удалили.")
			return
		}
		s.internalError(w, r, err)
		return
	}
	if old != "" {
		s.dropCover(old)
	}
	if b.AddedBy.ID != u.ID {
		s.log.Info("admin changed cover", "admin", u.Name, "book", b.Title)
	}
	b.Cover = name
	writeJSON(w, http.StatusOK, map[string]string{"coverUrl": coverURL(b)})
}

// getCover: GET /api/books/{id}/cover — the photo bytes. An <img> can't send the Telegram
// credentials, so the front end fetches this with them and shows the result as a blob.
func (s *Server) getCover(w http.ResponseWriter, r *http.Request, _ store.Person) {
	if !s.requireCovers(w) {
		return
	}
	b, err := s.store.Book(r.PathValue("id"))
	if err != nil || b.Cover == "" {
		writeError(w, http.StatusNotFound, "У этой книги нет фото.")
		return
	}
	s.servePhoto(w, r, b.Cover, "private")
}

// deleteCover: DELETE /api/books/{id}/cover.
func (s *Server) deleteCover(w http.ResponseWriter, r *http.Request, u store.Person) {
	if !s.requireCovers(w) {
		return
	}
	b, err := s.store.Book(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusNotFound, "Книга не найдена — возможно, её удалили.")
		return
	}
	if !s.canEditBook(b, u) {
		writeError(w, http.StatusForbidden, "Фото обложки убирает тот, кто добавил книгу, или администратор клуба.")
		return
	}
	old, err := s.store.SetCover(b.ID, "")
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "Книга не найдена — возможно, её удалили.")
		return
	case err != nil:
		s.internalError(w, r, err)
		return
	}
	if old != "" {
		s.dropCover(old)
	}
	w.WriteHeader(http.StatusNoContent)
}

// uploadPhoto reads the photo from the request body and stores it as "<owner>-<random>.<ext>".
// A new photo always gets a new name, so a name always means the same bytes. On failure it has
// already written the answer.
func (s *Server) uploadPhoto(w http.ResponseWriter, r *http.Request, owner string) (name string, ok bool) {
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, covers.MaxSize))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeError(w, http.StatusRequestEntityTooLarge, "Фото слишком большое. Выберите другое или сделайте снимок камерой.")
			return "", false
		}
		writeError(w, http.StatusBadRequest, "Фото не загрузилось целиком. Попробуйте ещё раз.")
		return "", false
	}
	ext, ok := covers.Ext(data)
	if !ok {
		writeError(w, http.StatusUnsupportedMediaType, "Это не похоже на фото. Подойдут снимки в JPEG, PNG или WebP.")
		return "", false
	}

	name = owner + "-" + randomHex() + ext
	ctx, cancel := context.WithTimeout(r.Context(), coverTimeout)
	defer cancel()
	if err := s.covers.Put(ctx, name, data); err != nil {
		s.log.Error("photo upload failed", "owner", owner, "bytes", len(data), "err", err)
		writeError(w, http.StatusBadGateway, "Не получилось сохранить фото. Попробуйте ещё раз через минуту.")
		return "", false
	}
	return name, true
}

// servePhoto writes the stored photo. With ?v=<name> the URL always means these bytes and may be
// cached for good; scope is "private" (only the member's own phone keeps it) or "public".
func (s *Server) servePhoto(w http.ResponseWriter, r *http.Request, name, scope string) {
	ctx, cancel := context.WithTimeout(r.Context(), coverTimeout)
	defer cancel()
	data, err := s.covers.Get(ctx, name)
	if err != nil {
		if errors.Is(err, covers.ErrNotFound) {
			// Deleted on the Disk by hand: the record keeps the name, but there's nothing to show.
			s.log.Warn("photo file missing", "file", name)
			writeError(w, http.StatusNotFound, "Фото не найдено.")
			return
		}
		s.log.Error("photo download failed", "file", name, "err", err)
		writeError(w, http.StatusBadGateway, "Не получилось загрузить фото.")
		return
	}

	h := w.Header()
	h.Set("Content-Type", covers.ContentType(name))
	h.Set("Content-Length", strconv.Itoa(len(data)))
	if r.URL.Query().Get("v") == name {
		h.Set("Cache-Control", scope+", max-age=31536000, immutable")
	} else {
		h.Set("Cache-Control", scope+", no-cache")
	}
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(data)
	}
}

// dropCover deletes a photo file (a cover or a review photo) that nothing points at any more. Failure only leaves a stray
// file on the Disk, so it is logged, not reported to the member.
func (s *Server) dropCover(name string) {
	if s.covers == nil || name == "" {
		return
	}
	// Not the request's context: the file should go even if the member closes the app.
	ctx, cancel := context.WithTimeout(context.Background(), coverTimeout)
	defer cancel()
	if err := s.covers.Delete(ctx, name); err != nil {
		s.log.Warn("cover file not deleted", "file", name, "err", err)
	}
}

func randomHex() string {
	var b [8]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read never fails since Go 1.24
	return hex.EncodeToString(b[:])
}
