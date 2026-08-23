package crypto

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"runtime"
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

func TestEncryptSingleFileRejectsSymlinkSource(t *testing.T) {
	vaultPath := t.TempDir()
	_, keys, err := InitVault(vaultPath, []byte("test-password"))
	if err != nil {
		t.Fatalf("InitVault: %v", err)
	}
	vault := &Vault{ID: "vault", Path: vaultPath, Keys: keys}

	outside := filepath.Join(t.TempDir(), "outside.mp4")
	if err := os.WriteFile(outside, []byte("secret"), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "link.mp4")
	if err := os.Symlink(outside, link); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("symlink unavailable: %v", err)
		}
		t.Fatal(err)
	}
	if _, err := EncryptSingleFile(vault, keys, link, "/link.mp4"); !errors.Is(err, ErrUnsafeSource) {
		t.Fatalf("symlink source error = %v, want ErrUnsafeSource", err)
	}
}
