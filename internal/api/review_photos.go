package api

import (
	"errors"
	"net/http"

	"bookclub/internal/store"
)

// One photo per review, kept in the same storage as book covers (named "<review id>-<random>.<ext>").

// reviewPhotoURL is where the front end loads the review's photo from; ?v= changes with every
// new photo, so the browser may cache each URL forever.
func reviewPhotoURL(r store.Review) string {
	if r.Photo == "" {
		return ""
	}
	return "api/reviews/" + r.ID + "/photo?v=" + r.Photo
}

// putReviewPhoto: PUT /api/reviews/{id}/photo, the body is the photo itself (JPEG, PNG or WebP).
// Only the review's author adds or replaces it. Same order as putCover: the file first, then the
// review points at it, then the old file goes.
func (s *Server) putReviewPhoto(w http.ResponseWriter, r *http.Request, u store.Person) {
	if !s.requireCovers(w) {
		return
	}
	rv, err := s.store.Review(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusNotFound, "Отзыв не найден — возможно, его удалили.")
		return
	}
	if rv.Author.ID != u.ID {
		writeError(w, http.StatusForbidden, "Фото к отзыву добавляет только его автор.")
		return
	}

	name, ok := s.uploadPhoto(w, r, rv.ID)
	if !ok {
		return
	}
	old, err := s.store.SetReviewPhoto(rv.ID, name)
	if err != nil {
		s.dropCover(name)
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "Отзыв не найден — возможно, его удалили.")
			return
		}
		s.internalError(w, r, err)
		return
	}
	if old != "" {
		s.dropCover(old)
	}
	rv.Photo = name
	writeJSON(w, http.StatusOK, map[string]string{"photoUrl": reviewPhotoURL(rv)})
}

// getReviewPhoto: GET /api/reviews/{id}/photo — the photo bytes, for any member.
func (s *Server) getReviewPhoto(w http.ResponseWriter, r *http.Request, _ store.Person) {
	if !s.requireCovers(w) {
		return
	}
	rv, err := s.store.Review(r.PathValue("id"))
	if err != nil || rv.Photo == "" {
		writeError(w, http.StatusNotFound, "У этого отзыва нет фото.")
		return
	}
	s.servePhoto(w, r, rv.Photo, "private")
}

// deleteReviewPhoto: DELETE /api/reviews/{id}/photo — the author, or an administrator moderating.
func (s *Server) deleteReviewPhoto(w http.ResponseWriter, r *http.Request, u store.Person) {
	if !s.requireCovers(w) {
		return
	}
	rv, err := s.store.Review(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusNotFound, "Отзыв не найден — возможно, его удалили.")
		return
	}
	if rv.Author.ID != u.ID && !s.isAdmin(u.ID) {
		writeError(w, http.StatusForbidden, "Фото убирает автор отзыва или администратор клуба.")
		return
	}
	old, err := s.store.SetReviewPhoto(rv.ID, "")
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "Отзыв не найден — возможно, его удалили.")
		return
	case err != nil:
		s.internalError(w, r, err)
		return
	}
	if old != "" {
		s.dropCover(old)
	}
	if rv.Author.ID != u.ID {
		s.log.Info("admin removed review photo", "admin", u.Name, "book_id", rv.BookID, "author", rv.Author.Name)
	}
	w.WriteHeader(http.StatusNoContent)
}
