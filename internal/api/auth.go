package api

import (
	"net/http"

	"cryp/internal/crypto"
	"cryp/internal/storage"

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
		c.JSON(http.StatusNotFound, gin.H{"error": "vault not found"})
		return
	}

	// Load vault config and attempt unlock
	config, err := crypto.LoadVaultConfig(vault.Path)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load vault config"})
		return
	}

	keys, err := crypto.UnlockVault([]byte(req.Password), config)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "wrong password"})
		return
	}

	// Create session
	sessionID, err := s.sessions.Create(vault.ID, vault.Path, keys)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create session"})
		return
	}

	// Set cookie
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
		s.sessions.Delete(sessionID)
	}

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

	vault, err := s.db.GetVault(sess.VaultID)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{"authenticated": false})
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

	id := generateID()
	vaultPath := s.vaultDir + "/" + id

	config, keys, err := crypto.InitVault(vaultPath, []byte(req.Password))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create vault: " + err.Error()})
		return
	}

	record := &storage.VaultRecord{
		ID:   id,
		Name: req.Name,
		Path: vaultPath,
	}
	if err := s.db.CreateVault(record); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to save vault record"})
		return
	}

	// Auto-login after creation
	sessionID, err := s.sessions.Create(id, vaultPath, keys)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "vault created but failed to create session"})
		return
	}

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

	// Delete sessions for this vault
	s.sessions.DeleteByVault(id)

	if err := s.db.DeleteVault(id); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete vault"})
		return
	}

	// Note: we don't delete the encrypted files on disk for safety
	c.JSON(http.StatusOK, gin.H{"message": "vault deleted"})
}
