package crypto

import (
	"crypto/rand"
	"strings"
	"testing"
)

func TestIndexPathEncryptionRoundtrip(t *testing.T) {
	macKey := make([]byte, MACKeySize)
	if _, err := rand.Read(macKey); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}

	path := "/photos/holiday/IMG_0001.jpg"
	encrypted, err := EncryptIndexPath(macKey, "vault-a", path)
	if err != nil {
		t.Fatalf("EncryptIndexPath: %v", err)
	}
	if strings.Contains(encrypted, "photos") || strings.Contains(encrypted, "IMG_0001") {
		t.Fatalf("encrypted path leaks plaintext: %q", encrypted)
	}

	decrypted, err := DecryptIndexPath(macKey, "vault-a", encrypted)
	if err != nil {
		t.Fatalf("DecryptIndexPath: %v", err)
	}
	if decrypted != path {
		t.Fatalf("decrypted path = %q, want %q", decrypted, path)
	}
}

func TestIndexPathEncryptionIsDeterministicAndVaultScoped(t *testing.T) {
	macKey := make([]byte, MACKeySize)
	if _, err := rand.Read(macKey); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}

	first, err := EncryptIndexPath(macKey, "vault-a", "docs/report.pdf")
	if err != nil {
		t.Fatalf("EncryptIndexPath first: %v", err)
	}
	second, err := EncryptIndexPath(macKey, "vault-a", "/docs/report.pdf")
	if err != nil {
		t.Fatalf("EncryptIndexPath second: %v", err)
	}
	if first != second {
		t.Fatalf("same normalized path encrypted differently")
	}

	otherVault, err := EncryptIndexPath(macKey, "vault-b", "/docs/report.pdf")
	if err != nil {
		t.Fatalf("EncryptIndexPath other vault: %v", err)
	}
	if otherVault == first {
		t.Fatalf("same path should be scoped by vault id")
	}
}

func TestProtectContentHashIsDeterministicAndKeyed(t *testing.T) {
	macKey := make([]byte, MACKeySize)
	if _, err := rand.Read(macKey); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}

	plainHash := "6f1ed002ab5595859014ebf0951522d9"
	first := ProtectContentHash(macKey, "vault-a", plainHash)
	second := ProtectContentHash(macKey, "vault-a", plainHash)
	if first != second {
		t.Fatalf("protected hash is not deterministic")
	}
	if first == plainHash || strings.Contains(first, plainHash) {
		t.Fatalf("protected hash leaks plaintext hash")
	}

	otherVault := ProtectContentHash(macKey, "vault-b", plainHash)
	if otherVault == first {
		t.Fatalf("protected hash should be scoped by vault id")
	}
}

func TestVirtualPathHelpers(t *testing.T) {
	cases := []struct {
		path   string
		parent string
		base   string
	}{
		{"/", "", "/"},
		{"docs/report.pdf", "/docs", "report.pdf"},
		{"/docs/report.pdf", "/docs", "report.pdf"},
		{"/docs", "/", "docs"},
	}
	for _, tc := range cases {
		if got := ParentVirtualPath(tc.path); got != tc.parent {
			t.Fatalf("ParentVirtualPath(%q) = %q, want %q", tc.path, got, tc.parent)
		}
		if got := BaseVirtualName(tc.path); got != tc.base {
			t.Fatalf("BaseVirtualName(%q) = %q, want %q", tc.path, got, tc.base)
		}
	}
}
