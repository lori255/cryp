package api

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"cryp/internal/crypto"
	"cryp/internal/pathguard"
	"cryp/internal/storage"
	"cryp/internal/task"

	"github.com/gin-gonic/gin"
)

type loginRequest struct {
	Name     string `json:"name" binding:"required"`
	Password string `json:"password" binding:"required"`
}

func (s *Server) handleLogin(c *gin.Context) {
	var req loginRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}

	// Get vault record by name
	vault, err := s.db.GetVaultByName(req.Name)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{"error": "vault not found"})
		} else {
			log.Printf("auth: lookup vault %q: %v", req.Name, err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to look up vault"})
		}
		return
	}
	if _, pathErr := pathguard.ValidateVaultPath(s.vaultDir, vault.ID, vault.Path); pathErr != nil {
		log.Printf("auth: refusing unsafe vault path for %s (%q): %v", vault.ID, vault.Path, pathErr)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "vault storage path is invalid"})
		return
	}

	// Load vault config and attempt unlock
	config, err := crypto.LoadVaultConfig(vault.Path)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load vault config"})
		return
	}

	// Limit concurrent scrypt derivations to prevent OOM under load.
	// Each scrypt call uses ~32MB; with GOMEMLIMIT=256MiB we allow max 2.
	select {
	case s.scryptSem <- struct{}{}:
		defer func() { <-s.scryptSem }()
	default:
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "server busy, try again"})
		return
	}

	keys, err := crypto.UnlockVault([]byte(req.Password), config)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "wrong password"})
		return
	}
	// Session and thumbnail/task entrypoints clone the keys they retain. The
	// request-owned unlock result must not remain live after this handler.
	defer keys.Zero()

	// Create session
	sessionID, err := s.sessions.Create(vault.ID, vault.Path, keys)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create session"})
		return
	}

	// Set cookie with SameSite=Strict
	c.SetSameSite(http.SameSiteStrictMode)
	c.SetCookie("session_id", sessionID, 86400, "/", "", false, true)

	// Trigger background thumbnail scan for videos missing thumbnails
	if s.thumbs != nil {
		s.thumbs.ScanVault(vault.ID, vault.Path, keys)
	}

	c.JSON(http.StatusOK, gin.H{
		"sessionId": sessionID,
		"vaultId":   vault.ID,
		"vaultName": vault.Name,
	})
}

func (s *Server) handleLogout(c *gin.Context) {
	sessionID := c.GetHeader("X-Session-ID")
	if sessionID == "" {
		if cookie, err := c.Cookie("session_id"); err == nil {
			sessionID = cookie
		}
	}

	if sessionID != "" {
		if sess, ok := s.sessions.Get(sessionID); ok {
			defer sess.Keys.Zero()
			// Serialize logout with HLS starts and destructive replacements. This
			// closes the window where a new stream could be registered after the
			// owner stop but before its internal auth session is deleted.
			s.hlsLifeMu.Lock()
			// Logout is a common page teardown path. Stop streams owned by this
			// session before deleting its credentials so FFmpeg does not linger
			// until the idle timeout.
			if !s.stopHLSForOwner(sess.VaultID, sessionID) {
				log.Printf("hls: logout requested while stream cleanup is still pending for session %s", sessionID)
			}
			s.sessions.Delete(sessionID)
			s.hlsLifeMu.Unlock()
		} else {
			s.sessions.Delete(sessionID)
		}
	}

	c.SetSameSite(http.SameSiteStrictMode)
	c.SetCookie("session_id", "", -1, "/", "", false, true)
	c.JSON(http.StatusOK, gin.H{"message": "logged out"})
}

func (s *Server) handleAuthStatus(c *gin.Context) {
	sessionID := c.GetHeader("X-Session-ID")
	if sessionID == "" {
		if cookie, err := c.Cookie("session_id"); err == nil {
			sessionID = cookie
		}
	}

	if sessionID == "" {
		c.JSON(http.StatusOK, gin.H{"authenticated": false})
		return
	}

	sess, ok := s.sessions.Get(sessionID)
	if !ok {
		c.JSON(http.StatusOK, gin.H{"authenticated": false})
		return
	}
	defer sess.Keys.Zero()

	vault, err := s.db.GetVault(sess.VaultID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			c.JSON(http.StatusOK, gin.H{"authenticated": false})
		} else {
			log.Printf("auth status: lookup vault %s: %v", sess.VaultID, err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to look up vault"})
		}
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"authenticated": true,
		"sessionId":     sessionID,
		"vaultId":       sess.VaultID,
		"vaultName":     vault.Name,
	})
}

func (s *Server) handleCreateVault(c *gin.Context) {
	var req struct {
		Name     string `json:"name" binding:"required"`
		Password string `json:"password" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "name and password required"})
		return
	}

	// Limit concurrent scrypt derivations
	select {
	case s.scryptSem <- struct{}{}:
		defer func() { <-s.scryptSem }()
	default:
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "server busy, try again"})
		return
	}

	id, err := task.GenerateID()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate vault id"})
		return
	}
	vaultPath := filepath.Join(s.vaultDir, id)

	config, keys, err := crypto.InitVault(vaultPath, []byte(req.Password))
	if err != nil {
		log.Printf("vault %s: initialize: %v", id, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create vault", "code": "vault_initialize_failed"})
		return
	}
	defer keys.Zero()

	record := &storage.VaultRecord{
		ID:   id,
		Name: req.Name,
		Path: vaultPath,
	}
	if err := s.db.CreateVault(record); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to save vault record"})
		return
	}
	if err := s.upsertEntry(keys.MACKey, id, "/", true, true, 0, 0, ""); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to initialize vault index"})
		return
	}

	// Auto-login after creation
	sessionID, err := s.sessions.Create(id, vaultPath, keys)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "vault created but failed to create session"})
		return
	}

	c.SetSameSite(http.SameSiteStrictMode)
	c.SetCookie("session_id", sessionID, 86400, "/", "", false, true)

	_ = config // config is stored on disk
	c.JSON(http.StatusCreated, gin.H{
		"id":        id,
		"name":      req.Name,
		"sessionId": sessionID,
	})
}

func (s *Server) handleListVaults(c *gin.Context) {
	// Don't expose vault names - only return count
	vaults, err := s.db.ListVaults()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list vaults"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"count": len(vaults)})
}

func (s *Server) handleDeleteVault(c *gin.Context) {
	id := c.Param("id")

	// Read optional body for deleteFiles flag
	var req struct {
		DeleteFiles bool `json:"deleteFiles"`
	}
	_ = c.ShouldBindJSON(&req) // optional body, ignore errors

	// Look up vault path before deleting the record
	vault, err := s.db.GetVault(id)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "vault not found"})
		return
	}
	validatedVaultPath, pathErr := pathguard.ValidateVaultPath(s.vaultDir, id, vault.Path)
	if pathErr != nil {
		log.Printf("vault %s: refusing unsafe storage path %q: %v", id, vault.Path, pathErr)
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": "vault storage path is invalid; vault was not deleted",
			"code":  "vault_path_invalid",
		})
		return
	}

	// Quiesce background import/index tasks first. This also blocks a new task
	// from starting in the cancellation-to-delete window.
	deleteCtx, deleteCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer deleteCancel()
	resumeWorkers := func() {
		if s.thumbs != nil {
			s.thumbs.ResumeVault(id)
		}
		if s.tasks != nil {
			s.tasks.ResumeVault(id)
		}
	}
	if s.tasks != nil {
		if err := s.tasks.QuiesceVault(deleteCtx, id); err != nil {
			s.tasks.ResumeVault(id)
			c.Header("Retry-After", "1")
			c.JSON(http.StatusGatewayTimeout, gin.H{"error": "background vault tasks are still stopping; retry vault deletion"})
			return
		}
	}
	if s.thumbs != nil {
		if err := s.thumbs.QuiesceVault(deleteCtx, id); err != nil {
			resumeWorkers()
			c.Header("Retry-After", "1")
			c.JSON(http.StatusGatewayTimeout, gin.H{"error": "thumbnail tasks are still stopping; retry vault deletion"})
			return
		}
	}

	// Prevent new HLS starts while streams are stopped and the vault is
	// removed. Otherwise a start racing this handler could recreate a reader
	// between cleanup and deletion.
	s.hlsLifeMu.Lock()

	// Stop any HLS/FFmpeg work before deleting the vault or its keys. Otherwise
	// a background transcode can keep reading a vault that is being removed and
	// hold GPU/file resources until an eventual authentication failure.
	if !s.stopHLSForVault(id) {
		s.hlsLifeMu.Unlock()
		resumeWorkers()
		c.Header("Retry-After", "1")
		c.JSON(http.StatusGatewayTimeout, gin.H{"error": "active video streams are still stopping; retry vault deletion"})
		return
	}

	// Optionally remove encrypted files from disk
	if req.DeleteFiles {
		// Move the validated directory to an application-generated sibling
		// before recursively deleting it. This closes the most dangerous
		// check-then-RemoveAll window: a replacement at the database-controlled
		// name can no longer redirect the recursive walk outside the vault root.
		quarantinePath, err := pathguard.QuarantineVaultPath(s.vaultDir, id, validatedVaultPath)
		if err != nil {
			log.Printf("vault %s: quarantine files %s: %v", id, validatedVaultPath, err)
			// Keep workers quiesced on any isolation failure. The path may have
			// changed after the initial validation; reopening it here would restore
			// background readers before a retry can re-establish the boundary.
			s.hlsLifeMu.Unlock()
			c.JSON(http.StatusInternalServerError, gin.H{
				"error": "encrypted files could not be isolated; vault was not deleted",
				"code":  "vault_quarantine_failed",
			})
			return
		}
		if quarantinePath != "" {
			if err := os.RemoveAll(quarantinePath); err != nil {
				log.Printf("vault %s: remove quarantined files %s: %v", id, quarantinePath, err)
				// RemoveAll may have completed only part of the tree. Keep all
				// background owners quiesced so a retry cannot read or recreate a
				// partially deleted vault.
				s.hlsLifeMu.Unlock()
				c.JSON(http.StatusInternalServerError, gin.H{
					"error": "encrypted files could not be removed; vault was not deleted",
					"code":  "vault_files_remove_failed",
				})
				return
			}
		}
	}

	if err := s.db.DeleteVault(id); err != nil {
		// The record remains as a retryable tombstone when the DB operation
		// fails. If files were already removed, keep all workers quiesced: a
		// resumed thumbnail/import job could recreate directories or write to a
		// path whose encrypted contents no longer exist. A subsequent delete
		// request will wait on the same tombstone and retry the DB operation.
		log.Printf("vault %s: delete database record after file cleanup: %v", id, err)
		s.hlsLifeMu.Unlock()
		if !req.DeleteFiles {
			resumeWorkers()
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete vault record"})
		return
	}

	// Delete sessions only after all work and persistent deletion succeeded;
	// failed attempts can therefore be retried with the existing credentials.
	s.sessions.DeleteByVault(id)
	if s.tasks != nil {
		s.tasks.ForgetVault(id)
	}
	if s.thumbs != nil {
		s.thumbs.ForgetVault(id)
	}
	s.hlsLifeMu.Unlock()

	c.JSON(http.StatusOK, gin.H{"message": "vault deleted"})
}
