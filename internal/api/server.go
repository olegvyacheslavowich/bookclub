// Package api is the HTTP layer: authentication, permissions, validation and JSON.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"bookclub/internal/auth"
	"bookclub/internal/store"
)

// Store is what the handlers need from persistence. store.FileStore implements it.
type Store interface {
	Books() []store.Book
	Book(id string) (store.Book, error)
	AddBook(b store.Book) (store.Book, error)
	DeleteBook(id string) error

	AllReviews() []store.Review
	Reviews(bookID string) []store.Review
	Review(id string) (store.Review, error)
	UpsertReview(bookID string, author store.Person, rating int, text string) (store.Review, error)
	DeleteReview(id string) error

	Meetings() []store.Meeting
	Meeting(id string) (store.Meeting, error)
	AddMeeting(m store.Meeting) (store.Meeting, error)
	UpdateMeeting(m store.Meeting) (store.Meeting, error)
	DeleteMeeting(id string) error
}

type Authenticator interface {
	Validate(initData string) (auth.User, error)
}

type MemberChecker interface {
	IsMember(ctx context.Context, userID int64) (bool, error)
}

type Config struct {
	Store   Store
	Auth    Authenticator
	Members MemberChecker // nil disables the club membership check
	BotID   string        // numeric bot ID (token prefix), shown in auth error reasons
	DevMode bool          // accept "Authorization: dev <id>:<name>"; never enable in production
	Static  fs.FS
	Logger  *slog.Logger
}

type Server struct {
	store   Store
	auth    Authenticator
	members MemberChecker
	devMode bool
	botID   string
	static  fs.FS
	log     *slog.Logger

	seen sync.Map // user IDs already logged, so each member appears in the log once
}

func New(cfg Config) *Server {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Server{
		store:   cfg.Store,
		auth:    cfg.Auth,
		members: cfg.Members,
		devMode: cfg.DevMode,
		botID:   cfg.BotID,
		static:  cfg.Static,
		log:     logger,
	}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })

	route := func(pattern string, h userHandler) { mux.Handle(pattern, s.withUser(h)) }
	route("GET /api/me", s.me)
	route("GET /api/books", s.listBooks)
	route("POST /api/books", s.createBook)
	route("GET /api/books/{id}", s.getBook)
	route("DELETE /api/books/{id}", s.deleteBook)
	route("PUT /api/books/{id}/review", s.putReview)
	route("DELETE /api/reviews/{id}", s.deleteReview)
	route("GET /api/meetings", s.listMeetings)
	route("POST /api/meetings", s.createMeeting)
	route("PUT /api/meetings/{id}", s.updateMeeting)
	route("DELETE /api/meetings/{id}", s.deleteMeeting)
	mux.HandleFunc("/api/", func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusNotFound, "Неизвестный адрес API.")
	})

	if s.static != nil {
		files := http.FileServerFS(s.static)
		// "/" rather than "GET /": a method-specific catch-all would conflict with "/api/".
		mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet && r.Method != http.MethodHead {
				w.Header().Set("Allow", "GET, HEAD")
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			// Telegram's webview caches hard; make it revalidate so updates reach everyone.
			w.Header().Set("Cache-Control", "no-cache")
			files.ServeHTTP(w, r)
		}))
	}

	return s.recoverPanics(s.logRequests(securityHeaders(mux)))
}

// ---- authentication ----

type userHandler func(w http.ResponseWriter, r *http.Request, u store.Person)

func (s *Server) withUser(h userHandler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, dev, err := s.authenticate(r)
		if err != nil {
			s.log.Info("auth rejected", "path", r.URL.Path, "err", err)
			// The reason is shown under the error so problems can be diagnosed without server logs.
			// It never contains secrets: at most the bot's numeric ID, which is public.
			writeJSON(w, http.StatusUnauthorized, map[string]string{
				"error":  "Не удалось подтвердить вход. Закройте приложение и откройте его заново из Telegram.",
				"reason": s.authReason(err),
			})
			return
		}

		if s.members != nil && !dev {
			ok, err := s.members.IsMember(r.Context(), u.ID)
			if err != nil {
				s.log.Error("membership check failed", "user", u.ID, "err", err)
				writeError(w, http.StatusServiceUnavailable, "Не получилось проверить участие в клубе. Попробуйте через минуту.")
				return
			}
			if !ok {
				writeError(w, http.StatusForbidden, "Приложение доступно только участникам книжного клуба.")
				return
			}
		}

		if _, loaded := s.seen.LoadOrStore(u.ID, true); !loaded {
			s.log.Info("user seen", "id", u.ID, "name", u.Name)
		}
		h(w, r, u)
	})
}

// authenticate reads "Authorization: tma <initData>" (or "dev <id>:<name>" in dev mode).
func (s *Server) authenticate(r *http.Request) (u store.Person, dev bool, err error) {
	// Yandex Serverless Containers take the Authorization header for their own IAM check and
	// don't pass it to the app, so the front end sends credentials in AuthHeader instead.
	raw := r.Header.Get(AuthHeader)
	if raw == "" {
		raw = r.Header.Get("Authorization")
	}
	scheme, value, _ := strings.Cut(raw, " ")
	switch {
	case scheme == "tma":
		tu, err := s.auth.Validate(value)
		if err != nil {
			return store.Person{}, false, err
		}
		return store.Person{ID: tu.ID, Name: tu.DisplayName()}, false, nil

	case scheme == "dev" && s.devMode:
		// The front end percent-encodes the value: HTTP headers can't carry Cyrillic as is.
		if decoded, err := url.PathUnescape(value); err == nil {
			value = decoded
		}
		idStr, name, _ := strings.Cut(value, ":")
		id, err := strconv.ParseInt(idStr, 10, 64)
		if err != nil || id == 0 {
			return store.Person{}, true, errBadDevUser
		}
		if name = strings.TrimSpace(name); name == "" {
			name = "Тестовый участник " + idStr
		}
		return store.Person{ID: id, Name: name}, true, nil

	default:
		return store.Person{}, false, errNoCredentials
	}
}

func (s *Server) authReason(err error) string {
	switch {
	case errors.Is(err, auth.ErrBadHash):
		return "Подпись Telegram не совпала с токеном на сервере (бот ID " + s.botID + "). " +
			"Приложение должно быть создано через /newapp у этого же бота, а в BOT_TOKEN — его токен."
	case errors.Is(err, auth.ErrExpired):
		return "Данные входа устарели. Закройте приложение крестиком и откройте снова."
	case errors.Is(err, auth.ErrMissingHash):
		return "Telegram не передал подпись (hash)."
	case errors.Is(err, auth.ErrNoUser):
		return "Telegram не передал данные пользователя."
	case errors.Is(err, errNoCredentials):
		return "Запрос пришёл без данных входа Telegram."
	default:
		return "Техническая причина: " + err.Error()
	}
}

// AuthHeader carries "tma <initData>" (or "dev <id>:<name>" in dev mode).
const AuthHeader = "X-Telegram-Auth"

type authError string

func (e authError) Error() string { return string(e) }

const (
	errNoCredentials = authError("no supported Authorization header")
	errBadDevUser    = authError("dev auth must look like 'dev <id>:<name>'")
)

// ---- middleware ----

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Content-Security-Policy",
			"default-src 'self'; script-src 'self' https://telegram.org; style-src 'self' 'unsafe-inline'; "+
				"img-src 'self' data:; connect-src 'self'; base-uri 'none'; form-action 'none'")
		next.ServeHTTP(w, r)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			next.ServeHTTP(w, r)
			return
		}
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		s.log.Info("http", "method", r.Method, "path", r.URL.Path, "status", rec.status, "took", time.Since(start).Round(time.Microsecond))
	})
}

func (s *Server) recoverPanics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				if v == http.ErrAbortHandler {
					panic(v)
				}
				s.log.Error("panic", "path", r.URL.Path, "value", v)
				writeError(w, http.StatusInternalServerError, "Что-то пошло не так. Попробуйте ещё раз.")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// ---- JSON helpers ----

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

const maxBody = 64 << 10

func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		writeError(w, http.StatusBadRequest, "Некорректный запрос.")
		return false
	}
	return true
}

func (s *Server) internalError(w http.ResponseWriter, r *http.Request, err error) {
	s.log.Error("internal error", "path", r.URL.Path, "err", err)
	writeError(w, http.StatusInternalServerError, "Не удалось сохранить. Попробуйте ещё раз.")
}
