// Command server is the agent-queue HTTP server.
package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/irchelper/agent-queue/internal/db"
	"github.com/irchelper/agent-queue/internal/handler"
	"github.com/irchelper/agent-queue/internal/notify"
	"github.com/irchelper/agent-queue/internal/openclaw"
	"github.com/irchelper/agent-queue/internal/store"
)

func main() {
	var (
		port   = flag.String("port", envOr("AGENT_QUEUE_PORT", "19827"), "listen port")
		dbPath = flag.String("db", "data/queue.db", "path to SQLite database file")
	)
	flag.Parse()

	// Ensure data directory exists.
	if err := os.MkdirAll("data", 0o755); err != nil {
		log.Fatalf("create data dir: %v", err)
	}

	// Open database.
	database, err := db.Open(*dbPath)
	if err != nil {
		log.Fatalf("open database: %v", err)
	}
	defer database.Close()

	// Build dependencies.
	s := store.New(database)
	oc := openclaw.NewFromEnv()

	// Build notifier: Discord webhook if configured.
	// SessionNotifier (CEO alerts on unhandled failures) is wired inside handler.New.
	n := notify.NewFromEnv()
	h := handler.New(database, s, n, oc)

	mux := http.NewServeMux()
	h.Register(mux)

	addr := "localhost:" + *port
	srv := &http.Server{
		Addr:         addr,
		Handler:      mux,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 15 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	// Graceful shutdown.
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		log.Printf("agent-queue listening on http://%s", addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("listen: %v", err)
		}
	}()

	<-quit
	log.Println("shutting down...")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Fatalf("shutdown: %v", err)
	}
	log.Println("stopped")
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
