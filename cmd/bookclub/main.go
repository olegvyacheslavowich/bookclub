// Command bookclub serves the book club Telegram Mini App: the static front end and its JSON API.
package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode"

	"bookclub/internal/api"
	"bookclub/internal/auth"
	"bookclub/internal/covers"
	"bookclub/internal/store"
	"bookclub/internal/tg"
	"bookclub/web"
)

type config struct {
	addr       string
	dataFile   string
	bucket     string // Object Storage bucket; when set, DATA_FILE is ignored
	bucketKey  string
	endpoint   string // S3 endpoint override, e.g. a local MinIO; default Yandex Object Storage
	iamToken   string // fixed IAM token for running locally against a real bucket
	botToken   string
	chatID     string
	clubChats  []string // chat_instance of the club's group: members open the app from there
	tgAPI      string   // Bot API base URL; a reverse proxy abroad when api.telegram.org is blocked
	notify     bool     // post new books and reviews to the club's group
	appURL     string   // Mini App direct link for the button under posts
	publicURL  string   // the server's own https address, for shared-review link previews
	adminIDs   []int64  // Telegram user IDs of the club's administrators
	diskToken  string   // Yandex Disk OAuth token: cover photos go to the owner's Disk
	diskDir    string   // folder on the Disk, "app:/covers" by default
	coversDir  string   // local folder for cover photos when there is no Disk token
	devMode    bool
	initMaxAge time.Duration
}

func loadConfig() (config, error) {
	c := config{
		addr:       listenAddr(),
		dataFile:   env("DATA_FILE", "data/bookclub.json"),
		bucket:     os.Getenv("DATA_BUCKET"),
		bucketKey:  env("DATA_KEY", "bookclub.json"),
		endpoint:   os.Getenv("DATA_ENDPOINT"),
		iamToken:   os.Getenv("YC_IAM_TOKEN"),
		botToken:   os.Getenv("BOT_TOKEN"),
		chatID:     os.Getenv("CLUB_CHAT_ID"),
		tgAPI:      strings.TrimSpace(os.Getenv("TELEGRAM_API_URL")),
		appURL:     strings.TrimSpace(os.Getenv("MINIAPP_URL")),
		publicURL:  strings.TrimSpace(os.Getenv("PUBLIC_URL")),
		diskToken:  strings.TrimSpace(os.Getenv("YADISK_TOKEN")),
		diskDir:    env("YADISK_DIR", "app:/covers"),
		coversDir:  os.Getenv("COVERS_DIR"),
		devMode:    os.Getenv("DEV_MODE") == "1",
		initMaxAge: 24 * time.Hour,
	}

	if c.botToken == "" && !c.devMode {
		return c, errors.New("BOT_TOKEN is required (or DEV_MODE=1 for local testing)")
	}
	for _, v := range strings.Split(os.Getenv("CLUB_CHAT_INSTANCE"), ",") {
		if v = strings.TrimSpace(v); v != "" {
			c.clubChats = append(c.clubChats, v)
		}
	}

	ids, err := parseIDs(os.Getenv("ADMIN_IDS"))
	if err != nil {
		return c, fmt.Errorf("ADMIN_IDS: %w", err)
	}
	c.adminIDs = ids

	if c.chatID != "" && c.botToken == "" {
		return c, errors.New("CLUB_CHAT_ID needs BOT_TOKEN to check membership")
	}

	// Bot posts are opt-in: from Yandex Cloud api.telegram.org is usually unreachable, and each post
	// would then hold the member's request for the whole timeout. Members share to the group from the
	// app instead ("Поделиться"), which goes through their own Telegram and needs no Bot API.
	switch os.Getenv("NOTIFY_GROUP") {
	case "", "0":
		c.notify = false
	case "1":
		if c.chatID == "" {
			return c, errors.New("NOTIFY_GROUP=1 needs CLUB_CHAT_ID: the group to post to")
		}
		c.notify = true
	default:
		return c, errors.New("NOTIFY_GROUP must be 0 or 1")
	}
	if c.tgAPI != "" {
		u, err := url.Parse(c.tgAPI)
		if err != nil || u.Scheme != "https" || u.Host == "" || u.RawQuery != "" {
			return c, errors.New("TELEGRAM_API_URL must be an https URL without a query, e.g. https://proxy.example.com/<secret>")
		}
	}
	if c.appURL != "" {
		u, err := url.Parse(c.appURL)
		if err != nil || u.Scheme != "https" || u.Host == "" {
			return c, errors.New("MINIAPP_URL must look like https://t.me/<bot>/<app>")
		}
	}

	if c.publicURL != "" {
		u, err := url.Parse(c.publicURL)
		if err != nil || u.Scheme != "https" || u.Host == "" || u.RawQuery != "" || strings.Trim(u.Path, "/") != "" {
			return c, errors.New("PUBLIC_URL must be the server's https address without a path, e.g. https://bba1234.containers.yandexcloud.net")
		}
	}

	if v := os.Getenv("INITDATA_MAX_AGE"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return c, fmt.Errorf("INITDATA_MAX_AGE: %w", err)
		}
		c.initMaxAge = d
	}
	return c, nil
}

// parseIDs reads Telegram user IDs separated by commas, semicolons or spaces: "111,222" or "111 222".
// Spaces matter for Yandex Cloud, where the deploy script passes the whole environment as one
// comma-separated flag and a comma inside a value would split it.
func parseIDs(raw string) ([]int64, error) {
	var ids []int64
	for _, part := range strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == ';' || unicode.IsSpace(r) }) {
		id, err := strconv.ParseInt(part, 10, 64)
		if err != nil || id <= 0 {
			return nil, fmt.Errorf("%q is not a Telegram user ID", part)
		}
		ids = append(ids, id)
	}
	return ids, nil
}

// listenAddr: ADDR wins; otherwise PORT, which Yandex Serverless Containers set for the app.
func listenAddr() string {
	if v := os.Getenv("ADDR"); v != "" {
		return v
	}
	if p := os.Getenv("PORT"); p != "" {
		return ":" + p
	}
	return ":8080"
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func main() {
	log := slog.New(slog.NewTextHandler(os.Stdout, nil))
	if err := run(log); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}

	st, where, err := openStore(cfg, log)
	if err != nil {
		return err
	}
	coverStore, coversWhere, err := openCovers(cfg)
	if err != nil {
		return err
	}
	if coverStore == nil {
		log.Info("cover photos are off: set YADISK_TOKEN (Yandex Disk) or COVERS_DIR (local folder)")
	} else {
		log.Info("cover photos", "where", coversWhere)
	}

	var members api.MemberChecker
	if cfg.chatID != "" {
		members = tg.NewMemberChecker(cfg.tgAPI, cfg.botToken, cfg.chatID)
	}
	switch {
	case len(cfg.clubChats) > 0 && members != nil:
		log.Info("club access: opened from the club's group, or getChatMember")
	case len(cfg.clubChats) > 0:
		log.Info("club access: only when opened from the club's group (CLUB_CHAT_INSTANCE)")
	case members != nil:
		log.Info("club access: getChatMember (needs api.telegram.org)")
	default:
		log.Warn("neither CLUB_CHAT_INSTANCE nor CLUB_CHAT_ID is set: anyone who opens the Mini App gets access. " +
			"Open the app from the club's group and look for \"opened from chat\" in the log to find CLUB_CHAT_INSTANCE")
	}
	var announcer api.Announcer
	if cfg.notify {
		announcer = tg.NewAnnouncer(cfg.tgAPI, cfg.botToken, cfg.chatID)
		if cfg.appURL == "" {
			log.Warn("MINIAPP_URL is not set: group posts go out without the \"open\" button")
		}
	}
	if len(cfg.adminIDs) == 0 {
		log.Info("no administrators (ADMIN_IDS is empty): every member may schedule meetings, " +
			"books and reviews are deleted only by their authors")
	} else {
		log.Info("administrators", "ids", cfg.adminIDs)
	}
	if cfg.devMode {
		log.Warn("DEV_MODE=1: requests can impersonate any user. Never run like this in production")
	}

	srv := api.New(api.Config{
		Store:             st,
		Auth:              auth.NewValidator(cfg.botToken, cfg.initMaxAge),
		Members:           members,
		Covers:            coverStore,
		ClubChatInstances: cfg.clubChats,
		Announcer:         announcer,
		AppURL:            cfg.appURL,
		PublicURL:         cfg.publicURL,
		ShareKey:          shareKey(cfg.botToken),
		AdminIDs:          cfg.adminIDs,
		DevMode:           cfg.devMode,
		BotID:             botID(cfg.botToken),
		Static:            web.FS(),
		Logger:            log,
	})

	httpSrv := &http.Server{
		Addr:              cfg.addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       90 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", cfg.addr, "data", where, "group_posts", cfg.notify)
		errCh <- httpSrv.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
	}

	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return httpSrv.Shutdown(shutdownCtx)
}

func openStore(cfg config, log *slog.Logger) (*store.Store, string, error) {
	if cfg.bucket != "" {
		var token store.TokenSource = &store.MetadataToken{}
		if cfg.iamToken != "" {
			token = store.StaticToken(cfg.iamToken)
		}
		st, err := store.OpenBucket(store.BucketConfig{Bucket: cfg.bucket, Key: cfg.bucketKey, Endpoint: cfg.endpoint, Token: token, Logger: log})
		return st, "bucket " + cfg.bucket + "/" + cfg.bucketKey, err
	}

	if err := os.MkdirAll(filepath.Dir(cfg.dataFile), 0o700); err != nil {
		return nil, "", fmt.Errorf("create data dir: %w", err)
	}
	st, err := store.OpenFile(cfg.dataFile)
	return st, cfg.dataFile, err
}

// coverCacheBytes: covers kept in memory per instance (out of 128 MB), so each one is fetched
// from Yandex Disk once rather than on every view.
const coverCacheBytes = 16 << 20

// openCovers picks where cover photos live: Yandex Disk if YADISK_TOKEN is set, else COVERS_DIR.
// Neither: nil, the app works without photos.
func openCovers(cfg config) (api.CoverStore, string, error) {
	var (
		b     covers.Backend
		where string
	)
	switch {
	case cfg.diskToken != "":
		d, err := covers.NewDisk(covers.DiskConfig{Token: cfg.diskToken, Dir: cfg.diskDir})
		if err != nil {
			return nil, "", fmt.Errorf("YADISK_DIR: %w", err)
		}
		b, where = d, "Yandex Disk "+cfg.diskDir
	case cfg.coversDir != "":
		d, err := covers.NewDir(cfg.coversDir)
		if err != nil {
			return nil, "", err
		}
		b, where = d, cfg.coversDir
	default:
		return nil, "", nil
	}
	return covers.NewCached(b, coverCacheBytes), where, nil
}

// shareKey signs public links to shared reviews. Derived from the bot token, so it is the same on
// every instance and survives restarts without one more secret; hashing keeps the token itself
// out of the HMAC. Rotating the token invalidates old links, which only drops their previews.
func shareKey(botToken string) []byte {
	if botToken == "" {
		return nil
	}
	sum := sha256.Sum256([]byte("bookclub share links\x00" + botToken))
	return sum[:]
}

// botID is the public numeric part of a bot token ("123456:ABC" -> "123456").
func botID(token string) string {
	id, _, _ := strings.Cut(token, ":")
	return id
}
