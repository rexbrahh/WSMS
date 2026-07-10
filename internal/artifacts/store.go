// Package artifacts provides content-addressed blob storage for exact evidence.
package artifacts

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// RefPrefix is the canonical artifact-reference prefix.
const RefPrefix = "artifact:sha256:"

var (
	// ErrInvalidHash reports a non-canonical SHA-256 address.
	ErrInvalidHash = errors.New("invalid artifact sha256 hash")
	// ErrInvalidRef reports an artifact-shaped reference that is not canonical.
	ErrInvalidRef = errors.New("invalid artifact reference")
	// ErrArtifactNotFound reports a valid address with no stored artifact.
	ErrArtifactNotFound = errors.New("artifact not found")
	// ErrArtifactCorrupt reports bytes that do not match their content address.
	ErrArtifactCorrupt = errors.New("artifact digest mismatch")
)

// Store is a content-addressed artifact store on the local filesystem.
type Store struct {
	rootPath   string
	root       *os.Root
	tempSuffix func() (string, error)
	cleanup    runtime.Cleanup
	closeOnce  sync.Once
	closeErr   error
}

// Meta describes a stored artifact.
type Meta struct {
	SHA256      string
	Path        string
	Size        int64
	ContentType string
	CreatedAt   time.Time
}

// Open creates a store rooted at dir (created if missing).
func Open(dir string) (*Store, error) {
	rootPath, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(rootPath, 0o755); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return nil, err
	}
	s := &Store{
		rootPath:   rootPath,
		root:       root,
		tempSuffix: randomSuffix,
	}
	s.cleanup = runtime.AddCleanup(s, closeRoot, root)
	return s, nil
}

// Close releases the directory capability held by the store.
func (s *Store) Close() error {
	s.closeOnce.Do(func() {
		s.cleanup.Stop()
		s.closeErr = s.root.Close()
	})
	return s.closeErr
}

// Put writes data if not already present and returns its sha256 hex digest.
func (s *Store) Put(data []byte, contentType string) (Meta, error) {
	defer runtime.KeepAlive(s)

	sum := sha256.Sum256(data)
	hash := hex.EncodeToString(sum[:])
	relPath, err := relativePathFor(hash)
	if err != nil {
		return Meta{}, err
	}
	absPath := filepath.Join(s.rootPath, relPath)
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	if _, err := s.root.Stat(relPath); err == nil {
		if _, err := s.Get(hash); err != nil {
			return Meta{}, err
		}
		return newMeta(hash, absPath, data, contentType), nil
	} else if !os.IsNotExist(err) {
		return Meta{}, err
	}

	parent := filepath.Dir(relPath)
	if err := s.root.MkdirAll(parent, 0o755); err != nil {
		return Meta{}, err
	}
	tmp, file, err := s.createTemp(parent, filepath.Base(relPath))
	if err != nil {
		return Meta{}, err
	}
	tempExists := true
	defer func() {
		if tempExists {
			_ = s.root.Remove(tmp)
		}
	}()

	if err := writeAndClose(file, data); err != nil {
		return Meta{}, err
	}
	if err := s.root.Rename(tmp, relPath); err != nil {
		return Meta{}, err
	}
	tempExists = false

	// A concurrent writer may have committed the same address immediately before
	// this rename. Verify the bytes currently reachable at the durable address
	// before reporting success; the content digest remains the authority.
	if _, err := s.Get(hash); err != nil {
		return Meta{}, err
	}
	return newMeta(hash, absPath, data, contentType), nil
}

// Get returns artifact bytes by sha256 hex.
func (s *Store) Get(hash string) ([]byte, error) {
	defer runtime.KeepAlive(s)

	normalized, err := normalizeHash(hash)
	if err != nil {
		return nil, err
	}
	relPath, err := relativePathFor(normalized)
	if err != nil {
		return nil, err
	}
	data, err := s.root.ReadFile(relPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%w: %s", ErrArtifactNotFound, normalized)
		}
		return nil, err
	}
	actual := sha256.Sum256(data)
	actualHash := hex.EncodeToString(actual[:])
	if actualHash != normalized {
		return nil, fmt.Errorf("%w: requested %s, found %s", ErrArtifactCorrupt, normalized, actualHash)
	}
	return data, nil
}

// Exists reports whether hash is present.
func (s *Store) Exists(hash string) bool {
	_, err := s.Get(hash)
	return err == nil
}

// Ref returns the WSL-style pointer for a hash.
func Ref(hash string) string {
	normalized, err := normalizeHash(hash)
	if err != nil {
		return ""
	}
	return RefPrefix + normalized
}

// ParseRef extracts the hex hash from artifact:sha256:<hex>.
func ParseRef(ref string) (string, bool) {
	if !strings.HasPrefix(ref, RefPrefix) {
		return "", false
	}
	normalized, err := normalizeHash(strings.TrimPrefix(ref, RefPrefix))
	if err != nil {
		return "", false
	}
	return normalized, true
}

func normalizeHash(hash string) (string, error) {
	if len(hash) != sha256.Size*2 {
		return "", fmt.Errorf("%w: expected 64 hexadecimal characters", ErrInvalidHash)
	}
	if _, err := hex.DecodeString(hash); err != nil {
		return "", fmt.Errorf("%w: %v", ErrInvalidHash, err)
	}
	return strings.ToLower(hash), nil
}

func (s *Store) pathFor(hash string) (string, error) {
	relPath, err := relativePathFor(hash)
	if err != nil {
		return "", err
	}
	return filepath.Join(s.rootPath, relPath), nil
}

func relativePathFor(hash string) (string, error) {
	normalized, err := normalizeHash(hash)
	if err != nil {
		return "", err
	}
	return filepath.Join(normalized[:2], normalized[2:4], normalized), nil
}

func (s *Store) createTemp(parent, base string) (string, *os.File, error) {
	const maxAttempts = 100
	for range maxAttempts {
		suffix, err := s.tempSuffix()
		if err != nil {
			return "", nil, err
		}
		name := filepath.Join(parent, "."+base+".tmp-"+suffix)
		file, err := s.root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			return name, file, nil
		}
		if !os.IsExist(err) {
			return "", nil, err
		}
	}
	return "", nil, fmt.Errorf("create artifact temporary file: too many name collisions")
}

func randomSuffix() (string, error) {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(random[:]), nil
}

func writeAndClose(file *os.File, data []byte) error {
	n, err := file.Write(data)
	if err != nil {
		_ = file.Close()
		return err
	}
	if n != len(data) {
		_ = file.Close()
		return io.ErrShortWrite
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

func newMeta(hash, path string, data []byte, contentType string) Meta {
	return Meta{
		SHA256:      hash,
		Path:        path,
		Size:        int64(len(data)),
		ContentType: contentType,
		CreatedAt:   time.Now().UTC(),
	}
}

func closeRoot(root *os.Root) {
	_ = root.Close()
}
