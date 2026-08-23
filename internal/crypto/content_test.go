package crypto

import (
	"bytes"
	"crypto/rand"
	"errors"
	"io"
	"testing"
)

func TestEncryptDecryptRoundtrip(t *testing.T) {
	// Test with various sizes to cover:
	// - empty
	// - less than one chunk
	// - exactly one chunk
	// - multiple full chunks
	// - multiple chunks + partial last chunk
	sizes := []int{0, 1, 100, ChunkPayloadSize, ChunkPayloadSize + 1, ChunkPayloadSize*3 + 500, ChunkPayloadSize * 5}

	for _, size := range sizes {
		t.Run("size_"+string(rune(size)), func(t *testing.T) {
			// Generate random plaintext
			plaintext := make([]byte, size)
			if size > 0 {
				rand.Read(plaintext)
			}

			// Generate keys
			masterKey := make([]byte, MasterKeySize)
			rand.Read(masterKey)
			contentKey := make([]byte, MasterKeySize)
			rand.Read(contentKey)

			// Encrypt
			var cipherBuf bytes.Buffer
			header, err := WriteFileHeader(&cipherBuf, masterKey, contentKey)
			if err != nil {
				t.Fatalf("WriteFileHeader: %v", err)
			}

			ew, err := NewEncryptingWriter(&cipherBuf, header.ContentKey, header.Nonce)
			if err != nil {
				t.Fatalf("NewEncryptingWriter: %v", err)
			}

			n, err := ew.Write(plaintext)
			if err != nil {
				t.Fatalf("Write: %v", err)
			}
			if n != size {
				t.Fatalf("Write returned %d, want %d", n, size)
			}
			if err := ew.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}

			// Decrypt
			cipherReader := bytes.NewReader(cipherBuf.Bytes())
			readHeader, err := ReadFileHeader(cipherReader, masterKey)
			if err != nil {
				t.Fatalf("ReadFileHeader: %v", err)
			}

			dr, err := NewDecryptingReader(cipherReader, readHeader.ContentKey, readHeader.Nonce)
			if err != nil {
				t.Fatalf("NewDecryptingReader: %v", err)
			}

			decrypted, err := io.ReadAll(dr)
			if err != nil {
				t.Fatalf("ReadAll: %v", err)
			}

			if !bytes.Equal(plaintext, decrypted) {
				t.Fatalf("roundtrip failed for size %d: plaintext len=%d, decrypted len=%d", size, len(plaintext), len(decrypted))
			}
		})
	}
}

func TestDecryptingReaderRejectsTruncatedChunk(t *testing.T) {
	masterKey := make([]byte, MasterKeySize)
	contentKey := make([]byte, MasterKeySize)
	if _, err := rand.Read(masterKey); err != nil {
		t.Fatal(err)
	}
	if _, err := rand.Read(contentKey); err != nil {
		t.Fatal(err)
	}

	var encrypted bytes.Buffer
	header, err := WriteFileHeader(&encrypted, masterKey, contentKey)
	if err != nil {
		t.Fatal(err)
	}
	// A chunk containing only a nonce cannot contain an authenticated payload.
	encrypted.Write(make([]byte, ChunkNonceSize-1))

	reader, err := NewDecryptingReader(bytes.NewReader(encrypted.Bytes()[HeaderSize:]), contentKey, header.Nonce)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Release()
	_, err = io.ReadAll(reader)
	if !errors.Is(err, ErrCorruptContent) {
		t.Fatalf("expected ErrCorruptContent, got %v", err)
	}
}

func TestCipherSize2PlaintextSizeClampsMalformedChunk(t *testing.T) {
	for _, size := range []int64{HeaderSize + ChunkNonceSize + ChunkTagSize - 1, HeaderSize + 1} {
		if got := CipherSize2PlaintextSize(size); got != 0 {
			t.Fatalf("CipherSize2PlaintextSize(%d) = %d, want 0", size, got)
		}
	}
}

func TestEncryptDecryptMultipleSmallWrites(t *testing.T) {
	// Simulate many small writes (like io.Copy with small buffer)
	masterKey := make([]byte, MasterKeySize)
	rand.Read(masterKey)
	contentKey := make([]byte, MasterKeySize)
	rand.Read(contentKey)

	totalSize := ChunkPayloadSize*2 + 1000
	plaintext := make([]byte, totalSize)
	rand.Read(plaintext)

	var cipherBuf bytes.Buffer
	header, err := WriteFileHeader(&cipherBuf, masterKey, contentKey)
	if err != nil {
		t.Fatalf("WriteFileHeader: %v", err)
	}

	ew, err := NewEncryptingWriter(&cipherBuf, header.ContentKey, header.Nonce)
	if err != nil {
		t.Fatalf("NewEncryptingWriter: %v", err)
	}

	// Write in small chunks of 1000 bytes
	for i := 0; i < totalSize; i += 1000 {
		end := i + 1000
		if end > totalSize {
			end = totalSize
		}
		if _, err := ew.Write(plaintext[i:end]); err != nil {
			t.Fatalf("Write chunk %d: %v", i/1000, err)
		}
	}
	if err := ew.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Decrypt
	cipherReader := bytes.NewReader(cipherBuf.Bytes())
	readHeader, err := ReadFileHeader(cipherReader, masterKey)
	if err != nil {
		t.Fatalf("ReadFileHeader: %v", err)
	}

	dr, err := NewDecryptingReader(cipherReader, readHeader.ContentKey, readHeader.Nonce)
	if err != nil {
		t.Fatalf("NewDecryptingReader: %v", err)
	}

	// Read in small chunks too
	var result bytes.Buffer
	buf := make([]byte, 512)
	for {
		n, err := dr.Read(buf)
		if n > 0 {
			result.Write(buf[:n])
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
	}

	if !bytes.Equal(plaintext, result.Bytes()) {
		t.Fatalf("roundtrip with small writes failed: want %d bytes, got %d", totalSize, result.Len())
	}
}

func TestDecryptFromChunkRoundtrip(t *testing.T) {
	// Test that seeking (NewDecryptingReaderFromChunk) works correctly
	masterKey := make([]byte, MasterKeySize)
	rand.Read(masterKey)
	contentKey := make([]byte, MasterKeySize)
	rand.Read(contentKey)

	// 3 full chunks + partial
	totalSize := ChunkPayloadSize*3 + 5000
	plaintext := make([]byte, totalSize)
	rand.Read(plaintext)

	var cipherBuf bytes.Buffer
	header, err := WriteFileHeader(&cipherBuf, masterKey, contentKey)
	if err != nil {
		t.Fatalf("WriteFileHeader: %v", err)
	}

	ew, err := NewEncryptingWriter(&cipherBuf, header.ContentKey, header.Nonce)
	if err != nil {
		t.Fatalf("NewEncryptingWriter: %v", err)
	}
	ew.Write(plaintext)
	ew.Close()

	cipherData := cipherBuf.Bytes()

	// Read starting from chunk 2 (3rd chunk)
	startChunk := uint64(2)
	cipherOffset := int64(HeaderSize) + int64(startChunk)*int64(ChunkCipherSize)
	chunkReader := bytes.NewReader(cipherData[cipherOffset:])

	dr, err := NewDecryptingReaderFromChunk(chunkReader, header.ContentKey, header.Nonce, startChunk)
	if err != nil {
		t.Fatalf("NewDecryptingReaderFromChunk: %v", err)
	}

	decrypted, err := io.ReadAll(dr)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}

	expected := plaintext[int(startChunk)*ChunkPayloadSize:]
	if !bytes.Equal(expected, decrypted) {
		t.Fatalf("chunk seek roundtrip failed: want %d bytes, got %d", len(expected), len(decrypted))
	}
}
