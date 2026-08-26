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

func TestEncryptResultSourceIdentityRejectsPathReplacement(t *testing.T) {
	vaultPath := t.TempDir()
	_, keys, err := InitVault(vaultPath, []byte("test-password"))
	if err != nil {
		t.Fatalf("InitVault: %v", err)
	}
	vault := &Vault{ID: "vault", Path: vaultPath, Keys: keys}

	sourceDir := t.TempDir()
	source := filepath.Join(sourceDir, "video.mp4")
	if err := os.WriteFile(source, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	result, err := EncryptSingleFile(vault, keys, source, "/video.mp4")
	if err != nil {
		t.Fatalf("EncryptSingleFile: %v", err)
	}
	if err := ValidateSourceIdentity(source, result.Source); err != nil {
		t.Fatalf("unchanged source rejected: %v", err)
	}

	replacement := filepath.Join(sourceDir, "replacement.tmp")
	if err := os.WriteFile(replacement, []byte("new file"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, source); err != nil {
		t.Fatal(err)
	}
	if err := ValidateSourceIdentity(source, result.Source); !errors.Is(err, ErrUnsafeSource) {
		t.Fatalf("replacement validation error = %v, want ErrUnsafeSource", err)
	}
	contents, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	if string(contents) != "new file" {
		t.Fatalf("replacement contents = %q", contents)
	}
}

func TestEncryptResultSourceIdentityRejectsGrowth(t *testing.T) {
	vaultPath := t.TempDir()
	_, keys, err := InitVault(vaultPath, []byte("test-password"))
	if err != nil {
		t.Fatalf("InitVault: %v", err)
	}
	vault := &Vault{ID: "vault", Path: vaultPath, Keys: keys}

	source := filepath.Join(t.TempDir(), "video.mp4")
	if err := os.WriteFile(source, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	result, err := EncryptSingleFile(vault, keys, source, "/video.mp4")
	if err != nil {
		t.Fatalf("EncryptSingleFile: %v", err)
	}
	file, err := os.OpenFile(source, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(" growing"); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if err := ValidateSourceIdentity(source, result.Source); !errors.Is(err, ErrUnsafeSource) {
		t.Fatalf("growth validation error = %v, want ErrUnsafeSource", err)
	}
}

func TestRemoveSourceFileDeletesOnlyCapturedIdentity(t *testing.T) {
	vaultPath := t.TempDir()
	_, keys, err := InitVault(vaultPath, []byte("test-password"))
	if err != nil {
		t.Fatalf("InitVault: %v", err)
	}
	vault := &Vault{ID: "vault", Path: vaultPath, Keys: keys}
	source := filepath.Join(t.TempDir(), "source.bin")
	if err := os.WriteFile(source, []byte("source"), 0600); err != nil {
		t.Fatal(err)
	}
	result, err := EncryptSingleFile(vault, keys, source, "/source.bin")
	if err != nil {
		t.Fatal(err)
	}
	if err := RemoveSourceFile(source, result.Source); err != nil {
		t.Fatalf("RemoveSourceFile: %v", err)
	}
	if _, err := os.Stat(source); !os.IsNotExist(err) {
		t.Fatalf("source still exists, stat error = %v", err)
	}
}

func TestRemoveSourceFilePreservesReplacement(t *testing.T) {
	vaultPath := t.TempDir()
	_, keys, err := InitVault(vaultPath, []byte("test-password"))
	if err != nil {
		t.Fatalf("InitVault: %v", err)
	}
	vault := &Vault{ID: "vault", Path: vaultPath, Keys: keys}
	sourceDir := t.TempDir()
	source := filepath.Join(sourceDir, "source.bin")
	if err := os.WriteFile(source, []byte("source"), 0600); err != nil {
		t.Fatal(err)
	}
	result, err := EncryptSingleFile(vault, keys, source, "/source.bin")
	if err != nil {
		t.Fatal(err)
	}
	replacement := filepath.Join(sourceDir, "replacement.bin")
	if err := os.WriteFile(replacement, []byte("replacement"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, source); err != nil {
		t.Fatal(err)
	}
	if err := RemoveSourceFile(source, result.Source); !errors.Is(err, ErrUnsafeSource) {
		t.Fatalf("RemoveSourceFile error = %v, want ErrUnsafeSource", err)
	}
	contents, err := os.ReadFile(source)
	if err != nil {
		t.Fatalf("replacement missing: %v", err)
	}
	if string(contents) != "replacement" {
		t.Fatalf("replacement contents = %q", contents)
	}
}
