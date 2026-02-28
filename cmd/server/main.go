package main

import (
	"flag"
	"io/fs"
	"log"
	"os"

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
	server := api.NewServer(db, sessions, tasks, thumbs, *vaultDir, staticFS)
	router := server.SetupRouter()

	log.Printf("Cryp server starting on :%s", *port)
	log.Printf("  Data dir: %s", *dataDir)
	log.Printf("  Vault dir: %s", *vaultDir)

	if err := router.Run(":" + *port); err != nil {
		log.Fatalf("Failed to start server: %v", err)
	}
}
