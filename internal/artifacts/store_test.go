package artifacts

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestPutGetRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s := openTestStore(t, filepath.Join(dir, "arts"))
	data := []byte("stream goroutine still blocked\n")
	meta, err := s.Put(data, "text/plain")
	if err != nil {
		t.Fatal(err)
	}
	if meta.SHA256 == "" {
		t.Fatal("empty hash")
	}
	got, err := s.Get(meta.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("got %q want %q", got, data)
	}
	// idempotent put
	meta2, err := s.Put(data, "text/plain")
	if err != nil {
		t.Fatal(err)
	}
	if meta2.SHA256 != meta.SHA256 {
		t.Fatalf("hash mismatch on re-put")
	}
	if !s.Exists(meta.SHA256) {
		t.Fatal("expected exists")
	}
	ref := Ref(meta.SHA256)
	h, ok := ParseRef(ref)
	if !ok || h != meta.SHA256 {
		t.Fatalf("ParseRef failed: %v %q", ok, h)
	}
}

func TestHashValidationAndCanonicalReferences(t *testing.T) {
	s := openTestStore(t, filepath.Join(t.TempDir(), "arts"))

	upper := strings.Repeat("A", 64)
	lower := strings.ToLower(upper)
	if got := Ref(upper); got != RefPrefix+lower {
		t.Fatalf("Ref uppercase=%q, want %q", got, RefPrefix+lower)
	}
	if got, ok := ParseRef(RefPrefix + upper); !ok || got != lower {
		t.Fatalf("ParseRef uppercase=(%q, %v), want (%q, true)", got, ok, lower)
	}

	invalid := []string{
		"",
		"abc",
		strings.Repeat("0", 63),
		strings.Repeat("0", 65),
		strings.Repeat("g", 64),
		"../../" + strings.Repeat("a", 64),
	}
	for _, hash := range invalid {
		t.Run(hash, func(t *testing.T) {
			if got := Ref(hash); got != "" {
				t.Fatalf("Ref(%q)=%q, want empty", hash, got)
			}
			if got, ok := ParseRef(RefPrefix + hash); ok || got != "" {
				t.Fatalf("ParseRef accepted %q as %q", hash, got)
			}
			if _, err := s.Get(hash); !errors.Is(err, ErrInvalidHash) {
				t.Fatalf("Get(%q) error=%v, want invalid hash", hash, err)
			}
			if s.Exists(hash) {
				t.Fatalf("Exists(%q)=true", hash)
			}
			if _, err := s.pathFor(hash); !errors.Is(err, ErrInvalidHash) {
				t.Fatalf("pathFor(%q) error=%v, want invalid hash", hash, err)
			}
		})
	}
}

func TestGetNormalizesValidHashCase(t *testing.T) {
	s := openTestStore(t, filepath.Join(t.TempDir(), "arts"))
	data := []byte("case-normalized artifact")
	meta, err := s.Put(data, "text/plain")
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(strings.ToUpper(meta.SHA256))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("got %q want %q", got, data)
	}
}

func TestGetMissingArtifactIsError(t *testing.T) {
	s := openTestStore(t, filepath.Join(t.TempDir(), "arts"))
	if _, err := s.Get(strings.Repeat("0", 64)); !errors.Is(err, ErrArtifactNotFound) {
		t.Fatalf("error=%v, want artifact not found", err)
	}
}

func TestGetDetectsCorruptArtifact(t *testing.T) {
	s := openTestStore(t, filepath.Join(t.TempDir(), "arts"))
	data := []byte("original exact evidence")
	meta, err := s.Put(data, "text/plain")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(meta.Path, []byte("tampered evidence"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(meta.SHA256); !errors.Is(err, ErrArtifactCorrupt) {
		t.Fatalf("Get error=%v, want digest mismatch", err)
	}
	if s.Exists(meta.SHA256) {
		t.Fatal("corrupt artifact reported as existing")
	}
	if _, err := s.Put(data, "text/plain"); !errors.Is(err, ErrArtifactCorrupt) {
		t.Fatalf("Put over corrupt address error=%v, want digest mismatch", err)
	}
}

func TestCloseIsConcurrentAndIdempotent(t *testing.T) {
	s := openTestStore(t, filepath.Join(t.TempDir(), "artifacts"))
	meta, err := s.Put([]byte("close capability exactly once"), "text/plain")
	if err != nil {
		t.Fatal(err)
	}

	const callers = 32
	start := make(chan struct{})
	results := make(chan error, callers)
	var wg sync.WaitGroup
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			results <- s.Close()
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatalf("concurrent Close returned %v", err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatalf("repeated Close returned %v", err)
	}
	if _, err := s.Get(meta.SHA256); err == nil {
		t.Fatal("closed store remained usable")
	}
}

func TestConcurrentIdenticalPutIsIdempotentAndVerified(t *testing.T) {
	s := openTestStore(t, filepath.Join(t.TempDir(), "artifacts"))
	data := []byte(strings.Repeat("concurrent exact evidence\n", 128))
	sum := sha256.Sum256(data)
	wantHash := hex.EncodeToString(sum[:])

	const writers = 32
	start := make(chan struct{})
	results := make(chan struct {
		meta Meta
		err  error
	}, writers)
	var wg sync.WaitGroup
	for range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			meta, err := s.Put(data, "text/plain")
			results <- struct {
				meta Meta
				err  error
			}{meta: meta, err: err}
		}()
	}
	close(start)
	wg.Wait()
	close(results)

	var finalPath string
	for result := range results {
		if result.err != nil {
			t.Fatalf("concurrent Put: %v", result.err)
		}
		if result.meta.SHA256 != wantHash {
			t.Fatalf("hash=%q, want %q", result.meta.SHA256, wantHash)
		}
		if finalPath == "" {
			finalPath = result.meta.Path
		} else if result.meta.Path != finalPath {
			t.Fatalf("path=%q, want common path %q", result.meta.Path, finalPath)
		}
	}

	got, err := s.Get(wantHash)
	if err != nil {
		t.Fatalf("digest verification after concurrent Put: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Fatal("concurrent Put changed artifact bytes")
	}
	temps, err := filepath.Glob(filepath.Join(filepath.Dir(finalPath), "."+wantHash+".tmp-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(temps) != 0 {
		t.Fatalf("temporary files leaked: %v", temps)
	}
}

func TestArtifactOperationsDoNotEscapeThroughSymlinks(t *testing.T) {
	data := []byte("exact bytes whose address names the final artifact")
	sum := sha256.Sum256(data)
	hash := hex.EncodeToString(sum[:])

	t.Run("parent", func(t *testing.T) {
		base := t.TempDir()
		rootPath := filepath.Join(base, "artifacts")
		s := openTestStore(t, rootPath)
		outside := filepath.Join(base, "outside")
		if err := os.MkdirAll(outside, 0o755); err != nil {
			t.Fatal(err)
		}
		sentinel := filepath.Join(outside, "sentinel")
		assertSentinelUnchangedAfter(t, sentinel, func() {
			requireSymlink(t, outside, filepath.Join(rootPath, hash[:2]))
			if _, err := s.Put(data, "text/plain"); err == nil {
				t.Fatal("Put followed an escaping parent symlink")
			}
		})
		if _, err := os.Stat(filepath.Join(outside, hash[2:4], hash)); !os.IsNotExist(err) {
			t.Fatalf("artifact created outside root through parent symlink: %v", err)
		}
	})

	t.Run("final", func(t *testing.T) {
		base := t.TempDir()
		rootPath := filepath.Join(base, "artifacts")
		s := openTestStore(t, rootPath)
		finalPath, err := s.pathFor(hash)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(finalPath), 0o755); err != nil {
			t.Fatal(err)
		}
		sentinel := filepath.Join(base, "outside-sentinel")
		if err := os.WriteFile(sentinel, data, 0o600); err != nil {
			t.Fatal(err)
		}
		requireSymlink(t, sentinel, finalPath)

		if _, err := s.Get(hash); err == nil {
			t.Fatal("Get accepted an artifact through an escaping final symlink")
		}
		before, err := os.ReadFile(sentinel)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Put(data, "text/plain"); err == nil {
			t.Fatal("Put accepted an artifact through an escaping final symlink")
		}
		after, err := os.ReadFile(sentinel)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(after, before) {
			t.Fatal("Put modified outside sentinel through final symlink")
		}
	})

	t.Run("temporary", func(t *testing.T) {
		base := t.TempDir()
		rootPath := filepath.Join(base, "artifacts")
		s := openTestStore(t, rootPath)
		finalPath, err := s.pathFor(hash)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(finalPath), 0o755); err != nil {
			t.Fatal(err)
		}
		const suffix = "forced-collision"
		s.tempSuffix = func() (string, error) { return suffix, nil }
		tempPath := filepath.Join(filepath.Dir(finalPath), "."+filepath.Base(finalPath)+".tmp-"+suffix)
		sentinel := filepath.Join(base, "outside-sentinel")
		assertSentinelUnchangedAfter(t, sentinel, func() {
			requireSymlink(t, sentinel, tempPath)
			if _, err := s.Put(data, "text/plain"); err == nil {
				t.Fatal("Put followed an escaping temporary-file symlink")
			}
		})
		if _, err := os.Lstat(finalPath); !os.IsNotExist(err) {
			t.Fatalf("final artifact unexpectedly created: %v", err)
		}
	})
}

func TestVerifyArtifactSuccessMissingCorruptAndCancel(t *testing.T) {
	s := openTestStore(t, filepath.Join(t.TempDir(), "arts"))
	data := []byte(strings.Repeat("verify-stream-exact-evidence\n", 128))
	meta, err := s.Put(data, "text/plain")
	if err != nil {
		t.Fatal(err)
	}

	if err := s.VerifyArtifact(context.Background(), meta.SHA256); err != nil {
		t.Fatalf("verify good artifact: %v", err)
	}
	if err := s.VerifyArtifact(context.Background(), strings.ToUpper(meta.SHA256)); err != nil {
		t.Fatalf("verify case-normalized hash: %v", err)
	}
	if err := s.VerifyArtifact(context.Background(), strings.Repeat("0", 64)); !errors.Is(err, ErrArtifactNotFound) {
		t.Fatalf("missing error=%v, want not found", err)
	}

	if err := os.WriteFile(meta.Path, []byte("tampered-stream-evidence"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.VerifyArtifact(context.Background(), meta.SHA256); !errors.Is(err, ErrArtifactCorrupt) {
		t.Fatalf("corrupt error=%v, want digest mismatch", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.VerifyArtifact(ctx, meta.SHA256); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled error=%v, want context.Canceled", err)
	}
	if err := s.VerifyArtifact(nil, meta.SHA256); err == nil {
		t.Fatal("nil context succeeded")
	}
}

func openTestStore(t *testing.T, dir string) *Store {
	t.Helper()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("close artifact store: %v", err)
		}
	})
	return s
}

func assertSentinelUnchangedAfter(t *testing.T, sentinel string, action func()) {
	t.Helper()
	want := []byte("outside sentinel must remain untouched")
	if err := os.WriteFile(sentinel, want, 0o600); err != nil {
		t.Fatal(err)
	}
	action()
	got, err := os.ReadFile(sentinel)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("outside sentinel changed: got %q, want %q", got, want)
	}
}

func requireSymlink(t *testing.T, oldname, newname string) {
	t.Helper()
	if err := os.Symlink(oldname, newname); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
}
