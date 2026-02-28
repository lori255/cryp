package session

import (
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"

	"cryp/internal/crypto"
)

const (
	// SessionExpiry is the default session lifetime
	SessionExpiry = 24 * time.Hour
	// SessionIDLength is the byte length of session IDs
	SessionIDLength = 32
)

// Session holds the in-memory session data including decrypted vault keys
type Session struct {
	ID        string
	VaultID   string
	VaultPath string
	Keys      *crypto.VaultKeys
	ExpiresAt time.Time
}

// Store manages in-memory sessions with vault keys
type Store struct {
	mu       sync.RWMutex
	sessions map[string]*Session
}

// NewStore creates a new session store
func NewStore() *Store {
	s := &Store{
		sessions: make(map[string]*Session),
	}
	// Start cleanup goroutine
	go s.cleanup()
	return s
}

// Create creates a new session for an unlocked vault
func (s *Store) Create(vaultID, vaultPath string, keys *crypto.VaultKeys) (string, error) {
	id, err := generateSessionID()
	if err != nil {
		return "", err
	}

	session := &Session{
		ID:        id,
		VaultID:   vaultID,
		VaultPath: vaultPath,
		Keys:      keys,
		ExpiresAt: time.Now().Add(SessionExpiry),
	}

	s.mu.Lock()
	s.sessions[id] = session
	s.mu.Unlock()

	return id, nil
}

// Get retrieves a session by ID
func (s *Store) Get(id string) (*Session, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	session, ok := s.sessions[id]
	if !ok {
		return nil, false
	}

	if time.Now().After(session.ExpiresAt) {
		return nil, false
	}

	return session, true
}

// Delete removes a session
func (s *Store) Delete(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if session, ok := s.sessions[id]; ok {
		// Zero out keys in memory
		zeroBytes(session.Keys.MasterKey)
		zeroBytes(session.Keys.MACKey)
		delete(s.sessions, id)
	}
}

// DeleteByVault removes all sessions for a specific vault
func (s *Store) DeleteByVault(vaultID string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for id, session := range s.sessions {
		if session.VaultID == vaultID {
			zeroBytes(session.Keys.MasterKey)
			zeroBytes(session.Keys.MACKey)
			delete(s.sessions, id)
		}
	}
}

// Refresh extends a session's expiry
func (s *Store) Refresh(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if session, ok := s.sessions[id]; ok {
		session.ExpiresAt = time.Now().Add(SessionExpiry)
	}
}

func (s *Store) cleanup() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	for range ticker.C {
		s.mu.Lock()
		now := time.Now()
		for id, session := range s.sessions {
			if now.After(session.ExpiresAt) {
				zeroBytes(session.Keys.MasterKey)
				zeroBytes(session.Keys.MACKey)
				delete(s.sessions, id)
			}
		}
		s.mu.Unlock()
	}
}

func generateSessionID() (string, error) {
	b := make([]byte, SessionIDLength)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func zeroBytes(b []byte) {
	for i := range b {
		b[i] = 0
	}
}
