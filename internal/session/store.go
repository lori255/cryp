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
	s.mu.RLock()
	defer s.mu.RUnlock()

	session, ok := s.sessions[id]
	if !ok {
		return nil, false
	}

	if time.Now().After(session.ExpiresAt) {
		return nil, false
	}

	// Return a snapshot. Handlers often use keys after this lock is released;
	// returning the store-owned pointer would race with logout/expiry, which
	// zeroes keys while removing a session.
	snapshot := *session
	snapshot.Keys = session.Keys.Clone()
	return &snapshot, true
}

// Delete removes a session
func (s *Store) Delete(id string) {
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

func zeroBytes(b []byte) {
	for i := range b {
		b[i] = 0
	}
}

func zeroVaultKeys(keys *crypto.VaultKeys) {
	if keys == nil {
		return
	}
	zeroBytes(keys.MasterKey)
	zeroBytes(keys.MACKey)
}
