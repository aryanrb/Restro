// Command api is the restaurant POS backend: Connect handlers + a WebSocket hub,
// backed by PostgreSQL. Migrations auto-apply on boot.
package main

import (
	"context"
	"errors"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"github.com/oneaccord/restro/api/internal/auth"
	"github.com/oneaccord/restro/api/internal/config"
	"github.com/oneaccord/restro/api/internal/db"
	"github.com/oneaccord/restro/api/internal/realtime"
	"github.com/oneaccord/restro/api/internal/server"
)

func main() {
	log, _ := zap.NewProduction()
	defer func() { _ = log.Sync() }()

	cfg, err := config.Load()
	if err != nil {
		log.Fatal("config", zap.Error(err))
	}

	ctx := context.Background()
	pool, err := db.New(ctx, cfg.DatabaseURL)
	if err != nil {
		log.Fatal("db connect", zap.Error(err))
	}
	defer pool.Close()

	if err := db.RunMigrations(ctx, pool); err != nil {
		log.Fatal("migrate", zap.Error(err))
	}

	seedOwner(ctx, pool, log)

	hub := realtime.NewHub()
	e := server.New(server.Deps{Cfg: cfg, Log: log, Pool: pool, Hub: hub})

	go func() {
		if err := e.Start(":" + cfg.Port); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatal("serve", zap.Error(err))
		}
	}()
	log.Info("api started", zap.String("port", cfg.Port), zap.String("env", cfg.Env))

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := e.Shutdown(shutdownCtx); err != nil {
		log.Error("shutdown", zap.Error(err))
	}
	log.Info("api stopped")
}

// seedOwner bootstraps the first owner from env vars when the DB has no users yet.
// Lets a fresh deployment be logged into before any staff exist. No-op otherwise.
func seedOwner(ctx context.Context, pool *pgxpool.Pool, log *zap.Logger) {
	username := os.Getenv("SEED_OWNER_USERNAME")
	password := os.Getenv("SEED_OWNER_PASSWORD")
	if username == "" || password == "" {
		return
	}
	name := os.Getenv("SEED_OWNER_NAME")
	if name == "" {
		name = "Owner"
	}
	pin := os.Getenv("SEED_OWNER_PIN")
	if pin == "" {
		pin = "0000"
	}
	created, err := auth.NewService(pool, log).SeedOwner(ctx, name, username, password, pin)
	if err != nil {
		log.Error("seed owner", zap.Error(err))
		return
	}
	if created {
		log.Info("seeded initial owner", zap.String("username", username))
	}
}
