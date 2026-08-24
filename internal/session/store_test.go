package session

import (
	"testing"
	"time"

	"cryp/internal/crypto"
)

func TestGetReturnsIndependentKeySnapshot(t *testing.T) {
	store := NewStore()
	defer store.Close()

	keys := &crypto.VaultKeys{
		MasterKey: []byte{1, 2, 3},
		MACKey:    []byte{4, 5, 6},
	}
	id, err := store.Create("vault", "/vault", keys)
	if err != nil {
		t.Fatal(err)
	}
	keys.MasterKey[0] = 99

	snapshot, ok := store.Get(id)
	if !ok {
		t.Fatal("session was not returned")
	}
	if snapshot.Keys.MasterKey[0] != 1 {
		t.Fatalf("stored key changed through caller mutation: %v", snapshot.Keys.MasterKey)
	}
	snapshot.Keys.MasterKey[0] = 88

	second, ok := store.Get(id)
	if !ok {
		t.Fatal("session disappeared")
	}
	if second.Keys.MasterKey[0] != 1 {
		t.Fatalf("Get returned store-owned key slice: %v", second.Keys.MasterKey)
	}
}

func TestCreateAfterCloseFails(t *testing.T) {
	store := NewStore()
	store.Close()
	_, err := store.Create("vault", "/vault", &crypto.VaultKeys{})
	if err == nil {
		t.Fatal("Create succeeded after Close")
	}
}

func TestGetImmediatelyDeletesExpiredSessionAndZeroesKeys(t *testing.T) {
	store := NewStore()
	defer store.Close()

	id, err := store.Create("vault", "/vault", &crypto.VaultKeys{
		MasterKey: []byte{1, 2, 3},
		MACKey:    []byte{4, 5, 6},
	})
	if err != nil {
		t.Fatal(err)
	}

	store.mu.Lock()
	stored := store.sessions[id]
	stored.ExpiresAt = time.Now().Add(-time.Second)
	masterKey := stored.Keys.MasterKey
	macKey := stored.Keys.MACKey
	store.mu.Unlock()

	if snapshot, ok := store.Get(id); ok || snapshot != nil {
		t.Fatal("expired session was returned")
	}

	store.mu.RLock()
	_, exists := store.sessions[id]
	store.mu.RUnlock()
	if exists {
		t.Fatal("expired session remained in the store")
	}
	for _, value := range append(masterKey, macKey...) {
		if value != 0 {
			t.Fatal("expired session keys were not zeroed")
		}
	}
}
