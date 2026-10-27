// Command bookclub serves the book club Telegram Mini App: the static front end and its JSON API.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"bookclub/internal/api"
	"bookclub/internal/auth"
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
		devMode:    os.Getenv("DEV_MODE") == "1",
		initMaxAge: 24 * time.Hour,
	}

	if c.botToken == "" && !c.devMode {
		return c, errors.New("BOT_TOKEN is required (or DEV_MODE=1 for local testing)")
	}
	if c.chatID != "" && c.botToken == "" {
		return c, errors.New("CLUB_CHAT_ID needs BOT_TOKEN to check membership")
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

	var members api.MemberChecker
	if cfg.chatID != "" {
		members = tg.NewMemberChecker(cfg.botToken, cfg.chatID)
	} else {
		log.Warn("CLUB_CHAT_ID is not set: anyone who opens the Mini App gets access")
	}
	if cfg.devMode {
		log.Warn("DEV_MODE=1: requests can impersonate any user. Never run like this in production")
	}

	srv := api.New(api.Config{
		Store:   st,
		Auth:    auth.NewValidator(cfg.botToken, cfg.initMaxAge),
		Members: members,
		DevMode: cfg.devMode,
		Static:  web.FS(),
		Logger:  log,
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
		log.Info("listening", "addr", cfg.addr, "data", where)
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
