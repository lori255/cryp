package session

import (
	"testing"

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
