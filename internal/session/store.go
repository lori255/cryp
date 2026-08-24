package session

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
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
	stopCh   chan struct{}
	stopOnce sync.Once
	closed   bool
}

// NewStore creates a new session store
func NewStore() *Store {
	s := &Store{
		sessions: make(map[string]*Session),
		stopCh:   make(chan struct{}),
	}
	// Start cleanup goroutine
	go s.cleanup()
	return s
}

// Close stops the cleanup goroutine and zeroes any remaining keys. It is safe
// to call more than once and should be used when the server shuts down.
func (s *Store) Close() {
	if s == nil {
		return
	}
	s.stopOnce.Do(func() {
		s.mu.Lock()
		s.closed = true
		if s.stopCh != nil {
			close(s.stopCh)
		}
		defer s.mu.Unlock()
		for id, session := range s.sessions {
			zeroVaultKeys(session.Keys)
			delete(s.sessions, id)
		}
	})
}

// Create creates a new session for an unlocked vault
func (s *Store) Create(vaultID, vaultPath string, keys *crypto.VaultKeys) (string, error) {
	if s == nil {
		return "", errors.New("session store is nil")
	}
	if keys == nil {
		return "", errors.New("session keys are missing")
	}
	id, err := generateSessionID()
	if err != nil {
		return "", err
	}

	ownedKeys := keys.Clone()
	session := &Session{
		ID:        id,
		VaultID:   vaultID,
		VaultPath: vaultPath,
		Keys:      ownedKeys,
		ExpiresAt: time.Now().Add(SessionExpiry),
	}

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		zeroVaultKeys(ownedKeys)
		return "", errors.New("session store is closed")
	}
	if s.sessions == nil {
		s.sessions = make(map[string]*Session)
	}
	s.sessions[id] = session
	s.mu.Unlock()

	return id, nil
}

// Get retrieves a session by ID
func (s *Store) Get(id string) (*Session, bool) {
	if s == nil {
		return nil, false
	}
	now := time.Now()
	s.mu.RLock()
	session, ok := s.sessions[id]
	if !ok {
		s.mu.RUnlock()
		return nil, false
	}
	if now.After(session.ExpiresAt) {
		s.mu.RUnlock()
		// Upgrade only the expired path to a write lock. Re-check after the
		// upgrade because another request may have refreshed or deleted it.
		s.mu.Lock()
		if current, exists := s.sessions[id]; exists && now.After(current.ExpiresAt) {
			zeroVaultKeys(current.Keys)
			delete(s.sessions, id)
		}
		s.mu.Unlock()
		return nil, false
	}

	// Return a snapshot. Handlers often use keys after this lock is released;
	// returning the store-owned pointer would race with logout/expiry, which
	// zeroes keys while removing a session.
	snapshot := *session
	snapshot.Keys = session.Keys.Clone()
	s.mu.RUnlock()
	return &snapshot, true
}

// Has reports whether an unexpired session is still present without cloning
// its key material. It is useful for re-checking authorization after a
// lifecycle barrier (for example, a write that waited while a vault was being
// deleted).
func (s *Store) Has(id string) bool {
	if s == nil || id == "" {
		return false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	sess, ok := s.sessions[id]
	return ok && time.Now().Before(sess.ExpiresAt)
}

// Delete removes a session
func (s *Store) Delete(id string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if session, ok := s.sessions[id]; ok {
		// Zero out keys in memory
		zeroVaultKeys(session.Keys)
		delete(s.sessions, id)
	}
}

// DeleteByVault removes all sessions for a specific vault
func (s *Store) DeleteByVault(vaultID string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	for id, session := range s.sessions {
		if session.VaultID == vaultID {
			zeroVaultKeys(session.Keys)
			delete(s.sessions, id)
		}
	}
}

// Refresh extends a session's expiry
func (s *Store) Refresh(id string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if session, ok := s.sessions[id]; ok {
		session.ExpiresAt = time.Now().Add(SessionExpiry)
	}
}

func (s *Store) cleanup() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-s.stopCh:
			return
		case <-ticker.C:
			s.mu.Lock()
			now := time.Now()
			for id, session := range s.sessions {
				if now.After(session.ExpiresAt) {
					zeroVaultKeys(session.Keys)
					delete(s.sessions, id)
				}
			}
			s.mu.Unlock()
		}
	}
}

func generateSessionID() (string, error) {
	b := make([]byte, SessionIDLength)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func zeroVaultKeys(keys *crypto.VaultKeys) {
	if keys != nil {
		keys.Zero()
	}
}
