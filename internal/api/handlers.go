package api

import (
	"errors"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"bookclub/internal/store"
)

// ---- DTOs: what the front end sees. Telegram user IDs of other members are never exposed. ----

type meDTO struct {
	ID      int64  `json:"id"`
	Name    string `json:"name"`
	IsAdmin bool   `json:"isAdmin"`
}

type ratingDTO struct {
	Average float64 `json:"average"` // 0 when there are no reviews
	Count   int     `json:"count"`
}

type bookDTO struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	Author    string    `json:"author,omitempty"`
	Link      string    `json:"link,omitempty"`
	Note      string    `json:"note,omitempty"`
	AddedBy   string    `json:"addedBy"`
	CreatedAt time.Time `json:"createdAt"`
	Rating    ratingDTO `json:"rating"`
	MyRating  int       `json:"myRating,omitempty"`
	CanDelete bool      `json:"canDelete"`
}

type reviewDTO struct {
	ID        string    `json:"id"`
	Author    string    `json:"author"`
	Rating    int       `json:"rating"`
	Text      string    `json:"text,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
	Mine      bool      `json:"mine"`
	CanDelete bool      `json:"canDelete"`
}

type bookDetailDTO struct {
	Book     bookDTO     `json:"book"`
	MyReview *reviewDTO  `json:"myReview"`
	Reviews  []reviewDTO `json:"reviews"` // everyone else's, newest first
}

type bookRefDTO struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	Author string `json:"author,omitempty"`
}

type meetingDTO struct {
	ID       string      `json:"id"`
	StartsAt time.Time   `json:"startsAt"`
	Place    string      `json:"place,omitempty"`
	Note     string      `json:"note,omitempty"`
	Book     *bookRefDTO `json:"book"`
}

// ---- me ----

func (s *Server) me(w http.ResponseWriter, _ *http.Request, u store.Person) {
	writeJSON(w, http.StatusOK, meDTO{ID: u.ID, Name: u.Name, IsAdmin: s.isAdmin(u.ID)})
}

// ---- books ----

type ratingAcc struct{ sum, count, mine int }

func (a ratingAcc) dto() ratingDTO {
	if a.count == 0 {
		return ratingDTO{}
	}
	return ratingDTO{Average: math.Round(float64(a.sum)*10/float64(a.count)) / 10, Count: a.count}
}

func accumulate(reviews []store.Review, me int64) map[string]ratingAcc {
	acc := make(map[string]ratingAcc)
	for _, r := range reviews {
		a := acc[r.BookID]
		a.sum += r.Rating
		a.count++
		if r.Author.ID == me {
			a.mine = r.Rating
		}
		acc[r.BookID] = a
	}
	return acc
}

func (s *Server) toBookDTO(b store.Book, acc ratingAcc, u store.Person) bookDTO {
	return bookDTO{
		ID: b.ID, Title: b.Title, Author: b.Author, Link: b.Link, Note: b.Note,
		AddedBy: b.AddedBy.Name, CreatedAt: b.CreatedAt,
		Rating: acc.dto(), MyRating: acc.mine,
		CanDelete: s.isAdmin(u.ID) || b.AddedBy.ID == u.ID,
	}
}

func (s *Server) toReviewDTO(r store.Review, u store.Person) reviewDTO {
	return reviewDTO{
		ID: r.ID, Author: r.Author.Name, Rating: r.Rating, Text: r.Text,
		CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
		Mine:      r.Author.ID == u.ID,
		CanDelete: s.isAdmin(u.ID) || r.Author.ID == u.ID,
	}
}

func (s *Server) listBooks(w http.ResponseWriter, _ *http.Request, u store.Person) {
	books := s.store.Books()
	acc := accumulate(s.store.AllReviews(), u.ID)
	out := make([]bookDTO, 0, len(books))
	for _, b := range books {
		out = append(out, s.toBookDTO(b, acc[b.ID], u))
	}
	writeJSON(w, http.StatusOK, map[string]any{"books": out})
}

type bookInput struct {
	Title  string `json:"title"`
	Author string `json:"author"`
	Link   string `json:"link"`
	Note   string `json:"note"`
}

func (in bookInput) validate() (store.Book, error) {
	var b store.Book
	var err error
	if b.Title, err = text(in.Title, 200, true, "Напишите название книги."); err != nil {
		return b, err
	}
	if b.Author, err = text(in.Author, 200, false, ""); err != nil {
		return b, err
	}
	if b.Note, err = text(in.Note, 1000, false, ""); err != nil {
		return b, err
	}
	b.Link, err = link(in.Link)
	return b, err
}

func (s *Server) createBook(w http.ResponseWriter, r *http.Request, u store.Person) {
	var in bookInput
	if !decodeJSON(w, r, &in) {
		return
	}
	b, err := in.validate()
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	b.AddedBy = u
	saved, err := s.store.AddBook(b)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, s.toBookDTO(saved, ratingAcc{}, u))
}

func (s *Server) getBook(w http.ResponseWriter, r *http.Request, u store.Person) {
	b, err := s.store.Book(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusNotFound, "Книга не найдена — возможно, её удалили.")
		return
	}
	reviews := s.store.Reviews(b.ID)
	out := bookDetailDTO{
		Book:    s.toBookDTO(b, accumulate(reviews, u.ID)[b.ID], u),
		Reviews: make([]reviewDTO, 0, len(reviews)),
	}
	for _, rv := range reviews {
		d := s.toReviewDTO(rv, u)
		if d.Mine {
			out.MyReview = &d
			continue
		}
		out.Reviews = append(out.Reviews, d)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) deleteBook(w http.ResponseWriter, r *http.Request, u store.Person) {
	b, err := s.store.Book(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusNotFound, "Книга не найдена — возможно, её уже удалили.")
		return
	}
	if !s.isAdmin(u.ID) && b.AddedBy.ID != u.ID {
		writeError(w, http.StatusForbidden, "Удалить книгу может только тот, кто её добавил, или администратор.")
		return
	}
	if err := s.store.DeleteBook(b.ID); err != nil && !errors.Is(err, store.ErrNotFound) {
		s.internalError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---- reviews ----

type reviewInput struct {
	Rating int    `json:"rating"`
	Text   string `json:"text"`
}

func (s *Server) putReview(w http.ResponseWriter, r *http.Request, u store.Person) {
	var in reviewInput
	if !decodeJSON(w, r, &in) {
		return
	}
	if in.Rating < 1 || in.Rating > 5 {
		writeError(w, http.StatusUnprocessableEntity, "Поставьте оценку от 1 до 5 звёзд.")
		return
	}
	body, err := text(in.Text, 4000, false, "")
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}

	saved, err := s.store.UpsertReview(r.PathValue("id"), u, in.Rating, body)
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "Книга не найдена — возможно, её удалили.")
	case err != nil:
		s.internalError(w, r, err)
	default:
		writeJSON(w, http.StatusOK, s.toReviewDTO(saved, u))
	}
}

func (s *Server) deleteReview(w http.ResponseWriter, r *http.Request, u store.Person) {
	rv, err := s.store.Review(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusNotFound, "Отзыв не найден — возможно, его уже удалили.")
		return
	}
	if !s.isAdmin(u.ID) && rv.Author.ID != u.ID {
		writeError(w, http.StatusForbidden, "Удалить отзыв может только его автор или администратор.")
		return
	}
	if err := s.store.DeleteReview(rv.ID); err != nil && !errors.Is(err, store.ErrNotFound) {
		s.internalError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---- meetings ----

func (s *Server) listMeetings(w http.ResponseWriter, _ *http.Request, _ store.Person) {
	books := make(map[string]store.Book)
	for _, b := range s.store.Books() {
		books[b.ID] = b
	}
	meetings := s.store.Meetings()
	out := make([]meetingDTO, 0, len(meetings))
	for _, m := range meetings {
		out = append(out, toMeetingDTO(m, books))
	}
	writeJSON(w, http.StatusOK, map[string]any{"meetings": out})
}

func toMeetingDTO(m store.Meeting, books map[string]store.Book) meetingDTO {
	d := meetingDTO{ID: m.ID, StartsAt: m.StartsAt, Place: m.Place, Note: m.Note}
	if b, ok := books[m.BookID]; ok {
		d.Book = &bookRefDTO{ID: b.ID, Title: b.Title, Author: b.Author}
	}
	return d
}

type meetingInput struct {
	StartsAt string `json:"startsAt"` // RFC 3339, the client sends its local time with offset or in UTC
	Place    string `json:"place"`
	BookID   string `json:"bookId"`
	Note     string `json:"note"`
}

func (in meetingInput) validate() (store.Meeting, error) {
	var m store.Meeting
	t, err := time.Parse(time.RFC3339, strings.TrimSpace(in.StartsAt))
	if err != nil {
		return m, validationError("Укажите дату и время встречи.")
	}
	m.StartsAt = t.UTC()
	if m.Place, err = text(in.Place, 200, false, ""); err != nil {
		return m, err
	}
	if m.Note, err = text(in.Note, 1000, false, ""); err != nil {
		return m, err
	}
	m.BookID = strings.TrimSpace(in.BookID)
	return m, nil
}

func (s *Server) requireAdmin(w http.ResponseWriter, u store.Person) bool {
	if s.isAdmin(u.ID) {
		return true
	}
	writeError(w, http.StatusForbidden, "Назначать и менять встречи может только администратор клуба.")
	return false
}

func (s *Server) createMeeting(w http.ResponseWriter, r *http.Request, u store.Person) {
	if !s.requireAdmin(w, u) {
		return
	}
	var in meetingInput
	if !decodeJSON(w, r, &in) {
		return
	}
	m, err := in.validate()
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	m.CreatedBy = u
	saved, err := s.store.AddMeeting(m)
	s.respondMeeting(w, r, http.StatusCreated, saved, err)
}

func (s *Server) updateMeeting(w http.ResponseWriter, r *http.Request, u store.Person) {
	if !s.requireAdmin(w, u) {
		return
	}
	var in meetingInput
	if !decodeJSON(w, r, &in) {
		return
	}
	m, err := in.validate()
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	m.ID = r.PathValue("id")
	saved, err := s.store.UpdateMeeting(m)
	s.respondMeeting(w, r, http.StatusOK, saved, err)
}

func (s *Server) respondMeeting(w http.ResponseWriter, r *http.Request, status int, m store.Meeting, err error) {
	switch {
	case errors.Is(err, store.ErrUnknownBook):
		writeError(w, http.StatusUnprocessableEntity, "Выбранная книга не найдена — возможно, её удалили.")
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "Встреча не найдена — возможно, её удалили.")
	case err != nil:
		s.internalError(w, r, err)
	default:
		books := make(map[string]store.Book, 1)
		if b, err := s.store.Book(m.BookID); err == nil {
			books[b.ID] = b
		}
		writeJSON(w, status, toMeetingDTO(m, books))
	}
}

func (s *Server) deleteMeeting(w http.ResponseWriter, r *http.Request, u store.Person) {
	if !s.requireAdmin(w, u) {
		return
	}
	err := s.store.DeleteMeeting(r.PathValue("id"))
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "Встреча не найдена — возможно, её уже удалили.")
	case err != nil:
		s.internalError(w, r, err)
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

// ---- validation ----

// validationError messages are shown to the user as is.
type validationError string

func (e validationError) Error() string { return string(e) }

// text trims, normalises line endings and enforces a length limit in characters.
func text(s string, max int, required bool, requiredMsg string) (string, error) {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\r\n", "\n"))
	if s == "" && required {
		return "", validationError(requiredMsg)
	}
	if !utf8.ValidString(s) {
		return "", validationError("Текст содержит недопустимые символы.")
	}
	if n := utf8.RuneCountInString(s); n > max {
		return "", validationError("Слишком длинный текст: " + strconv.Itoa(n) + " символов, можно не больше " + strconv.Itoa(max) + ".")
	}
	return s, nil
}

// link accepts http(s) URLs; a bare "litres.ru/..." gets https:// in front.
func link(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	const bad = validationError("Ссылка выглядит неправильно. Скопируйте её целиком из адресной строки браузера.")
	if len(raw) > 1000 || strings.ContainsAny(raw, " \t\n") {
		return "", bad
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || !strings.Contains(u.Hostname(), ".") {
		return "", bad
	}
	return raw, nil
}
