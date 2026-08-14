package main

import (
	"fmt"
	"log/slog"
	"os"

	"github.com/mikeyaustin/jlp/internal/config"
	httpx "github.com/mikeyaustin/jlp/internal/adapters/http"
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
		srv := httpx.NewServer(httpx.Options{Addr: fmt.Sprintf(":%d", cfg.Server.Port)})
		slog.Info("listening", "port", cfg.Server.Port)
		if err := srv.ListenAndServe(); err != nil {
			slog.Error("server exited", "err", err)
			os.Exit(1)
		}
	default:
		slog.Error("unknown command", "cmd", cmd)
		os.Exit(2)
	}
}
