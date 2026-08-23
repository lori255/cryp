package main

import (
	"context"
	"errors"
	"flag"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"cryp/internal/api"
	"cryp/internal/session"
	"cryp/internal/storage"
	"cryp/internal/task"
	"cryp/internal/thumbnail"

	"github.com/gin-gonic/gin"
)

func main() {
	port := flag.String("port", "8080", "server port")
	dataDir := flag.String("data", "/data/config", "data directory for config/DB")
	vaultDir := flag.String("vaults", "/data/vaults", "directory for encrypted vaults")
	flag.Parse()

	// Override with env vars
	if p := os.Getenv("PORT"); p != "" {
		*port = p
	}
	if d := os.Getenv("DATA_DIR"); d != "" {
		*dataDir = d
	}
	if v := os.Getenv("VAULT_DIR"); v != "" {
		*vaultDir = v
	}

	// Initialize database
	db, err := storage.NewDB(*dataDir)
	if err != nil {
		log.Fatalf("Failed to initialize database: %v", err)
	}
	defer db.Close()

	// Initialize session store
	sessions := session.NewStore()
	defer sessions.Close()

	// Initialize task manager
	tasks := task.NewManager(db)

	// Initialize thumbnail generator
	thumbs := thumbnail.NewGenerator(*vaultDir, sessions, *port)
	defer thumbs.Stop()

	// Wire thumbnail enqueuer into task manager (avoids circular init)
	tasks.SetThumbEnqueuer(thumbs)

	// Create vault directory
	if err := os.MkdirAll(*vaultDir, 0700); err != nil {
		log.Fatalf("Failed to create vault directory: %v", err)
	}

	// Get embedded static files (nil in dev mode)
	var staticFS fs.FS
	if embedFS := getEmbeddedFS(); embedFS != nil {
		staticFS, _ = fs.Sub(embedFS, "web/dist")
	}

	// Setup API server
	gin.SetMode(gin.ReleaseMode)
	server := api.NewServerWithPort(db, sessions, tasks, thumbs, *vaultDir, *port, staticFS)
	router := server.SetupRouter()

	log.Printf("Cryp server starting on :%s", *port)
	log.Printf("  Data dir: %s", *dataDir)
	log.Printf("  Vault dir: %s", *vaultDir)

	httpServer := &http.Server{
		Addr:    ":" + *port,
		Handler: router,
	}
	serveErr := make(chan error, 1)
	go func() {
		serveErr <- httpServer.ListenAndServe()
	}()

	// Stop accepting requests and cancel every HLS process on a normal
	// container/OS shutdown. HLS FFmpeg commands run in independent process
	// groups, so merely returning from main would otherwise orphan them and
	// leave GPU work and temporary files behind.
	shutdown := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		hlsDone := make(chan error, 1)
		go func() { hlsDone <- server.Shutdown(ctx) }()
		if err := httpServer.Shutdown(ctx); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("HTTP graceful shutdown failed: %v", err)
			// Force-close connections that ignored the bounded graceful window;
			// otherwise an upload/long response could keep the process alive
			// while dependent resources are being torn down.
			_ = httpServer.Close()
		}
		if err := <-hlsDone; err != nil {
			log.Printf("HLS graceful shutdown incomplete: %v", err)
		}
	}

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)
	select {
	case sig := <-signals:
		log.Printf("Received %s, shutting down", sig)
		shutdown()
	case err := <-serveErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("Failed to serve HTTP: %v", err)
			shutdown()
		}
	}
}
