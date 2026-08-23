package pathguard

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ErrInvalidVaultPath indicates that a database vault path is not the
// application-owned directory for the requested vault ID.  Destructive
// operations must fail closed instead of trusting a mutable database field.
var ErrInvalidVaultPath = errors.New("vault path is outside the configured vault directory")

// ValidateVaultPath validates a vault directory before a destructive
// operation. The returned path is absolute and lexical; deletion callers
// should use QuarantineVaultPath to atomically detach it before RemoveAll.
// Existing symlinks in the vault path are rejected, and missing paths are
// accepted so a retry can still remove the database record.
func ValidateVaultPath(vaultRoot, vaultID, candidate string) (string, error) {
	vaultRoot = strings.TrimSpace(vaultRoot)
	vaultID = strings.TrimSpace(vaultID)
	candidate = strings.TrimSpace(candidate)
	if vaultRoot == "" || candidate == "" || vaultID == "" ||
		filepath.Base(vaultID) != vaultID || vaultID == "." || vaultID == ".." ||
		strings.ContainsRune(vaultID, 0) {
		return "", fmt.Errorf("%w: invalid vault identity", ErrInvalidVaultPath)
	}

	rootAbs, err := filepath.Abs(vaultRoot)
	if err != nil {
		return "", fmt.Errorf("%w: resolve vault root: %v", ErrInvalidVaultPath, err)
	}
	pathAbs, err := filepath.Abs(candidate)
	if err != nil {
		return "", fmt.Errorf("%w: resolve vault path: %v", ErrInvalidVaultPath, err)
	}
	rootAbs = filepath.Clean(rootAbs)
	pathAbs = filepath.Clean(pathAbs)
	if pathAbs == rootAbs || !within(rootAbs, pathAbs) || filepath.Base(pathAbs) != vaultID {
		return "", fmt.Errorf("%w: %s", ErrInvalidVaultPath, candidate)
	}

	rootReal, err := filepath.EvalSymlinks(rootAbs)
	if err != nil {
		return "", fmt.Errorf("%w: resolve vault root: %v", ErrInvalidVaultPath, err)
	}
	if err := rejectSymlinkBelow(rootAbs, pathAbs); err != nil {
		return "", err
	}

	info, statErr := os.Lstat(pathAbs)
	if statErr == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("%w: vault directory is a symlink", ErrInvalidVaultPath)
		}
		if !info.IsDir() {
			return "", fmt.Errorf("%w: vault path is not a directory", ErrInvalidVaultPath)
		}
		realPath, evalErr := filepath.EvalSymlinks(pathAbs)
		if evalErr != nil {
			return "", fmt.Errorf("%w: resolve vault path: %v", ErrInvalidVaultPath, evalErr)
		}
		if !within(rootReal, realPath) {
			return "", fmt.Errorf("%w: resolved vault path escapes root", ErrInvalidVaultPath)
		}
	} else if !os.IsNotExist(statErr) {
		return "", fmt.Errorf("%w: stat vault path: %v", ErrInvalidVaultPath, statErr)
	} else {
		// The final directory may already have been removed.  Resolve its
		// parent so a missing path cannot hide a symlink escape.
		parentReal, evalErr := filepath.EvalSymlinks(filepath.Dir(pathAbs))
		if evalErr != nil || !within(rootReal, parentReal) {
			if evalErr != nil {
				return "", fmt.Errorf("%w: resolve vault parent: %v", ErrInvalidVaultPath, evalErr)
			}
			return "", fmt.Errorf("%w: vault parent escapes root", ErrInvalidVaultPath)
		}
	}
	return pathAbs, nil
}

// QuarantineVaultPath atomically moves an existing vault directory to a fresh
// sibling under the configured vault root. Callers can then remove the
// returned path without ever recursively walking the database-controlled name.
// A missing vault directory returns an empty quarantine path and is treated as
// already cleaned up.
func QuarantineVaultPath(vaultRoot, vaultID, candidate string) (string, error) {
	validated, err := ValidateVaultPath(vaultRoot, vaultID, candidate)
	if err != nil {
		return "", err
	}
	info, statErr := os.Lstat(validated)
	if os.IsNotExist(statErr) {
		return "", nil
	}
	if statErr != nil {
		return "", fmt.Errorf("%w: stat vault before quarantine: %v", ErrInvalidVaultPath, statErr)
	}
	// Repeat the object-kind check immediately before rename. If the path was
	// replaced after validation, fail closed instead of treating a symlink or
	// regular file as a vault directory.
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return "", fmt.Errorf("%w: vault path changed before quarantine", ErrInvalidVaultPath)
	}

	rootAbs, err := filepath.Abs(vaultRoot)
	if err != nil {
		return "", fmt.Errorf("%w: resolve vault root: %v", ErrInvalidVaultPath, err)
	}
	// MkdirTemp reserves a unique sibling name. Remove the empty directory
	// before rename; rename replaces a maliciously substituted symlink rather
	// than following it, so the operation remains bounded to this root.
	quarantine, err := os.MkdirTemp(filepath.Clean(rootAbs), ".cryp-delete-"+vaultID+"-")
	if err != nil {
		return "", fmt.Errorf("quarantine vault: %w", err)
	}
	// The caller owns the quarantine path only after rename succeeds. Clean up
	// the temporary reservation on every earlier failure.
	keepQuarantine := false
	defer func() {
		if !keepQuarantine {
			_ = os.Remove(quarantine)
		}
	}()
	if err := os.Remove(quarantine); err != nil {
		return "", fmt.Errorf("prepare vault quarantine: %w", err)
	}
	if err := os.Rename(validated, quarantine); err != nil {
		return "", fmt.Errorf("quarantine vault: %w", err)
	}
	keepQuarantine = true
	return quarantine, nil
}

func rejectSymlinkBelow(rootAbs, candidateAbs string) error {
	rel, err := filepath.Rel(rootAbs, candidateAbs)
	if err != nil {
		return fmt.Errorf("%w: compare vault path: %v", ErrInvalidVaultPath, err)
	}
	current := rootAbs
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		if part == "" || part == "." {
			continue
		}
		current = filepath.Join(current, part)
		info, statErr := os.Lstat(current)
		if os.IsNotExist(statErr) {
			break
		}
		if statErr != nil {
			return fmt.Errorf("%w: inspect vault path: %v", ErrInvalidVaultPath, statErr)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%w: vault path contains a symlink", ErrInvalidVaultPath)
		}
	}
	return nil
}
