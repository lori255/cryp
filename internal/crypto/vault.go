package crypto

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	VaultConfigFile = "vault.json"
	VaultDataDir    = "d"
	DirIDFile       = "dirid.c9r"
	EncryptedExt    = ".c9r"
	ShortNameDir    = ".c9s"
	ShortNameFile   = "name.c9r"
	// MaxEncryptedNameLen is the threshold for filename shortening
	MaxEncryptedNameLen = 220
)

// Vault represents an opened vault with its keys and path
type Vault struct {
	ID     string
	Path   string
	Config *VaultConfig
	Keys   *VaultKeys
}

// InitVault creates a new vault directory with initial structure
func InitVault(vaultPath string, password []byte) (*VaultConfig, *VaultKeys, error) {
	// Create vault directory structure
	dataDir := filepath.Join(vaultPath, VaultDataDir)
	if err := os.MkdirAll(dataDir, 0700); err != nil {
		return nil, nil, fmt.Errorf("create vault directory: %w", err)
	}

	// Create vault config
	config, keys, err := CreateVaultConfig(password)
	if err != nil {
		return nil, nil, fmt.Errorf("create vault config: %w", err)
	}

	// Write vault.json
	configData, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return nil, nil, fmt.Errorf("marshal vault config: %w", err)
	}

	configPath := filepath.Join(vaultPath, VaultConfigFile)
	if err := os.WriteFile(configPath, configData, 0600); err != nil {
		return nil, nil, fmt.Errorf("write vault config: %w", err)
	}

	// Create root directory with a directory ID
	rootDirID, err := generateDirID()
	if err != nil {
		return nil, nil, fmt.Errorf("generate root dir id: %w", err)
	}
	rootEncName, err := EncryptFileName(keys.MACKey, "root", rootDirID)
	if err != nil {
		return nil, nil, fmt.Errorf("encrypt root dir name: %w", err)
	}

	rootDir := filepath.Join(dataDir, rootEncName)
	if err := os.MkdirAll(rootDir, 0700); err != nil {
		return nil, nil, fmt.Errorf("create root directory: %w", err)
	}

	// Write root dir ID
	dirIDPath := filepath.Join(rootDir, DirIDFile)
	if err := os.WriteFile(dirIDPath, []byte(rootDirID), 0600); err != nil {
		return nil, nil, fmt.Errorf("write root dir ID: %w", err)
	}

	// Also store the root dir ID mapping in vault.json for easy lookup
	rootMapping := &RootDirMapping{
		RootDirID:   rootDirID,
		RootEncName: rootEncName,
	}
	mappingData, err := json.Marshal(rootMapping)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal root mapping: %w", err)
	}
	mappingPath := filepath.Join(vaultPath, "root.json")
	if err := os.WriteFile(mappingPath, mappingData, 0600); err != nil {
		return nil, nil, fmt.Errorf("write root mapping: %w", err)
	}

	return config, keys, nil
}

// RootDirMapping stores the root directory information
type RootDirMapping struct {
	RootDirID   string `json:"rootDirId"`
	RootEncName string `json:"rootEncName"`
}

// LoadVaultConfig reads and parses the vault configuration
func LoadVaultConfig(vaultPath string) (*VaultConfig, error) {
	configPath := filepath.Join(vaultPath, VaultConfigFile)
	data, err := os.ReadFile(configPath)
	if err != nil {
		return nil, fmt.Errorf("read vault config: %w", err)
	}

	var config VaultConfig
	if err := json.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("parse vault config: %w", err)
	}

	return &config, nil
}

// LoadRootMapping reads the root directory mapping
func LoadRootMapping(vaultPath string) (*RootDirMapping, error) {
	mappingPath := filepath.Join(vaultPath, "root.json")
	data, err := os.ReadFile(mappingPath)
	if err != nil {
		return nil, fmt.Errorf("read root mapping: %w", err)
	}

	var mapping RootDirMapping
	if err := json.Unmarshal(data, &mapping); err != nil {
		return nil, fmt.Errorf("parse root mapping: %w", err)
	}

	return &mapping, nil
}

// OpenVault opens an existing vault with the given password
func OpenVault(id, vaultPath string, password []byte) (*Vault, error) {
	config, err := LoadVaultConfig(vaultPath)
	if err != nil {
		return nil, err
	}

	keys, err := UnlockVault(password, config)
	if err != nil {
		return nil, err
	}

	return &Vault{
		ID:     id,
		Path:   vaultPath,
		Config: config,
		Keys:   keys,
	}, nil
}

// ResolvePath converts a virtual path (e.g., "/photos/vacation") to an encrypted filesystem path
// Returns the encrypted filesystem path and the directory ID of the containing directory
func (v *Vault) ResolvePath(virtualPath string) (fsPath string, dirID string, err error) {
	rootMapping, err := LoadRootMapping(v.Path)
	if err != nil {
		return "", "", err
	}

	// Start from root
	currentDir := filepath.Join(v.Path, VaultDataDir, rootMapping.RootEncName)
	currentDirID := rootMapping.RootDirID

	// Normalize and split path
	virtualPath = filepath.Clean(virtualPath)
	virtualPath = strings.TrimPrefix(virtualPath, "/")
	if virtualPath == "" || virtualPath == "." {
		return currentDir, currentDirID, nil
	}

	parts := strings.Split(virtualPath, "/")
	for _, part := range parts {
		encName, err := EncryptFileName(v.Keys.MACKey, part, currentDirID)
		if err != nil {
			return "", "", fmt.Errorf("encrypt path segment '%s': %w", part, err)
		}

		// Determine filesystem name (may be shortened for long names)
		fsName, isShort := shortenEncName(encName)

		if isShort {
			// Shortened: check .c9s dir for directory (dirid.c9r inside .c9s dir)
			dirIDPath := filepath.Join(currentDir, fsName, DirIDFile)
			if dirIDData, readErr := os.ReadFile(dirIDPath); readErr == nil {
				currentDir = filepath.Join(currentDir, fsName)
				currentDirID = string(dirIDData)
				continue
			}
			// Shortened file: content is stored inside .c9s dir as contents.c9r
			contentPath := filepath.Join(currentDir, fsName, "contents.c9r")
			return contentPath, currentDirID, nil
		}

		// Normal length: check if it's a directory
		dirIDPath := filepath.Join(currentDir, encName, DirIDFile)
		if dirIDData, readErr := os.ReadFile(dirIDPath); readErr == nil {
			currentDir = filepath.Join(currentDir, encName)
			currentDirID = string(dirIDData)
			continue
		}

		// Normal file
		targetPath := filepath.Join(currentDir, encName+EncryptedExt)
		return targetPath, currentDirID, nil
	}

	return currentDir, currentDirID, nil
}

// ListDirectory lists the contents of an encrypted directory
func (v *Vault) ListDirectory(virtualPath string) ([]FileInfo, error) {
	dirPath, dirID, err := v.ResolvePath(virtualPath)
	if err != nil {
		return nil, err
	}

	entries, err := os.ReadDir(dirPath)
	if err != nil {
		return nil, fmt.Errorf("read directory: %w", err)
	}

	var files []FileInfo
	for _, entry := range entries {
		name := entry.Name()

		// Skip special files
		if name == DirIDFile || name == VaultConfigFile || name == "root.json" {
			continue
		}

		// Handle shortened names (.c9s directories)
		if strings.HasSuffix(name, ShortNameDir) && entry.IsDir() {
			fullEncName, readErr := readShortNameFile(dirPath, name)
			if readErr != nil {
				continue
			}
			decName, decErr := DecryptFileName(v.Keys.MACKey, fullEncName, dirID)
			if decErr != nil {
				continue
			}
			// Check if it's a shortened directory (has dirid.c9r inside .c9s)
			dirIDPath := filepath.Join(dirPath, name, DirIDFile)
			if _, statErr := os.Stat(dirIDPath); statErr == nil {
				files = append(files, FileInfo{
					Name:  decName,
					IsDir: true,
				})
				continue
			}
			// It's a shortened file (contents.c9r inside .c9s)
			contentPath := filepath.Join(dirPath, name, "contents.c9r")
			info, statErr := os.Stat(contentPath)
			if statErr != nil {
				continue
			}
			files = append(files, FileInfo{
				Name:         decName,
				IsDir:        false,
				Size:         CipherSize2PlaintextSize(info.Size()),
				EncryptedLen: info.Size(),
				ModTime:      info.ModTime().Unix(),
			})
			continue
		}

		// Normal names: determine if it's an encrypted file or directory
		encName := strings.TrimSuffix(name, EncryptedExt)
		if encName == name {
			// No .c9r extension, might be a directory with encrypted name
			subDirIDPath := filepath.Join(dirPath, name, DirIDFile)
			if _, err := os.Stat(subDirIDPath); err == nil {
				decName, decErr := DecryptFileName(v.Keys.MACKey, name, dirID)
				if decErr != nil {
					continue
				}
				files = append(files, FileInfo{
					Name:  decName,
					IsDir: true,
				})
				continue
			}
			continue
		}

		// It's an encrypted file
		decName, decErr := DecryptFileName(v.Keys.MACKey, encName, dirID)
		if decErr != nil {
			continue
		}

		info, statErr := entry.Info()
		if statErr != nil {
			continue
		}

		files = append(files, FileInfo{
			Name:         decName,
			IsDir:        false,
			Size:         CipherSize2PlaintextSize(info.Size()),
			EncryptedLen: info.Size(),
			ModTime:      info.ModTime().Unix(),
		})
	}

	return files, nil
}

// FileInfo represents a decrypted file/directory entry
type FileInfo struct {
	Name         string `json:"name"`
	IsDir        bool   `json:"isDir"`
	Size         int64  `json:"size,omitempty"`
	EncryptedLen int64  `json:"-"`
	ModTime      int64  `json:"modTime,omitempty"`
}

// GetEncryptedFilePath returns the encrypted filesystem path for a virtual file path
func (v *Vault) GetEncryptedFilePath(virtualPath string) (string, error) {
	virtualPath = filepath.Clean(virtualPath)
	dir := filepath.Dir(virtualPath)
	name := filepath.Base(virtualPath)

	dirPath, dirID, err := v.ResolvePath(dir)
	if err != nil {
		return "", err
	}

	encName, err := EncryptFileName(v.Keys.MACKey, name, dirID)
	if err != nil {
		return "", err
	}

	fsName, isShort := shortenEncName(encName)
	if isShort {
		// Ensure .c9s dir exists and name.c9r is written
		if err := writeShortNameFile(dirPath, fsName, encName); err != nil {
			return "", fmt.Errorf("write short name: %w", err)
		}
		// Content stored as contents.c9r inside .c9s dir
		return filepath.Join(dirPath, fsName, "contents.c9r"), nil
	}

	return filepath.Join(dirPath, encName+EncryptedExt), nil
}

// CreateEncryptedDirectory creates a new encrypted directory
func (v *Vault) CreateEncryptedDirectory(virtualPath string) error {
	virtualPath = filepath.Clean(virtualPath)
	parentDir := filepath.Dir(virtualPath)
	dirName := filepath.Base(virtualPath)

	parentPath, parentDirID, err := v.ResolvePath(parentDir)
	if err != nil {
		return fmt.Errorf("resolve parent: %w", err)
	}

	encName, err := EncryptFileName(v.Keys.MACKey, dirName, parentDirID)
	if err != nil {
		return fmt.Errorf("encrypt dir name: %w", err)
	}

	fsName, isShort := shortenEncName(encName)
	var newDirPath string
	if isShort {
		newDirPath = filepath.Join(parentPath, fsName)
		if err := os.MkdirAll(newDirPath, 0700); err != nil {
			return fmt.Errorf("create directory: %w", err)
		}
		if err := writeShortNameFile(parentPath, fsName, encName); err != nil {
			return fmt.Errorf("write short name: %w", err)
		}
	} else {
		newDirPath = filepath.Join(parentPath, encName)
		if err := os.MkdirAll(newDirPath, 0700); err != nil {
			return fmt.Errorf("create directory: %w", err)
		}
	}

	// Generate and write directory ID
	newDirID, err := generateDirID()
	if err != nil {
		return fmt.Errorf("generate dir ID: %w", err)
	}
	dirIDPath := filepath.Join(newDirPath, DirIDFile)
	if err := os.WriteFile(dirIDPath, []byte(newDirID), 0600); err != nil {
		return fmt.Errorf("write dir ID: %w", err)
	}

	return nil
}

func generateDirID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate dir id: %w", err)
	}
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:]), nil
}

// shortenEncName returns the filesystem name for an encrypted name.
// If the encrypted name exceeds MaxEncryptedNameLen, it returns a shortened
// SHA-256 hash name and isShort=true. The caller must store the full name
// in a .c9s/name.c9r file.
func shortenEncName(encName string) (fsName string, isShort bool) {
	if len(encName) <= MaxEncryptedNameLen {
		return encName, false
	}
	hash := sha256.Sum256([]byte(encName))
	shortName := base64.RawURLEncoding.EncodeToString(hash[:])
	return shortName + ShortNameDir, true
}

// writeShortNameFile stores the full encrypted name in a .c9s directory.
func writeShortNameFile(parentDir, shortDirName, fullEncName string) error {
	shortDir := filepath.Join(parentDir, shortDirName)
	if err := os.MkdirAll(shortDir, 0700); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(shortDir, ShortNameFile), []byte(fullEncName), 0600)
}

// readShortNameFile reads the full encrypted name from a .c9s directory.
func readShortNameFile(parentDir, shortDirName string) (string, error) {
	data, err := os.ReadFile(filepath.Join(parentDir, shortDirName, ShortNameFile))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}
