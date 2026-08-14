package main

import (
	"log/slog"
	"os"

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
		srv := httpx.NewServer(httpx.Options{Addr: ":8080"})
		slog.Info("listening", "addr", ":8080")
		if err := srv.ListenAndServe(); err != nil {
			slog.Error("server exited", "err", err)
			os.Exit(1)
		}
	default:
		slog.Error("unknown command", "cmd", cmd)
		os.Exit(2)
	}
}
