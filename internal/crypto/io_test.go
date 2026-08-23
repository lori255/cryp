package crypto

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestEncryptSingleFileFailedReplacementKeepsPreviousCiphertext(t *testing.T) {
	vaultPath := t.TempDir()
	_, keys, err := InitVault(vaultPath, []byte("test-password"))
	if err != nil {
		t.Fatalf("InitVault: %v", err)
	}
	vault := &Vault{ID: "vault", Path: vaultPath, Keys: keys}

	source := filepath.Join(t.TempDir(), "video.mp4")
	if err := os.WriteFile(source, []byte("old plaintext"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := EncryptSingleFile(vault, keys, source, "/video.mp4"); err != nil {
		t.Fatalf("initial encryption: %v", err)
	}
	encPath, err := vault.ResolveExistingFilePath("/video.mp4")
	if err != nil {
		t.Fatal(err)
	}
	previous, err := os.ReadFile(encPath)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := EncryptSingleFile(vault, keys, filepath.Join(t.TempDir(), "missing.mp4"), "/video.mp4"); err == nil {
		t.Fatal("missing source unexpectedly encrypted")
	}
	current, err := os.ReadFile(encPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(previous, current) {
		t.Fatal("failed replacement changed the previous ciphertext")
	}
}
