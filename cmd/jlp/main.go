package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"github.com/mikeyaustin/jlp/internal/adapters/authelia"
	httpx "github.com/mikeyaustin/jlp/internal/adapters/http"
	"github.com/mikeyaustin/jlp/internal/adapters/inprocbus"
	"github.com/mikeyaustin/jlp/internal/adapters/postgres"
	"github.com/mikeyaustin/jlp/internal/adapters/staticauth"
	"github.com/mikeyaustin/jlp/internal/application/learning"
	"github.com/mikeyaustin/jlp/internal/application/sessions"
	appwriting "github.com/mikeyaustin/jlp/internal/application/writing"
	"github.com/mikeyaustin/jlp/internal/config"
	"github.com/mikeyaustin/jlp/internal/ports/auth"
)

func main() {
	cmd := "serve"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	switch cmd {
	case "serve":
		cfg, err := config.Load()
		if err != nil {
			slog.Error("config", "err", err)
			os.Exit(1)
		}
		pool, err := postgres.NewPool(context.Background(), cfg.Database.URL)
		if err != nil {
			slog.Error("database", "err", err)
			os.Exit(1)
		}
		identities := postgres.NewIdentityRepository(pool)
		sessionsSvc := sessions.NewService(postgres.NewSessionRepository(pool))
		eventRepo := postgres.NewLearningEventRepository(pool)
		bus := inprocbus.New()
		recorder := learning.NewRecorder(eventRepo, bus)
		writingSvc := appwriting.NewService(postgres.NewDocumentRepository(pool), recorder)
		var authn auth.Authenticator
		switch cfg.Auth.Mode {
		case "static":
			authn = staticauth.New(cfg.Auth.Static.ID, cfg.Auth.Static.DisplayName)
		case "authelia":
			authn, err = authelia.New(cfg.Auth.TrustedProxies)
			if err != nil {
				slog.Error("auth", "err", err)
				os.Exit(1)
			}
		default:
			slog.Error("auth", "err", fmt.Sprintf("unknown auth mode %q", cfg.Auth.Mode))
			os.Exit(1)
		}
		srv := httpx.NewServer(httpx.Options{
			Addr:       fmt.Sprintf(":%d", cfg.Server.Port),
			Auth:       authn,
			Identities: identities,
			Sessions:   sessionsSvc,
			Writing:    writingSvc,
			Events:     eventRepo,
		})
		slog.Info("listening", "port", cfg.Server.Port)
		if err := srv.ListenAndServe(); err != nil {
			slog.Error("server exited", "err", err)
			os.Exit(1)
		}
	case "migrate":
		cfg, err := config.Load()
		if err != nil {
			slog.Error("config", "err", err)
			os.Exit(1)
		}
		if err := postgres.Migrate(context.Background(), cfg.Database.URL); err != nil {
			slog.Error("migrate", "err", err)
			os.Exit(1)
		}
		slog.Info("migrations applied")
	default:
		slog.Error("unknown command", "cmd", cmd)
		os.Exit(2)
	}
}
