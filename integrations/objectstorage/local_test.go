package objectstorage

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
)

func TestLocalStoreCRUDAndStreamingOverwrite(t *testing.T) {
	store, root := testLocalStore(t, 1<<20, 100)
	ctx := context.Background()

	info, err := store.Put(ctx, "reports/2026/summary.txt", strings.NewReader("first"), PutOptions{Size: UnknownSize})
	if err != nil {
		t.Fatalf("Put() error = %v", err)
	}
	if info.Key != "reports/2026/summary.txt" || info.Size != 5 || info.ModifiedAt.IsZero() {
		t.Fatalf("Put() info = %+v", info)
	}
	stat, err := store.Stat(ctx, info.Key)
	if err != nil || stat.Key != info.Key || stat.Size != 5 {
		t.Fatalf("Stat() = %+v, %v", stat, err)
	}
	object, err := store.Get(ctx, info.Key)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	got, err := io.ReadAll(object.Body)
	if err != nil {
		t.Fatalf("ReadAll() error = %v", err)
	}
	if err := object.Body.Close(); err != nil {
		t.Fatalf("Body.Close() error = %v", err)
	}
	if string(got) != "first" || object.Info.Size != 5 {
		t.Fatalf("Get() = %q, info %+v", got, object.Info)
	}

	if _, err := store.Put(ctx, info.Key, &oneByteReader{data: []byte("replacement")}, PutOptions{Size: 11}); err != nil {
		t.Fatalf("overwrite Put() error = %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(root, "reports", "2026", "summary.txt")); err != nil || string(got) != "replacement" {
		t.Fatalf("committed file = %q, %v", got, err)
	}
	if err := store.Delete(ctx, info.Key); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if err := store.Delete(ctx, info.Key); err != nil {
		t.Fatalf("idempotent Delete() error = %v", err)
	}
	if _, err := store.Get(ctx, info.Key); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get() missing error = %v, want ErrNotFound", err)
	}
	if _, err := store.Stat(ctx, info.Key); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Stat() missing error = %v, want ErrNotFound", err)
	}
}

func TestLocalStoreListPrefixPaginationAndLimits(t *testing.T) {
	store, _ := testLocalStore(t, 1024, 3)
	ctx := context.Background()
	for _, key := range []string{"images/c.png", "other/z.txt", "images/a.png", "images/nested/b.png"} {
		if _, err := store.Put(ctx, key, strings.NewReader(key), PutOptions{Size: int64(len(key))}); err != nil {
			t.Fatalf("Put(%q) error = %v", key, err)
		}
	}
	first, err := store.List(ctx, ListOptions{Prefix: "images/", Limit: 2})
	if err != nil {
		t.Fatalf("first List() error = %v", err)
	}
	if got := infoKeys(first.Objects); !slices.Equal(got, []string{"images/a.png", "images/c.png"}) {
		t.Fatalf("first keys = %v", got)
	}
	if first.NextCursor != "images/c.png" {
		t.Fatalf("NextCursor = %q", first.NextCursor)
	}
	second, err := store.List(ctx, ListOptions{Prefix: "images/", Cursor: first.NextCursor, Limit: 2})
	if err != nil {
		t.Fatalf("second List() error = %v", err)
	}
	if got := infoKeys(second.Objects); !slices.Equal(got, []string{"images/nested/b.png"}) || second.NextCursor != "" {
		t.Fatalf("second page = %+v", second)
	}
	all, err := store.List(ctx, ListOptions{})
	if err != nil {
		t.Fatalf("default-limit List() error = %v", err)
	}
	if len(all.Objects) != 3 || all.NextCursor == "" {
		t.Fatalf("default-limit page = %+v", all)
	}
	if _, err := store.List(ctx, ListOptions{Limit: 4}); !errors.Is(err, ErrListLimitExceeded) {
		t.Fatalf("oversized List() error = %v", err)
	}
	if _, err := store.List(ctx, ListOptions{Limit: -1}); err == nil {
		t.Fatal("negative List limit error = nil")
	}
}

func TestLocalStoreRejectsUnsafeKeysOnEveryOperation(t *testing.T) {
	store, _ := testLocalStore(t, 1024, 10)
	ctx := context.Background()
	keys := []string{
		"", ".", "..", "../secret", "a/../secret", "a/./secret", "/absolute",
		"a//b", "trailing/", `a\b`, "nul\x00byte", "C:/windows", "c:\\windows",
		".xbc-objectstorage-reserved", "dir/.xbc-objectstorage-reserved/file",
	}
	for _, key := range keys {
		t.Run(strings.ReplaceAll(key, "\x00", "NUL"), func(t *testing.T) {
			if _, err := store.Put(ctx, key, strings.NewReader("x"), PutOptions{Size: 1}); !errors.Is(err, ErrInvalidKey) {
				t.Fatalf("Put(%q) error = %v", key, err)
			}
			if _, err := store.Get(ctx, key); !errors.Is(err, ErrInvalidKey) {
				t.Fatalf("Get(%q) error = %v", key, err)
			}
			if _, err := store.Stat(ctx, key); !errors.Is(err, ErrInvalidKey) {
				t.Fatalf("Stat(%q) error = %v", key, err)
			}
			if err := store.Delete(ctx, key); !errors.Is(err, ErrInvalidKey) {
				t.Fatalf("Delete(%q) error = %v", key, err)
			}
		})
	}
	for _, prefix := range []string{"../", "/", `a\b`, "a//", "nul\x00"} {
		if _, err := store.List(ctx, ListOptions{Prefix: prefix}); !errors.Is(err, ErrInvalidKey) {
			t.Fatalf("List(prefix=%q) error = %v", prefix, err)
		}
	}
}

func TestLocalStoreOpenRootPreventsSymlinkEscape(t *testing.T) {
	store, root := testLocalStore(t, 1024, 10)
	outside := t.TempDir()
	secretPath := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secretPath, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("symlinks unavailable: %v", err)
		}
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := store.Get(ctx, "escape/secret.txt"); err == nil {
		t.Fatal("Get through escaping symlink succeeded")
	}
	if _, err := store.Put(ctx, "escape/secret.txt", strings.NewReader("changed"), PutOptions{Size: 7}); err == nil {
		t.Fatal("Put through escaping symlink succeeded")
	}
	if err := store.Delete(ctx, "escape/secret.txt"); err == nil {
		t.Fatal("Delete through escaping symlink succeeded")
	}
	got, err := os.ReadFile(secretPath)
	if err != nil || string(got) != "outside" {
		t.Fatalf("outside file changed: %q, %v", got, err)
	}
	listed, err := store.List(ctx, ListOptions{})
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(listed.Objects) != 0 {
		t.Fatalf("List() exposed symlink: %+v", listed.Objects)
	}
}

func TestLocalStoreRejectsTerminalSymlinksIncludingPutRace(t *testing.T) {
	store, root := testLocalStore(t, 1024, 10)
	target := filepath.Join(root, "target.txt")
	if err := os.WriteFile(target, []byte("outside-object"), 0o600); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(root, "alias.txt")
	if err := os.Symlink("target.txt", alias); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("symlinks unavailable: %v", err)
		}
		t.Fatal(err)
	}

	ctx := context.Background()
	if _, err := store.Put(ctx, "alias.txt", strings.NewReader("changed"), PutOptions{Size: 7}); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("Put terminal symlink error = %v", err)
	}
	if _, err := store.Get(ctx, "alias.txt"); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("Get terminal symlink error = %v", err)
	}
	if _, err := store.Stat(ctx, "alias.txt"); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("Stat terminal symlink error = %v", err)
	}
	if err := store.Delete(ctx, "alias.txt"); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("Delete terminal symlink error = %v", err)
	}
	if _, err := os.Lstat(alias); err != nil {
		t.Fatalf("rejected Delete removed symlink: %v", err)
	}

	gated := &gatedReader{
		reader:  strings.NewReader("new-data"),
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	putDone := make(chan error, 1)
	go func() {
		_, err := store.Put(ctx, "raced.txt", gated, PutOptions{Size: 8})
		putDone <- err
	}()
	<-gated.started
	raced := filepath.Join(root, "raced.txt")
	if err := os.Symlink("target.txt", raced); err != nil {
		close(gated.release)
		t.Fatal(err)
	}
	close(gated.release)
	if err := <-putDone; !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("Put with raced terminal symlink error = %v", err)
	}

	got, err := os.ReadFile(target)
	if err != nil || string(got) != "outside-object" {
		t.Fatalf("symlink target changed: %q, %v", got, err)
	}
	listed, err := store.List(ctx, ListOptions{})
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if got := infoKeys(listed.Objects); !slices.Equal(got, []string{"target.txt"}) {
		t.Fatalf("List() exposed terminal symlinks: %v", got)
	}
	for _, entry := range mustReadDir(t, root) {
		if strings.HasPrefix(entry.Name(), temporaryComponentPrefix) {
			t.Fatalf("temporary file leaked: %s", entry.Name())
		}
	}
}

func TestLocalDirectoryReplacementIsDetectedBeforeAndAfterOpen(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("renaming an open directory is platform-specific on Windows")
	}
	store, root := testLocalStore(t, 1024, 10)

	makeDirectory := func() fs.FileInfo {
		t.Helper()
		if err := os.RemoveAll(filepath.Join(root, "safe")); err != nil {
			t.Fatal(err)
		}
		if err := os.RemoveAll(filepath.Join(root, "moved")); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(filepath.Join(root, "safe"), 0o750); err != nil {
			t.Fatal(err)
		}
		info, err := store.root.Lstat("safe")
		if err != nil {
			t.Fatal(err)
		}
		return info
	}
	replaceDirectory := func() {
		t.Helper()
		if err := os.Rename(filepath.Join(root, "safe"), filepath.Join(root, "moved")); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(filepath.Join(root, "safe"), 0o750); err != nil {
			t.Fatal(err)
		}
	}

	inspected := makeDirectory()
	replaceDirectory()
	if opened, err := openVerifiedDirectory(store.root, "safe", "safe/object", inspected); !errors.Is(err, ErrInvalidKey) {
		if opened != nil {
			_ = opened.Close()
		}
		t.Fatalf("pre-open replacement error = %v", err)
	}

	inspected = makeDirectory()
	opened, err := store.root.OpenRoot("safe")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	replaceDirectory()
	if err := verifyOpenedDirectory(store.root, opened, "safe", "safe/object", inspected); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("post-open replacement error = %v", err)
	}
}

func TestLocalStoreFailedPutIsAtomicAndCleansTemporaryFile(t *testing.T) {
	store, root := testLocalStore(t, 1024, 10)
	ctx := context.Background()
	if _, err := store.Put(ctx, "stable.txt", strings.NewReader("old"), PutOptions{Size: 3}); err != nil {
		t.Fatal(err)
	}
	streamErr := errors.New("source failed")
	_, err := store.Put(ctx, "stable.txt", &failAfterReader{data: []byte("partial"), err: streamErr}, PutOptions{Size: UnknownSize})
	if !errors.Is(err, streamErr) {
		t.Fatalf("failed Put() error = %v", err)
	}
	got, err := os.ReadFile(filepath.Join(root, "stable.txt"))
	if err != nil || string(got) != "old" {
		t.Fatalf("atomic target = %q, %v", got, err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), temporaryComponentPrefix) {
			t.Fatalf("temporary file leaked: %s", entry.Name())
		}
	}
}

func TestLocalStoreEnforcesUploadDownloadAndDeclaredSizeLimits(t *testing.T) {
	store, root := testLocalStore(t, 4, 10)
	ctx := context.Background()
	if _, err := store.Put(ctx, "declared", strings.NewReader("12345"), PutOptions{Size: 5}); !errors.Is(err, ErrObjectTooLarge) {
		t.Fatalf("declared oversized Put() error = %v", err)
	}
	if _, err := store.Put(ctx, "streamed", strings.NewReader("12345"), PutOptions{Size: UnknownSize}); !errors.Is(err, ErrObjectTooLarge) {
		t.Fatalf("streamed oversized Put() error = %v", err)
	}
	if _, err := store.Put(ctx, "short", strings.NewReader("12"), PutOptions{Size: 3}); !errors.Is(err, ErrSizeMismatch) {
		t.Fatalf("short Put() error = %v", err)
	}
	if _, err := store.Put(ctx, "long", strings.NewReader("123"), PutOptions{Size: 2}); !errors.Is(err, ErrSizeMismatch) {
		t.Fatalf("long Put() error = %v", err)
	}
	for _, key := range []string{"declared", "streamed", "short", "long"} {
		if _, err := store.Stat(ctx, key); !errors.Is(err, ErrNotFound) {
			t.Fatalf("failed upload %q exists, Stat error = %v", key, err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "oversized"), []byte("12345"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(ctx, "oversized"); !errors.Is(err, ErrObjectTooLarge) {
		t.Fatalf("oversized Get() error = %v", err)
	}
	if _, err := store.Stat(ctx, "oversized"); !errors.Is(err, ErrObjectTooLarge) {
		t.Fatalf("oversized Stat() error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "growing"), []byte("1234"), 0o600); err != nil {
		t.Fatal(err)
	}
	object, err := store.Get(ctx, "growing")
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(filepath.Join(root, "growing"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = file.WriteString("5")
	_ = file.Close()
	_, err = io.ReadAll(object.Body)
	_ = object.Body.Close()
	if !errors.Is(err, ErrObjectTooLarge) {
		t.Fatalf("growing Body.Read error = %v", err)
	}
}

func TestLocalStoreHonorsContextAndCloseLifecycle(t *testing.T) {
	store, _ := testLocalStore(t, 1024, 10)
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := store.Put(cancelled, "x", strings.NewReader("x"), PutOptions{Size: 1}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled Put() error = %v", err)
	}
	if _, err := store.List(cancelled, ListOptions{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled List() error = %v", err)
	}
	if _, err := store.Stat(nil, "x"); err == nil {
		t.Fatal("Stat(nil) error = nil")
	}
	if _, err := store.Put(context.Background(), "open", strings.NewReader("payload"), PutOptions{Size: 7}); err != nil {
		t.Fatal(err)
	}
	readCtx, readCancel := context.WithCancel(context.Background())
	object, err := store.Get(readCtx, "open")
	if err != nil {
		t.Fatal(err)
	}
	readCancel()
	var one [1]byte
	if _, err := object.Body.Read(one[:]); !errors.Is(err, context.Canceled) {
		t.Fatalf("read after cancellation error = %v", err)
	}
	_ = object.Body.Close()
	object, err = store.Get(context.Background(), "open")
	if err != nil {
		t.Fatal(err)
	}
	const closers = 16
	var wg sync.WaitGroup
	errs := make(chan error, closers)
	wg.Add(closers)
	for range closers {
		go func() {
			defer wg.Done()
			errs <- store.Close()
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent Close() error = %v", err)
		}
	}
	if _, err := object.Body.Read(one[:]); !errors.Is(err, context.Canceled) {
		t.Fatalf("outstanding body after Close error = %v", err)
	}
	if err := object.Body.Close(); err != nil {
		t.Fatalf("repeated body Close() error = %v", err)
	}
	if _, err := store.Get(context.Background(), "open"); !errors.Is(err, ErrClosed) {
		t.Fatalf("Get after Close error = %v", err)
	}
	if _, err := store.Put(context.Background(), "new", strings.NewReader("x"), PutOptions{Size: 1}); !errors.Is(err, ErrClosed) {
		t.Fatalf("Put after Close error = %v", err)
	}
}

func testLocalStore(t *testing.T, maxBytes int64, maxItems int) (*localStore, string) {
	t.Helper()
	root := t.TempDir()
	cfg := defaultConfig()
	cfg.MaxObjectBytes = maxBytes
	cfg.MaxListItems = maxItems
	cfg.Local.Directory = root
	store, err := newLocalStore(cfg)
	if err != nil {
		t.Fatalf("newLocalStore() error = %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store, root
}

func infoKeys(infos []ObjectInfo) []string {
	keys := make([]string, len(infos))
	for i := range infos {
		keys[i] = infos[i].Key
	}
	return keys
}

type oneByteReader struct{ data []byte }

func (r *oneByteReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, io.EOF
	}
	p[0] = r.data[0]
	r.data = r.data[1:]
	return 1, nil
}

type failAfterReader struct {
	data []byte
	err  error
}

func (r *failAfterReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, r.err
	}
	n := copy(p, r.data)
	r.data = r.data[n:]
	return n, nil
}

type gatedReader struct {
	reader  io.Reader
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (r *gatedReader) Read(p []byte) (int, error) {
	r.once.Do(func() {
		close(r.started)
		<-r.release
	})
	return r.reader.Read(p)
}

func mustReadDir(t *testing.T, directory string) []os.DirEntry {
	t.Helper()
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	return entries
}
