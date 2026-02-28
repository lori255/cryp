package api

import (
	"io/fs"
	"net/http"
	"strings"

	"cryp/internal/session"
	"cryp/internal/storage"
	"cryp/internal/task"
	"cryp/internal/thumbnail"

	"github.com/gin-gonic/gin"
)

// Server holds all dependencies for the API
type Server struct {
	db        *storage.DB
	sessions  *session.Store
	tasks     *task.Manager
	thumbs    *thumbnail.Generator
	vaultDir  string
	staticFS  fs.FS
	scryptSem chan struct{} // limits concurrent scrypt derivations to prevent OOM
}

func NewServer(db *storage.DB, sessions *session.Store, tasks *task.Manager, thumbs *thumbnail.Generator, vaultDir string, staticFS fs.FS) *Server {
	return &Server{
		db:        db,
		sessions:  sessions,
		tasks:     tasks,
		thumbs:    thumbs,
		vaultDir:  vaultDir,
		staticFS:  staticFS,
		scryptSem: make(chan struct{}, 2), // max 2 concurrent scrypt ops (~64MB peak)
	}
}

// SetupRouter configures all routes
func (s *Server) SetupRouter() *gin.Engine {
	r := gin.Default()
	r.MaxMultipartMemory = 8 << 20 // 8MB — excess spills to temp files on disk

	// CORS middleware — restrict to same origin (not wildcard)
	r.Use(func(c *gin.Context) {
		origin := c.GetHeader("Origin")
		if origin != "" {
			c.Header("Access-Control-Allow-Origin", origin)
			c.Header("Access-Control-Allow-Credentials", "true")
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
		// Directory browsing (requires session for security)
		api.GET("/browse-dir", s.handleBrowseDir)
		// Vault operations - session required
		vaultOps := api.Group("/vaults/:id")
		vaultOps.Use(s.authMiddleware())
		{
			vaultOps.GET("/files", s.handleListFiles)
			vaultOps.GET("/files/content", s.handleFileContent)
			vaultOps.POST("/files/upload", s.handleUploadFile)
			vaultOps.POST("/files/mkdir", s.handleMkdir)
			vaultOps.DELETE("/files", s.handleDeleteFile)
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
				// Debug: log the error
				c.String(http.StatusInternalServerError, "embed error: %v", err)
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
		c.Next()
	}
}

func getSession(c *gin.Context) *session.Session {
	sess, _ := c.Get("session")
	return sess.(*session.Session)
}
