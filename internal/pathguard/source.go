// Package pathguard centralizes validation for user-selected filesystem paths.
//
// Importing plaintext from the host is deliberately kept behind this small
// policy object so the HTTP and background-task layers cannot drift apart.
package pathguard

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

var (
	// ErrOutsideRoot indicates that a path resolves outside the configured
	// source root (including through a symbolic link).
	ErrOutsideRoot = errors.New("path is outside the configured source root")
	// ErrSymlinkNotAllowed indicates that an import tree contains a symbolic
	// link. Following links during an import would make deleteSource unsafe.
	ErrSymlinkNotAllowed = errors.New("symbolic links are not allowed in import sources")
	// ErrRootDelete prevents deleteSource from turning an import request into a
	// request to remove the entire configured source root.
	ErrRootDelete = errors.New("the configured source root cannot be deleted")
	// ErrProtectedPath prevents browsing/importing the application's own data
	// directories when the source root is a broad mount such as /data.
	ErrProtectedPath = errors.New("path is reserved by the application")
)

// Guard validates paths below one configured, host-level root. The root is
// immutable after construction and therefore safe to share between handlers
// and background workers.
type Guard struct {
	root     string
	reserved []string
}

// New creates a guard for root. The root must exist and be a directory; this
// makes configuration errors fail during server startup instead of surfacing
// as confusing per-request authorization failures.
func New(root string) (*Guard, error) {
	return NewWithReserved(root)
}

// NewWithReserved creates a guard and reserves application-owned directories
// (for example DATA_DIR and VAULT_DIR) even when they sit below the broad
// source root. Reserved paths are canonicalized once and treated as immutable
// policy for the lifetime of the guard.
func NewWithReserved(root string, reserved ...string) (*Guard, error) {
	root = strings.TrimSpace(root)
	if root == "" {
		return nil, fmt.Errorf("source root is empty")
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve source root: %w", err)
	}
	info, err := os.Stat(absRoot)
	if err != nil {
		return nil, fmt.Errorf("stat source root: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("source root is not a directory: %s", absRoot)
	}
	rootReal, err := filepath.EvalSymlinks(absRoot)
	if err != nil {
		return nil, fmt.Errorf("resolve source root: %w", err)
	}
	guard := &Guard{root: filepath.Clean(absRoot)}
	for _, item := range reserved {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		absItem, absErr := filepath.Abs(item)
		if absErr != nil {
			return nil, fmt.Errorf("resolve reserved path: %w", absErr)
		}
		realItem, evalErr := filepath.EvalSymlinks(absItem)
		if evalErr != nil {
			// A not-yet-created reserved directory is still protected by its
			// lexical path; ResolveDir will compare it after creation.
			realItem = filepath.Clean(absItem)
		}
		if within(rootReal, realItem) {
			guard.reserved = append(guard.reserved, filepath.Clean(realItem))
		}
	}
	return guard, nil
}

// Root returns the lexical, absolute root used in API responses and logs.
func (g *Guard) Root() string {
	if g == nil {
		return ""
	}
	return g.root
}

// IsReserved reports whether path belongs to an application-owned subtree.
// It is intended for directory listings, where reserved entries should be
// hidden rather than exposed as links that will later be rejected.
func (g *Guard) IsReserved(path string) bool {
	if g == nil {
		return false
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	if g.isReserved(absPath) {
		return true
	}
	if realPath, evalErr := filepath.EvalSymlinks(absPath); evalErr == nil {
		return g.isReserved(realPath)
	}
	return false
}

// ResolveDir resolves candidate (including a symlink at the selected entry)
// and verifies that the resulting directory remains below the configured
// root. The resolved absolute path is returned so later operations do not
// follow the user-controlled link again.
func (g *Guard) ResolveDir(candidate string) (string, error) {
	if g == nil {
		return "", fmt.Errorf("source guard is not configured")
	}
	absCandidate, err := filepath.Abs(candidate)
	if err != nil {
		return "", fmt.Errorf("resolve source path: %w", err)
	}
	rootReal, err := filepath.EvalSymlinks(g.root)
	if err != nil {
		return "", fmt.Errorf("resolve source root: %w", err)
	}
	candidateReal, err := filepath.EvalSymlinks(absCandidate)
	if err != nil {
		return "", fmt.Errorf("resolve source path: %w", err)
	}
	if !within(rootReal, candidateReal) {
		return "", fmt.Errorf("%w: %s", ErrOutsideRoot, candidate)
	}
	if g.isReserved(candidateReal) {
		return "", fmt.Errorf("%w: %s", ErrProtectedPath, candidate)
	}
	info, err := os.Stat(candidateReal)
	if err != nil {
		return "", fmt.Errorf("stat source path: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("source path is not a directory: %s", candidate)
	}
	return filepath.Clean(candidateReal), nil
}

// ValidateImport resolves a source directory and validates every entry before
// a background task is created. Symlinks inside the selected tree are rejected
// rather than followed so deleteSource can never remove a target outside the
// source tree; the selected directory itself is already canonicalized by
// ResolveDir.
func (g *Guard) ValidateImport(candidate string, deleteSource bool) (string, error) {
	realPath, err := g.ResolveDir(candidate)
	if err != nil {
		return "", err
	}
	rootReal, err := filepath.EvalSymlinks(g.root)
	if err != nil {
		return "", fmt.Errorf("resolve source root: %w", err)
	}
	if deleteSource && filepath.Clean(realPath) == filepath.Clean(rootReal) {
		return "", ErrRootDelete
	}
	if err := g.validateTree(realPath); err != nil {
		return "", err
	}
	return realPath, nil
}

// ValidateEntry re-checks a path immediately before it is opened or removed.
// The second check narrows the symlink replacement window between the initial
// tree walk and the actual encryption operation.
func (g *Guard) ValidateEntry(path string) error {
	if g == nil {
		return fmt.Errorf("source guard is not configured")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("stat source entry: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%w: %s", ErrSymlinkNotAllowed, path)
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("resolve source entry: %w", err)
	}
	rootReal, err := filepath.EvalSymlinks(g.root)
	if err != nil {
		return fmt.Errorf("resolve source root: %w", err)
	}
	realPath, err := filepath.EvalSymlinks(absPath)
	if err != nil {
		return fmt.Errorf("resolve source entry: %w", err)
	}
	if !within(rootReal, realPath) {
		return fmt.Errorf("%w: %s", ErrOutsideRoot, path)
	}
	if g.isReserved(realPath) {
		return fmt.Errorf("%w: %s", ErrProtectedPath, path)
	}
	return nil
}

func (g *Guard) validateTree(root string) error {
	return filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("%w: %s", ErrSymlinkNotAllowed, path)
		}
		return g.ValidateEntry(path)
	})
}

func within(root, candidate string) bool {
	root = filepath.Clean(root)
	candidate = filepath.Clean(candidate)
	return candidate == root || strings.HasPrefix(candidate, root+string(filepath.Separator))
}

func (g *Guard) isReserved(candidate string) bool {
	for _, reserved := range g.reserved {
		if within(reserved, candidate) {
			return true
		}
	}
	return false
}
