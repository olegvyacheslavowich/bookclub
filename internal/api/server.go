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
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"bookclub/internal/auth"
	"bookclub/internal/store"
)

// Store is what the handlers need from persistence. store.FileStore implements it.
type Store interface {
	Books() []store.Book
	Book(id string) (store.Book, error)
	AddBook(b store.Book) (store.Book, error)
	SetCover(bookID, cover string) (old string, err error)
	DeleteBook(id string) error

	AllReviews() []store.Review
	Reviews(bookID string) []store.Review
	Review(id string) (store.Review, error)
	UpsertReview(bookID string, author store.Person, rating int, text string) (store.Review, error)
	SetReviewPhoto(reviewID, photo string) (old string, err error)
	SetPhotoLink(file, link string) error
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
	Members MemberChecker // nil disables the getChatMember check
	// Covers stores photos of book covers (Yandex Disk). nil: no photos, the buttons are hidden.
	Covers CoverStore
	// ClubChatInstances: the chat_instance of the club's group. When set, the app opens for anyone
	// who launched it from a link inside that group, without asking the Bot API. Empty disables it.
	ClubChatInstances []string
	// Announcer posts new books and reviews to the club's group; nil disables the posts.
	Announcer Announcer
	// AppURL is the Mini App's direct link (https://t.me/<bot>/<app>) for the button under posts.
	// Empty: posts go out without a button.
	AppURL string
	// PublicURL is the server's own https address, for absolute links in shared-review previews.
	// Empty: taken from the request (Host), which is right unless a proxy rewrites it.
	PublicURL string
	// ShareKey signs the public links to shared reviews (see share.go). Empty: a random key, so
	// links stop working after a restart; main derives it from the bot token.
	ShareKey []byte
	// AdminIDs: Telegram user IDs of the club's administrators. They may delete any book or review
	// and are the only ones who schedule meetings. Empty: no administrators, everyone is equal and
	// any member manages meetings.
	AdminIDs []int64
	BotID    string // numeric bot ID (token prefix), shown in auth error reasons
	DevMode  bool   // accept "Authorization: dev <id>:<name>"; never enable in production
	Static   fs.FS
	Logger   *slog.Logger
}

type Server struct {
	store     Store
	auth      Authenticator
	members   MemberChecker
	covers    CoverStore
	clubChats []string
	announcer Announcer
	appURL    string
	publicURL string
	shareKey  []byte
	admins    map[int64]bool
	devMode   bool
	botID     string
	static    fs.FS
	log       *slog.Logger

	publishFailedAt atomic.Int64 // UnixNano of the last failed publish on Disk; 0: none (see share.go)

	seen      sync.Map // user IDs already logged, so each member appears in the log once
	seenChats sync.Map // chat_instance values already logged
}

func New(cfg Config) *Server {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	admins := make(map[int64]bool, len(cfg.AdminIDs))
	for _, id := range cfg.AdminIDs {
		admins[id] = true
	}
	return &Server{
		store:     cfg.Store,
		auth:      cfg.Auth,
		members:   cfg.Members,
		covers:    cfg.Covers,
		clubChats: cfg.ClubChatInstances,
		announcer: cfg.Announcer,
		appURL:    cfg.AppURL,
		publicURL: strings.TrimRight(cfg.PublicURL, "/"),
		shareKey:  newShareKey(cfg.ShareKey),
		admins:    admins,
		devMode:   cfg.DevMode,
		botID:     cfg.BotID,
		static:    cfg.Static,
		log:       logger,
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
	route("GET /api/books/{id}/cover", s.getCover)
	route("PUT /api/books/{id}/cover", s.putCover)
	route("DELETE /api/books/{id}/cover", s.deleteCover)
	route("PUT /api/books/{id}/review", s.putReview)
	route("DELETE /api/reviews/{id}", s.deleteReview)
	route("GET /api/reviews/{id}/photo", s.getReviewPhoto)
	route("PUT /api/reviews/{id}/photo", s.putReviewPhoto)
	route("DELETE /api/reviews/{id}/photo", s.deleteReviewPhoto)
	route("GET /api/meetings", s.listMeetings)
	route("POST /api/meetings", s.createMeeting)
	route("PUT /api/meetings/{id}", s.updateMeeting)
	route("DELETE /api/meetings/{id}", s.deleteMeeting)
	// Public, for Telegram's link previews of shared reviews (see share.go).
	mux.HandleFunc("GET /s/{token}", s.sharePage)
	mux.HandleFunc("GET /s/{token}/photo", s.shareImage)
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

// ---- permissions ----

func (s *Server) isAdmin(userID int64) bool { return s.admins[userID] }

// canManageMeetings: with administrators configured, only they schedule, change and cancel
// meetings; without them, any member does.
func (s *Server) canManageMeetings(userID int64) bool { return len(s.admins) == 0 || s.isAdmin(userID) }

// ---- authentication ----

type userHandler func(w http.ResponseWriter, r *http.Request, u store.Person)

func (s *Server) withUser(h userHandler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := s.authenticate(r)
		u := c.Person
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

		// Logged once per chat: this is how the club's chat_instance is found for CLUB_CHAT_INSTANCE.
		if c.chatInstance != "" {
			if _, loaded := s.seenChats.LoadOrStore(c.chatInstance, true); !loaded {
				s.log.Info("opened from chat", "chat_instance", c.chatInstance, "chat_type", c.chatType, "user", u.Name)
			}
		}

		if !c.dev && !s.admit(w, r, c) {
			return
		}

		if _, loaded := s.seen.LoadOrStore(u.ID, true); !loaded {
			s.log.Info("user seen", "id", u.ID, "name", u.Name)
		}
		h(w, r.WithContext(context.WithValue(r.Context(), callerKey{}, c)), u)
	})
}

// admit decides whether an authenticated caller is a club member and writes the refusal if not.
//
//  1. Opened from a link inside the club's group (chat_instance matches): a member. Only people in
//     the group can see that link; the same link forwarded elsewhere arrives with another
//     chat_instance. Needs no Bot API, which is often unreachable from servers in Russia.
//  2. Otherwise, if CLUB_CHAT_ID is set: ask Telegram with getChatMember.
//  3. Otherwise, if only CLUB_CHAT_INSTANCE is set: refuse, asking to open the app from the group.
//  4. Nothing configured: everyone gets in.
func (s *Server) admit(w http.ResponseWriter, r *http.Request, c caller) bool {
	if len(s.clubChats) > 0 && c.chatInstance != "" && slices.Contains(s.clubChats, c.chatInstance) {
		return true
	}

	if s.members != nil {
		ok, err := s.members.IsMember(r.Context(), c.ID)
		if err != nil {
			s.log.Error("membership check failed", "user", c.ID, "err", err)
			msg := "Не получилось проверить участие в клубе. Попробуйте через минуту."
			if len(s.clubChats) > 0 {
				msg = "Не получилось проверить участие в клубе. Откройте приложение по ссылке из группы книжного клуба."
			}
			// err is Telegram's own description or a network error; the bot token is never in it.
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{
				"error":  msg,
				"reason": "Техническая причина: " + err.Error(),
			})
			return false
		}
		if !ok {
			writeError(w, http.StatusForbidden, "Приложение доступно только участникам книжного клуба.")
		}
		return ok
	}

	if len(s.clubChats) > 0 {
		writeError(w, http.StatusForbidden, "Приложение доступно только участникам книжного клуба. "+
			"Откройте его по ссылке из группы клуба — например, из закреплённого сообщения.")
		return false
	}
	return true
}

type callerKey struct{}

// callerFrom returns the authenticated caller withUser put into the request context.
func callerFrom(r *http.Request) caller {
	c, _ := r.Context().Value(callerKey{}).(caller)
	return c
}

// accessRestricted: some club membership check is configured (see admit).
func (s *Server) accessRestricted() bool { return len(s.clubChats) > 0 || s.members != nil }

// caller is an authenticated request: who it is and which chat the app was opened from.
type caller struct {
	store.Person
	chatInstance string
	chatType     string
	dev          bool
}

// authenticate reads "X-Telegram-Auth: tma <initData>" (or "dev <id>:<name>" in dev mode).
func (s *Server) authenticate(r *http.Request) (caller, error) {
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
			return caller{}, err
		}
		return caller{
			Person:       store.Person{ID: tu.ID, Name: tu.DisplayName()},
			chatInstance: tu.ChatInstance,
			chatType:     tu.ChatType,
		}, nil

	case scheme == "dev" && s.devMode:
		// The front end percent-encodes the value: HTTP headers can't carry Cyrillic as is.
		if decoded, err := url.PathUnescape(value); err == nil {
			value = decoded
		}
		idStr, name, _ := strings.Cut(value, ":")
		id, err := strconv.ParseInt(idStr, 10, 64)
		if err != nil || id == 0 {
			return caller{dev: true}, errBadDevUser
		}
		if name = strings.TrimSpace(name); name == "" {
			name = "Тестовый участник " + idStr
		}
		return caller{Person: store.Person{ID: id, Name: name}, dev: true}, nil

	default:
		return caller{}, errNoCredentials
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
				"img-src 'self' data: blob:; connect-src 'self'; base-uri 'none'; form-action 'none'")
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
		attrs := []any{"method", r.Method, "path", r.URL.Path, "status", rec.status, "took", time.Since(start).Round(time.Microsecond)}
		if strings.HasPrefix(r.URL.Path, "/s/") {
			// Public share pages: shows whether Telegram's preview robot ("TelegramBot") got here.
			attrs = append(attrs, "ua", r.UserAgent())
		}
		s.log.Info("http", attrs...)
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
