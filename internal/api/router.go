package api

import (
	"io/fs"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"

	"cryp/internal/pathguard"
	"cryp/internal/session"
	"cryp/internal/storage"
	"cryp/internal/task"
	"cryp/internal/thumbnail"

	"github.com/gin-gonic/gin"
)

// Server holds all dependencies for the API
type Server struct {
	db          *storage.DB
	sessions    *session.Store
	tasks       *task.Manager
	thumbs      *thumbnail.Generator
	vaultDir    string
	sourceRoot  string
	sourceGuard *pathguard.Guard
	port        string
	staticFS    fs.FS
	scryptSem   chan struct{} // limits concurrent scrypt derivations to prevent OOM
	corsAllow   map[string]struct{}
	hlsLifeMu   sync.RWMutex // coordinates HLS starts, mutations, and shutdown
	hlsMu       sync.Mutex
	hls         map[string]*hlsStream
	hlsPending  map[hlsKey]*hlsPending
	hlsActive   int
	hlsStarts   int
	hlsClosing  bool
}

func NewServer(db *storage.DB, sessions *session.Store, tasks *task.Manager, thumbs *thumbnail.Generator, vaultDir string, staticFS fs.FS) *Server {
	return NewServerWithPortAndSourceRoot(db, sessions, tasks, thumbs, vaultDir, serverPortFromEnv(), sourceRootFromEnv(), staticFS, dataRootFromEnv(), vaultDir)
}

// NewServerWithPort creates an API server and uses port for internal loopback
// requests made by FFmpeg. Keeping this value alongside the listener avoids
// failures when the -port flag is used without PORT in the environment.
func NewServerWithPort(db *storage.DB, sessions *session.Store, tasks *task.Manager, thumbs *thumbnail.Generator, vaultDir, port string, staticFS fs.FS) *Server {
	return NewServerWithPortAndSourceRoot(db, sessions, tasks, thumbs, vaultDir, port, sourceRootFromEnv(), staticFS, dataRootFromEnv(), vaultDir)
}

// NewServerWithPortAndSourceRoot is the fully-configurable constructor. The
// source guard is shared with the task manager so HTTP validation and the
// asynchronous worker enforce one policy.
func NewServerWithPortAndSourceRoot(db *storage.DB, sessions *session.Store, tasks *task.Manager, thumbs *thumbnail.Generator, vaultDir, port, sourceRoot string, staticFS fs.FS, reservedRoots ...string) *Server {
	if strings.TrimSpace(port) == "" {
		port = serverPortFromEnv()
	}
	sourceRoot = strings.TrimSpace(sourceRoot)
	if sourceRoot == "" {
		sourceRoot = sourceRootFromEnv()
	}
	sourceGuard, guardErr := pathguard.NewWithReserved(sourceRoot, reservedRoots...)
	if guardErr != nil {
		// Keep the server constructible for tests and let the endpoint return a
		// clear configuration error. The production entrypoint validates the
		// directory before constructing the server.
		log.Printf("source path guard unavailable for %q: %v", sourceRoot, guardErr)
	}
	server := &Server{
		db:          db,
		sessions:    sessions,
		tasks:       tasks,
		thumbs:      thumbs,
		vaultDir:    vaultDir,
		sourceRoot:  sourceRoot,
		sourceGuard: sourceGuard,
		port:        port,
		staticFS:    staticFS,
		scryptSem:   make(chan struct{}, 2), // max 2 concurrent scrypt ops (~64MB peak)
		corsAllow:   parseAllowedOrigins(os.Getenv("CRYP_ALLOWED_ORIGINS")),
		hls:         make(map[string]*hlsStream),
		hlsPending:  make(map[hlsKey]*hlsPending),
	}
	if tasks != nil {
		tasks.SetReplaceGuard(server.PrepareFileReplacement)
		tasks.SetReplaceLeaseGuard(server.BeginFileReplacement)
		tasks.SetImportSourceGuard(sourceGuard)
	}
	return server
}

func sourceRootFromEnv() string {
	if root := strings.TrimSpace(os.Getenv("SOURCE_DIR")); root != "" {
		return root
	}
	if root := strings.TrimSpace(os.Getenv("BROWSE_ROOT")); root != "" {
		return root
	}
	return "/data"
}

func dataRootFromEnv() string {
	if root := strings.TrimSpace(os.Getenv("DATA_DIR")); root != "" {
		return root
	}
	return "/data/config"
}

func serverPortFromEnv() string {
	if port := strings.TrimSpace(os.Getenv("PORT")); port != "" {
		return port
	}
	return "8080"
}

func parseAllowedOrigins(raw string) map[string]struct{} {
	out := make(map[string]struct{})
	for _, p := range strings.Split(raw, ",") {
		v := strings.TrimSpace(p)
		if v == "" {
			continue
		}
		out[v] = struct{}{}
	}
	return out
}

func (s *Server) isAllowedOrigin(origin string) bool {
	_, ok := s.corsAllow[origin]
	return ok
}

// SetupRouter configures all routes
func (s *Server) SetupRouter() *gin.Engine {
	r := gin.New()
	r.Use(gin.Recovery())
	r.MaxMultipartMemory = 8 << 20 // 8MB — excess spills to temp files on disk

	// CORS middleware — restrict to same origin (not wildcard)
	r.Use(func(c *gin.Context) {
		origin := c.GetHeader("Origin")
		if origin != "" {
			if s.isAllowedOrigin(origin) {
				c.Header("Access-Control-Allow-Origin", origin)
				c.Header("Access-Control-Allow-Credentials", "true")
				c.Header("Vary", "Origin")
			} else if c.Request.Method == "OPTIONS" {
				c.AbortWithStatus(http.StatusForbidden)
				return
			}
		}
		c.Header("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		c.Header("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Session-ID")
		c.Header("Access-Control-Expose-Headers", "Content-Length, Content-Range, Accept-Ranges")
		if c.Request.Method == "OPTIONS" {
			c.AbortWithStatus(http.StatusNoContent)
			return
		}
		c.Next()
	})

	// API routes
	api := r.Group("/api")
	{
		// Auth - no session required
		api.POST("/auth/login", s.handleLogin)
		api.POST("/auth/logout", s.handleLogout)
		api.GET("/auth/status", s.handleAuthStatus)

		// Vault management - no session required (manages vault lifecycle)
		api.POST("/vaults", s.handleCreateVault)
		api.GET("/vaults", s.handleListVaults)
		authOps := api.Group("")
		authOps.Use(s.authMiddleware())
		authOps.GET("/browse-dir", s.handleBrowseDir)

		// Vault operations - session required
		vaultOps := api.Group("/vaults/:id")
		vaultOps.Use(s.authMiddleware())
		{
			vaultOps.GET("/files", s.handleListFiles)
			vaultOps.GET("/files/content", s.handleFileContent)
			vaultOps.GET("/files/download", s.handleDownloadFile)
			vaultOps.GET("/files/hls", s.handleHLSStart)
			vaultOps.POST("/files/hls/stop", s.handleHLSStop)
			vaultOps.POST("/files/hls/:stream/stop", s.handleHLSStop)
			vaultOps.GET("/files/hls/:stream/*name", s.handleHLSAsset)
			vaultOps.POST("/files/upload", s.handleUploadFile)
			vaultOps.POST("/files/mkdir", s.handleMkdir)
			vaultOps.DELETE("/files", s.handleDeleteFile)
			vaultOps.POST("/files/delete-batch", s.handleDeleteFilesBatch)
			vaultOps.GET("/files/duplicates", s.handleListDuplicates)
			vaultOps.POST("/files/index/rebuild", s.handleRebuildFileIndex)
			vaultOps.GET("/thumbnail", s.handleThumbnail)
			vaultOps.DELETE("", s.handleDeleteVault) // vault deletion requires auth

			// Task management
			vaultOps.GET("/tasks", s.handleListTasks)
			vaultOps.POST("/tasks/import", s.handleCreateImportTask)
			vaultOps.POST("/tasks/upload", s.handleCreateUploadTask)
			vaultOps.GET("/tasks/:taskId", s.handleGetTask)
			vaultOps.POST("/tasks/:taskId/cancel", s.handleCancelTask)
			vaultOps.DELETE("/tasks/completed", s.handleDeleteCompletedTasks)
			vaultOps.DELETE("/tasks/:taskId", s.handleDeleteTask)
		}
	}
	// Serve React static files
	if s.staticFS != nil {
		// Serve static assets (js, css, etc.)
		fileServer := http.FileServer(http.FS(s.staticFS))
		r.GET("/assets/*filepath", gin.WrapH(fileServer))
		r.GET("/vite.svg", gin.WrapH(fileServer))
	}

	r.NoRoute(func(c *gin.Context) {
		path := c.Request.URL.Path
		// API routes that weren't matched
		if strings.HasPrefix(path, "/api/") {
			c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
			return
		}
		// SPA fallback: serve index.html
		if s.staticFS != nil {
			data, err := fs.ReadFile(s.staticFS, "index.html")
			if err != nil {
				log.Printf("spa fallback: read index: %v", err)
				c.String(http.StatusInternalServerError, "failed to load application")
				return
			}
			c.Data(http.StatusOK, "text/html; charset=utf-8", data)
			return
		}
		c.String(http.StatusNotFound, "not found")
	})

	return r
}

// authMiddleware checks for a valid session
func (s *Server) authMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		sessionID := c.GetHeader("X-Session-ID")
		if sessionID == "" {
			// Use httpOnly cookie (secure fallback for img/video elements)
			if cookie, err := c.Cookie("session_id"); err == nil {
				sessionID = cookie
			}
		}

		if sessionID == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "session required"})
			c.Abort()
			return
		}

		sess, ok := s.sessions.Get(sessionID)
		if !ok {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid or expired session"})
			c.Abort()
			return
		}

		// Verify vault ID matches
		vaultID := c.Param("id")
		if vaultID != "" && sess.VaultID != vaultID {
			c.JSON(http.StatusForbidden, gin.H{"error": "session not for this vault"})
			c.Abort()
			return
		}

		// Refresh session
		s.sessions.Refresh(sessionID)

		// Store session in context
		c.Set("session", sess)
		c.Set("sessionID", sessionID)
		// Store.Get returns an owned snapshot. Handlers clone any keys that
		// escape the request; wipe this snapshot as soon as the middleware
		// chain returns.
		defer sess.Keys.Zero()
		c.Next()
	}
}

func getSession(c *gin.Context) *session.Session {
	sess, _ := c.Get("session")
	return sess.(*session.Session)
}

func (s *Server) requestSessionActive(c *gin.Context) bool {
	if s.sessions == nil {
		return false
	}
	value, ok := c.Get("sessionID")
	if !ok {
		return false
	}
	sessionID, ok := value.(string)
	return ok && s.sessions.Has(sessionID)
}
