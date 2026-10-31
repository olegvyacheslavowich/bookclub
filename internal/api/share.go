package api

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"html/template"
	"net"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"bookclub/internal/covers"
	"bookclub/internal/store"
)

// Link previews for reviews shared to the club's group.
//
// The front end shares through t.me/share/url, which carries only text and a link: a photo can't
// be attached to the message. So when the review (or its book) has a photo, the message's first
// link is one Telegram can build a photo preview from, and the Mini App link goes into the text.
//
// The preview link is the photo's public link on Yandex Disk (https://yadi.sk/i/…): Telegram's
// preview robot can't reach Yandex Cloud, but it reads Disk's pages fine. The photo is published
// once, when its author first opens the book page, and the link is kept in the club's document.
//
// Without Disk (photos in a local folder, a VPS) the link goes to our own public page /s/<token>
// with Open Graph tags instead; that works wherever Telegram reaches the server.
//
// Our page doesn't send people on to the Mini App: opened from a browser, the app has no
// chat_instance of the club's group and refuses access (see admit). Members open the app by the
// link in the message text, from the group, as before. The page only says so.
//
// The page and its photo are public: Telegram's crawler has no member credentials. The token is
// the review ID signed with a key derived from the bot token, so only links the app handed out
// work and nobody can walk through the club's reviews. The page shows only the book, the rating
// and the photo — the review text is in the message itself, not on the page.

// shareTokenBytes: 16 bytes of HMAC, 22 characters in the link.
const shareTokenBytes = 16

func newShareKey(seed []byte) []byte {
	if len(seed) == 0 {
		// No bot token (DEV_MODE): links work until the restart, which is enough for testing.
		key := make([]byte, 32)
		_, _ = rand.Read(key)
		return key
	}
	return seed
}

func (s *Server) shareSig(reviewID string) string {
	m := hmac.New(sha256.New, s.shareKey)
	m.Write([]byte("review:" + reviewID))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil)[:shareTokenBytes])
}

// shareToken: "<review id>-<signature>".
func (s *Server) shareToken(reviewID string) string { return reviewID + "-" + s.shareSig(reviewID) }

func (s *Server) checkShareToken(token string) (reviewID string, ok bool) {
	// Cut at the first "-": review IDs are hex, but the base64url signature may contain "-".
	id, sig, ok := strings.Cut(token, "-")
	if !ok || id == "" {
		return "", false
	}
	return id, hmac.Equal([]byte(sig), []byte(s.shareSig(id)))
}

// sharePhoto: the photo shown in the link preview — the review's own, else the book's cover.
func (s *Server) sharePhoto(b store.Book, r store.Review) string {
	if s.covers == nil {
		return ""
	}
	if r.Photo != "" {
		return r.Photo
	}
	return b.Cover
}

// publishTimeout bounds publishing a photo on Disk: two API requests, made while the author's
// book page loads.
const publishTimeout = 8 * time.Second

// publishPause: after a failed publish (e.g. the token may not publish), don't retry for a while,
// so every page load doesn't wait for the same error.
const publishPause = 5 * time.Minute

// reviewShareURL is the link the front end puts first in the shared message so that Telegram
// shows the photo — the review's own, else the book's cover. Empty when there's no photo or it
// couldn't be published: the front end then shares the Mini App link as before.
func (s *Server) reviewShareURL(ctx context.Context, b store.Book, r store.Review) string {
	photo := s.sharePhoto(b, r)
	if photo == "" {
		return ""
	}
	link := r.PhotoLink
	if photo != r.Photo {
		link = b.CoverLink
	}
	if link != "" {
		return link
	}

	link, err := s.publishPhoto(ctx, photo)
	switch {
	case err == nil:
		return link
	case errors.Is(err, covers.ErrCannotPublish):
		// Photos in a local folder: our own page. ?v= changes with the photo, as Telegram caches
		// previews per URL; no extension, or the link may be taken for a bare image.
		return "s/" + s.shareToken(r.ID) + "?v=" + url.QueryEscape(strings.TrimSuffix(photo, path.Ext(photo)))
	default:
		return ""
	}
}

// publishPhoto gives a photo a public link on Yandex Disk and keeps it in the club's document.
func (s *Server) publishPhoto(ctx context.Context, file string) (string, error) {
	p, ok := s.covers.(covers.Publisher)
	if !ok {
		return "", covers.ErrCannotPublish
	}
	if last := s.publishFailedAt.Load(); last != 0 && time.Since(time.Unix(0, last)) < publishPause {
		return "", errPublishPaused
	}
	ctx, cancel := context.WithTimeout(ctx, publishTimeout)
	defer cancel()
	link, err := p.Publish(ctx, file)
	if errors.Is(err, covers.ErrCannotPublish) {
		return "", err
	}
	if err != nil {
		s.publishFailedAt.Store(time.Now().UnixNano())
		s.log.Warn("photo not published on Yandex Disk: shared reviews go without a photo preview", "file", file, "err", err)
		return "", err
	}
	s.publishFailedAt.Store(0)
	if err := s.store.SetPhotoLink(file, link); err != nil && !errors.Is(err, store.ErrNotFound) {
		s.log.Warn("photo link not saved", "file", file, "err", err) // it'll be published again next time
	}
	return link, nil
}

var errPublishPaused = errors.New("publishing paused after a recent failure")

// sharedReview resolves /s/{token}; on failure it has written a 404.
func (s *Server) sharedReview(w http.ResponseWriter, r *http.Request) (store.Book, store.Review, bool) {
	id, ok := s.checkShareToken(r.PathValue("token"))
	if ok {
		if rv, err := s.store.Review(id); err == nil {
			if b, err := s.store.Book(rv.BookID); err == nil {
				return b, rv, true
			}
		}
	}
	w.Header().Set("X-Robots-Tag", "noindex")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusNotFound)
	_ = sharePageTmpl.Execute(w, sharePageData{
		Title:       "Отзыв не найден",
		Description: "Возможно, его удалили. Остальные отзывы — в приложении книжного клуба.",
	})
	return store.Book{}, store.Review{}, false
}

// sharePage: GET /s/{token} — the page Telegram turns into the link preview.
func (s *Server) sharePage(w http.ResponseWriter, r *http.Request) {
	b, rv, ok := s.sharedReview(w, r)
	if !ok {
		return
	}
	d := sharePageData{
		Title:       bookLine(b),
		Description: "Отзыв участника книжного клуба · " + starLine(rv.Rating),
		Hint:        "Чтобы прочитать отзыв в приложении клуба, нажмите ссылку «Открыть в приложении» в сообщении в группе.",
	}
	if photo := s.sharePhoto(b, rv); photo != "" {
		d.Image = s.publicBase(r) + "/s/" + s.shareToken(rv.ID) + "/photo?v=" + url.QueryEscape(photo)
	}
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "public, max-age=300")
	h.Set("X-Robots-Tag", "noindex")
	_ = sharePageTmpl.Execute(w, d)
}

// shareImage: GET /s/{token}/photo — the preview photo, fetched by Telegram's servers.
func (s *Server) shareImage(w http.ResponseWriter, r *http.Request) {
	b, rv, ok := s.sharedReview(w, r)
	if !ok {
		return
	}
	photo := s.sharePhoto(b, rv)
	if photo == "" {
		writeError(w, http.StatusNotFound, "Фото нет.")
		return
	}
	w.Header().Set("X-Robots-Tag", "noindex")
	s.servePhoto(w, r, photo, "public")
}

// publicBase is the server's own address for absolute links (og:image must be absolute):
// PUBLIC_URL if set, otherwise the address the request came to.
func (s *Server) publicBase(r *http.Request) string {
	if s.publicURL != "" {
		return s.publicURL
	}
	scheme := r.Header.Get("X-Forwarded-Proto")
	if scheme != "http" && scheme != "https" {
		scheme = "https"
		if r.TLS == nil && isLoopback(r.Host) {
			scheme = "http" // local testing
		}
	}
	return scheme + "://" + r.Host
}

func isLoopback(hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	ip := net.ParseIP(host)
	return host == "localhost" || (ip != nil && ip.IsLoopback())
}

// bookAppLink opens the Mini App straight on the book: https://t.me/<bot>/<app>?startapp=book_<id>.
// Empty when MINIAPP_URL isn't set.
func (s *Server) bookAppLink(bookID string) string {
	if s.appURL == "" {
		return ""
	}
	u, err := url.Parse(s.appURL)
	if err != nil {
		return ""
	}
	q := u.Query()
	q.Set("startapp", "book_"+bookID)
	u.RawQuery = q.Encode()
	return u.String()
}

type sharePageData struct {
	Title, Description string
	Image              string // absolute URL; empty: no photo
	Hint               string // how to get into the app; shown to people only, not in the preview
}

var sharePageTmpl = template.Must(template.New("share").Parse(`<!doctype html>
<html lang="ru">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="robots" content="noindex, nofollow">
<title>{{.Title}}</title>
<meta property="og:type" content="article">
<meta property="og:site_name" content="Книжный клуб">
<meta property="og:title" content="{{.Title}}">
<meta property="og:description" content="{{.Description}}">
{{- if .Image}}
<meta property="og:image" content="{{.Image}}">
<meta name="twitter:card" content="summary_large_image">
{{- end}}
<style>
body { margin: 0; font: 16px/1.45 -apple-system, system-ui, "Segoe UI", Roboto, sans-serif; background: #f4f4f5; color: #1c1c1e; }
main { max-width: 480px; margin: 0 auto; padding: 24px 16px; text-align: center; }
img { max-width: 100%; max-height: 360px; border-radius: 12px; }
h1 { font-size: 20px; margin: 16px 0 6px; }
p { color: #6b6b70; margin: 0 0 20px; }
.hint { font-size: 15px; }
@media (prefers-color-scheme: dark) { body { background: #18181b; color: #f4f4f5; } p { color: #a1a1aa; } }
</style>
</head>
<body>
<main>
{{- if .Image}}
<img src="{{.Image}}" alt="">
{{- end}}
<h1>{{.Title}}</h1>
<p>{{.Description}}</p>
{{- if .Hint}}
<p class="hint">{{.Hint}}</p>
{{- end}}
</main>
</body>
</html>
`))
