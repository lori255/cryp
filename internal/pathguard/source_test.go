package pathguard

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestResolveDirRejectsSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	link := filepath.Join(root, "outside")
	if err := os.Symlink(outside, link); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("symlink unavailable: %v", err)
		}
		t.Fatal(err)
	}

	guard, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := guard.ResolveDir(link); !errors.Is(err, ErrOutsideRoot) {
		t.Fatalf("ResolveDir(%q) error = %v, want ErrOutsideRoot", link, err)
	}
}

func TestValidateImportRejectsSymlinkAndRootDelete(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	guard, err := New(root)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := guard.ValidateImport(root, true); !errors.Is(err, ErrRootDelete) {
		t.Fatalf("root delete error = %v, want ErrRootDelete", err)
	}

	outside := t.TempDir()
	link := filepath.Join(root, "nested", "link")
	if err := os.Symlink(outside, link); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("symlink unavailable: %v", err)
		}
		t.Fatal(err)
	}
	if _, err := guard.ValidateImport(filepath.Join(root, "nested"), false); !errors.Is(err, ErrSymlinkNotAllowed) {
		t.Fatalf("symlink import error = %v, want ErrSymlinkNotAllowed", err)
	}
}

func TestValidateEntryRejectsLexicalTraversal(t *testing.T) {
	root := t.TempDir()
	guard, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	entry := filepath.Join(root, "..", filepath.Base(root)+"-outside")
	if err := guard.ValidateEntry(entry); err == nil {
		t.Fatal("ValidateEntry accepted a path outside the source root")
	}
}

func TestReservedSubtreeIsNotBrowsable(t *testing.T) {
	root := t.TempDir()
	reserved := filepath.Join(root, "config")
	if err := os.Mkdir(reserved, 0o700); err != nil {
		t.Fatal(err)
	}
	guard, err := NewWithReserved(root, reserved)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := guard.ResolveDir(reserved); !errors.Is(err, ErrProtectedPath) {
		t.Fatalf("ResolveDir(reserved) error = %v, want ErrProtectedPath", err)
	}
}

func TestValidateVaultPathRequiresOwnedChild(t *testing.T) {
	root := t.TempDir()
	vaultID := "0123456789abcdef"
	vaultPath := filepath.Join(root, vaultID)
	if err := os.Mkdir(vaultPath, 0o700); err != nil {
		t.Fatal(err)
	}

	resolved, err := ValidateVaultPath(root, vaultID, vaultPath)
	if err != nil {
		t.Fatalf("valid vault path rejected: %v", err)
	}
	if resolved != vaultPath {
		t.Fatalf("resolved path = %q, want %q", resolved, vaultPath)
	}

	for _, candidate := range []string{root, filepath.Join(root, "other"), filepath.Join(t.TempDir(), vaultID)} {
		if _, err := ValidateVaultPath(root, vaultID, candidate); !errors.Is(err, ErrInvalidVaultPath) {
			t.Fatalf("ValidateVaultPath(%q) error = %v, want ErrInvalidVaultPath", candidate, err)
		}
	}
}

func TestValidateVaultPathRejectsSymlink(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	vaultID := "vault"
	link := filepath.Join(root, vaultID)
	if err := os.Symlink(outside, link); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("symlink unavailable: %v", err)
		}
		t.Fatal(err)
	}
	if _, err := ValidateVaultPath(root, vaultID, link); !errors.Is(err, ErrInvalidVaultPath) {
		t.Fatalf("symlink vault path error = %v, want ErrInvalidVaultPath", err)
	}
}

func TestQuarantineVaultPathMovesDirectory(t *testing.T) {
	root := t.TempDir()
	vaultID := "vault-a"
	vaultPath := filepath.Join(root, vaultID)
	if err := os.MkdirAll(filepath.Join(vaultPath, "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	want := []byte("encrypted")
	if err := os.WriteFile(filepath.Join(vaultPath, "nested", "file.bin"), want, 0o600); err != nil {
		t.Fatal(err)
	}

	quarantine, err := QuarantineVaultPath(root, vaultID, vaultPath)
	if err != nil {
		t.Fatalf("QuarantineVaultPath() error = %v", err)
	}
	defer os.RemoveAll(quarantine)
	if quarantine == "" || filepath.Dir(quarantine) != root {
		t.Fatalf("quarantine path = %q, want a sibling directly below %q", quarantine, root)
	}
	if _, err := os.Lstat(vaultPath); !os.IsNotExist(err) {
		t.Fatalf("original vault still exists, lstat error = %v", err)
	}
	got, err := os.ReadFile(filepath.Join(quarantine, "nested", "file.bin"))
	if err != nil {
		t.Fatalf("read quarantined file: %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("quarantined content = %q, want %q", got, want)
	}
}

func TestQuarantineVaultPathAllowsMissingDirectory(t *testing.T) {
	root := t.TempDir()
	vaultID := "missing"
	quarantine, err := QuarantineVaultPath(root, vaultID, filepath.Join(root, vaultID))
	if err != nil {
		t.Fatalf("missing vault error = %v", err)
	}
	if quarantine != "" {
		t.Fatalf("missing vault quarantine = %q, want empty", quarantine)
	}
}

func TestQuarantineVaultPathRejectsChangedObject(t *testing.T) {
	root := t.TempDir()
	vaultID := "not-a-dir"
	vaultPath := filepath.Join(root, vaultID)
	if err := os.WriteFile(vaultPath, []byte("not a vault"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := QuarantineVaultPath(root, vaultID, vaultPath); !errors.Is(err, ErrInvalidVaultPath) {
		t.Fatalf("regular file error = %v, want ErrInvalidVaultPath", err)
	}
}

func TestQuarantineVaultPathRejectsInvalidIdentity(t *testing.T) {
	root := t.TempDir()
	for _, vaultID := range []string{"", ".", "..", "nested/id", "../escape"} {
		candidate := filepath.Join(root, vaultID)
		if _, err := QuarantineVaultPath(root, vaultID, candidate); !errors.Is(err, ErrInvalidVaultPath) {
			t.Fatalf("vault ID %q error = %v, want ErrInvalidVaultPath", vaultID, err)
		}
	}
}
