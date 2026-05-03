package main

import (
	"fmt"
	"log/slog"
	"os"

	"gioui.org/app"

	"github.com/user/wlslack/internal/config"
	"github.com/user/wlslack/internal/logger"
	"github.com/user/wlslack/internal/slack"
	"github.com/user/wlslack/internal/ui"
)

func main() {
	cleanup, err := logger.Init()
	if err != nil {
		fmt.Fprintf(os.Stderr, "logger init: %v\n", err)
	} else {
		defer cleanup()
	}

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "config error: %v\n", err)
		os.Exit(1)
	}

	client, err := slack.NewClient(cfg.Token, cfg.Cookie)
	if err != nil {
		fmt.Fprintf(os.Stderr, "slack client error: %v\n", err)
		os.Exit(1)
	}

	selfID, err := client.AuthTest()
	if err != nil {
		fmt.Fprintf(os.Stderr, "auth error: %v\n", err)
		os.Exit(1)
	}
	client.SetSelfID(selfID)
	slog.Info("authenticated", "self_id", selfID)

	// Probe files.list once so a missing files:read scope (the most common
	// reason file URLs 302 to the workspace login) is reported up front
	// instead of buried in image-loader debug logs.
	if err := client.VerifyFileAccess(); err != nil {
		slog.Warn("file access probe failed; image fetches will likely fail", "error", err)
		fmt.Fprintf(os.Stderr, "warning: %v\n", err)
	}

	go func() {
		if err := ui.Run(client, cfg); err != nil {
			fmt.Fprintf(os.Stderr, "ui error: %v\n", err)
			os.Exit(1)
		}
		os.Exit(0)
	}()
	app.Main()
}
