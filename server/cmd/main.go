package main

import (
	"context"
	"crypto/rand"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/tomanagle/harness-kit/server/internal/auth"
	"github.com/tomanagle/harness-kit/server/internal/config"
	"github.com/tomanagle/harness-kit/server/internal/database"
	"github.com/tomanagle/harness-kit/server/internal/email"
	"github.com/tomanagle/harness-kit/server/internal/handlers"
	"github.com/tomanagle/harness-kit/server/internal/httpserver"
	"github.com/tomanagle/harness-kit/server/internal/logger"
	"github.com/tomanagle/harness-kit/server/internal/maintenance"
	"github.com/tomanagle/harness-kit/server/internal/store/dbstore"
	"github.com/tomanagle/harness-kit/server/migrations"
)

// main is the composition root and the only place that calls Must constructors.
// Setup failures panic here; operational errors are logged and exit nonzero.
func main() {
	role := database.Runtime
	if len(os.Args) == 2 && os.Args[1] == "migrate" {
		role = database.Migrator
	} else if len(os.Args) != 1 && !(len(os.Args) == 2 && os.Args[1] == "cleanup") {
		fmt.Fprintln(os.Stderr, "usage: harness-kit-server [migrate|cleanup]")
		os.Exit(2)
	}

	cfg := config.MustLoad(os.Getenv)
	log := logger.MustNew(os.Stderr, cfg.Logging)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	db := database.MustOpen(ctx, cfg.Database, role)
	defer db.Close()

	mailStop := func() {}
	defer func() { mailStop() }()
	var err error
	if len(os.Args) == 2 && os.Args[1] == "cleanup" {
		err = maintenance.Run(ctx, db)
	} else if role == database.Migrator {
		err = migrations.Apply(ctx, db)
	} else {
		if cfg.Mail.Address != "" {
			sender := email.MustNewSMTP(cfg.Mail)
			mailCtx, cancel := context.WithCancel(ctx)
			done := make(chan struct{})
			mailStop = func() { cancel(); <-done }
			go func() {
				defer close(done)
				for mailCtx.Err() == nil {
					_, err := maintenance.DeliverOne(mailCtx, db, sender)
					if err != nil && mailCtx.Err() == nil {
						log.Error("mail delivery", "error", err)
					}
					select {
					case <-mailCtx.Done():
						return
					case <-time.After(time.Second):
					}
				}
			}()
		}
		passwords := auth.MustNewPasswords(rand.Reader)
		tokens := auth.MustNewTokens(rand.Reader)
		repository := dbstore.MustNew(dbstore.Options{
			Database:             db,
			RequireVerifiedEmail: cfg.RequireVerifiedEmail,
			MailEnabled:          cfg.Mail.Address != "",
			PublicURL:            cfg.PublicURL,
			Passwords:            passwords,
			Tokens:               tokens,
			Now:                  time.Now,
		})
		router := handlers.MustNew(handlers.Options{
			Store:                 repository,
			Logger:                log,
			PublicURL:             cfg.PublicURL,
			Registration:          cfg.Registration,
			AuthConcurrency:       4,
			AuthRequestsPerMinute: cfg.AuthRequestsPerMinute,
			TrustedProxies:        cfg.TrustedProxies,
		})
		server := httpserver.MustNew(httpserver.Options{
			Config:  cfg.HTTP,
			Handler: router,
			Logger:  log,
		})
		listener := httpserver.MustListen(cfg.Address)
		err = server.Serve(ctx, listener)
	}
	if err != nil {
		log.Error("server stopped", "error", err)
		// os.Exit does not run defers. Release owned resources before an error exit.
		mailStop()
		db.Close()
		stop()
		os.Exit(1)
	}
}
