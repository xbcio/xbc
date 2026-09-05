package objectstorage

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"sort"
	"strings"
	"sync"
)

const (
	localDirectoryMode fs.FileMode = 0o750
	localFileMode      fs.FileMode = 0o640
)

type localStore struct {
	root           *os.Root
	maxObjectBytes int64
	maxListItems   int
	lifetime       context.Context
	cancel         context.CancelFunc

	operations sync.RWMutex
	stateMu    sync.Mutex
	closed     bool
	bodies     map[*managedBody]struct{}
	closeOnce  sync.Once
	closeErr   error
}

func newLocalStore(cfg Config) (*localStore, error) {
	if err := os.MkdirAll(cfg.Local.Directory, localDirectoryMode); err != nil {
		return nil, fmt.Errorf("objectstorage: create local root %q: %w", cfg.Local.Directory, err)
	}
	root, err := os.OpenRoot(cfg.Local.Directory)
	if err != nil {
		return nil, fmt.Errorf("objectstorage: open local root %q: %w", cfg.Local.Directory, err)
	}
	lifetime, cancel := context.WithCancel(context.Background())
	return &localStore{
		root:           root,
		maxObjectBytes: cfg.MaxObjectBytes,
		maxListItems:   cfg.MaxListItems,
		lifetime:       lifetime,
		cancel:         cancel,
		bodies:         make(map[*managedBody]struct{}),
	}, nil
}

func (s *localStore) begin(ctx context.Context) (context.Context, func(), error) {
	if err := validateContext(ctx); err != nil {
		return nil, nil, err
	}
	s.operations.RLock()
	s.stateMu.Lock()
	closed := s.closed
	s.stateMu.Unlock()
	if closed {
		s.operations.RUnlock()
		return nil, nil, ErrClosed
	}
	linked, cleanup := linkedContext(ctx, s.lifetime)
	return linked, func() {
		cleanup()
		s.operations.RUnlock()
	}, nil
}

func (s *localStore) Put(ctx context.Context, key string, body io.Reader, options PutOptions) (ObjectInfo, error) {
	if err := validateKey(key); err != nil {
		return ObjectInfo{}, err
	}
	if body == nil {
		return ObjectInfo{}, errors.New("objectstorage: put body is nil")
	}
	if options.Size < UnknownSize {
		return ObjectInfo{}, fmt.Errorf("objectstorage: invalid declared size %d", options.Size)
	}
	if options.Size > s.maxObjectBytes {
		return ObjectInfo{}, objectTooLarge(key, s.maxObjectBytes)
	}

	opCtx, done, err := s.begin(ctx)
	if err != nil {
		return ObjectInfo{}, err
	}
	defer done()

	parent, base, releaseParent, err := s.openParent(key, true)
	if err != nil {
		return ObjectInfo{}, fmt.Errorf("objectstorage: create parent for %q: %w", key, err)
	}
	defer releaseParent()
	if err := validatePutDestination(parent, base, key); err != nil {
		return ObjectInfo{}, err
	}
	temporary, file, err := createTemporary(parent)
	if err != nil {
		return ObjectInfo{}, fmt.Errorf("objectstorage: create temporary upload for %q: %w", key, err)
	}
	keep := false
	defer func() {
		if !keep {
			_ = file.Close()
			_ = parent.Remove(temporary)
		}
	}()

	limited := &boundedReader{ctx: opCtx, src: body, key: key, max: s.maxObjectBytes}
	written, err := io.CopyBuffer(file, limited, make([]byte, 32<<10))
	if err != nil {
		return ObjectInfo{}, fmt.Errorf("objectstorage: stream upload %q: %w", key, err)
	}
	if options.Size >= 0 && written != options.Size {
		return ObjectInfo{}, fmt.Errorf("objectstorage: %q declared %d bytes but streamed %d: %w", key, options.Size, written, ErrSizeMismatch)
	}
	if err := opCtx.Err(); err != nil {
		return ObjectInfo{}, err
	}
	if err := file.Sync(); err != nil {
		return ObjectInfo{}, fmt.Errorf("objectstorage: sync upload %q: %w", key, err)
	}
	info, err := file.Stat()
	if err != nil {
		return ObjectInfo{}, fmt.Errorf("objectstorage: stat upload %q: %w", key, err)
	}
	if err := file.Close(); err != nil {
		return ObjectInfo{}, fmt.Errorf("objectstorage: close upload %q: %w", key, err)
	}
	if err := opCtx.Err(); err != nil {
		return ObjectInfo{}, err
	}
	// Recheck immediately before rename. If an untrusted process installed a
	// terminal symlink while the stream was in flight, reject it rather than
	// replacing it. A replacement after this check is still safe: rename only
	// unlinks that directory entry and never follows its target.
	if err := validatePutDestination(parent, base, key); err != nil {
		return ObjectInfo{}, err
	}
	if err := parent.Rename(temporary, base); err != nil {
		return ObjectInfo{}, fmt.Errorf("objectstorage: commit upload %q: %w", key, err)
	}
	keep = true
	return localObjectInfo(key, info), nil
}

func createTemporary(parent *os.Root) (string, *os.File, error) {
	for range 16 {
		var random [16]byte
		if _, err := rand.Read(random[:]); err != nil {
			return "", nil, err
		}
		name := temporaryComponentPrefix + hex.EncodeToString(random[:]) + ".tmp"
		file, err := parent.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, localFileMode)
		if err == nil {
			return name, file, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return "", nil, err
		}
	}
	return "", nil, errors.New("could not allocate a unique temporary name")
}

func validatePutDestination(parent *os.Root, base, key string) error {
	info, err := parent.Lstat(base)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("objectstorage: inspect upload destination %q: %w", key, err)
	}
	if info.Mode()&fs.ModeSymlink != 0 {
		return invalidKey(key, "symbolic links are forbidden")
	}
	if !info.Mode().IsRegular() {
		return invalidKey(key, "upload destination is not a regular file")
	}
	return nil
}

// openParent walks one component at a time and rejects every symbolic link.
// Each opened directory is compared with the entry before and after opening,
// closing lstat/open replacement races. os.Root remains the final escape
// boundary if an untrusted process mutates the tree concurrently.
func (s *localStore) openParent(key string, create bool) (*os.Root, string, func(), error) {
	components := strings.Split(key, "/")
	base := components[len(components)-1]
	current := s.root
	owned := false
	cleanup := func() {
		if owned {
			_ = current.Close()
		}
	}
	fail := func(err error) (*os.Root, string, func(), error) {
		cleanup()
		return nil, "", nil, err
	}

	for _, component := range components[:len(components)-1] {
		info, err := current.Lstat(component)
		if errors.Is(err, fs.ErrNotExist) && create {
			if mkdirErr := current.Mkdir(component, localDirectoryMode); mkdirErr != nil && !errors.Is(mkdirErr, fs.ErrExist) {
				return fail(mkdirErr)
			}
			info, err = current.Lstat(component)
		}
		if err != nil {
			return fail(err)
		}
		next, err := openVerifiedDirectory(current, component, key, info)
		if err != nil {
			return fail(err)
		}
		if owned {
			_ = current.Close()
		}
		current = next
		owned = true
	}
	return current, base, cleanup, nil
}

func openVerifiedDirectory(parent *os.Root, component, key string, inspected fs.FileInfo) (*os.Root, error) {
	if inspected.Mode()&fs.ModeSymlink != 0 {
		return nil, invalidKey(key, "symbolic-link path components are forbidden")
	}
	if !inspected.IsDir() {
		return nil, fmt.Errorf("path component %q is not a directory", component)
	}
	current, err := parent.Lstat(component)
	if err != nil {
		return nil, err
	}
	if current.Mode()&fs.ModeSymlink != 0 || !current.IsDir() || !os.SameFile(inspected, current) {
		return nil, invalidKey(key, "directory changed before opening")
	}

	next, err := parent.OpenRoot(component)
	if err != nil {
		return nil, err
	}
	if err := verifyOpenedDirectory(parent, next, component, key, inspected); err != nil {
		_ = next.Close()
		return nil, err
	}
	return next, nil
}

func verifyOpenedDirectory(parent, openedRoot *os.Root, component, key string, inspected fs.FileInfo) error {
	opened, err := openedRoot.Stat(".")
	if err != nil {
		return err
	}
	current, err := parent.Lstat(component)
	if err != nil {
		return err
	}
	if current.Mode()&fs.ModeSymlink != 0 || !current.IsDir() ||
		!os.SameFile(inspected, current) || !os.SameFile(inspected, opened) {
		return invalidKey(key, "directory changed while opening")
	}
	return nil
}

func (s *localStore) openRegular(key string) (*os.File, fs.FileInfo, func(), error) {
	parent, base, releaseParent, err := s.openParent(key, false)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil, nil, notFound(key, err)
		}
		return nil, nil, nil, err
	}
	entryInfo, err := parent.Lstat(base)
	if err != nil {
		releaseParent()
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil, nil, notFound(key, err)
		}
		return nil, nil, nil, err
	}
	file, openedInfo, err := openVerifiedRegular(parent, base, key, entryInfo)
	if err != nil {
		releaseParent()
		return nil, nil, nil, err
	}
	return file, openedInfo, releaseParent, nil
}

func openVerifiedRegular(parent *os.Root, base, key string, inspected fs.FileInfo) (*os.File, fs.FileInfo, error) {
	if inspected.Mode()&fs.ModeSymlink != 0 {
		return nil, nil, invalidKey(key, "symbolic links are forbidden")
	}
	if !inspected.Mode().IsRegular() {
		return nil, nil, notFound(key, errors.New("not a regular file"))
	}
	current, err := parent.Lstat(base)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil, notFound(key, err)
		}
		return nil, nil, err
	}
	if current.Mode()&fs.ModeSymlink != 0 || !current.Mode().IsRegular() || !os.SameFile(inspected, current) {
		return nil, nil, invalidKey(key, "object changed before opening")
	}

	file, err := parent.Open(base)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil, notFound(key, err)
		}
		return nil, nil, err
	}
	valid := false
	defer func() {
		if !valid {
			_ = file.Close()
		}
	}()
	opened, err := file.Stat()
	if err != nil {
		return nil, nil, err
	}
	current, err = parent.Lstat(base)
	if err != nil {
		return nil, nil, err
	}
	if current.Mode()&fs.ModeSymlink != 0 || !current.Mode().IsRegular() ||
		!opened.Mode().IsRegular() || !os.SameFile(inspected, current) || !os.SameFile(inspected, opened) {
		return nil, nil, invalidKey(key, "object changed while opening")
	}
	valid = true
	return file, opened, nil
}

func (s *localStore) Get(ctx context.Context, key string) (*Object, error) {
	if err := validateKey(key); err != nil {
		return nil, err
	}
	opCtx, done, err := s.begin(ctx)
	if err != nil {
		return nil, err
	}
	keep := false
	defer func() {
		if !keep {
			done()
		}
	}()

	file, info, releaseParent, err := s.openRegular(key)
	if err != nil {
		return nil, fmt.Errorf("objectstorage: open %q: %w", key, err)
	}
	releaseParent()
	if info.Size() > s.maxObjectBytes {
		_ = file.Close()
		return nil, objectTooLarge(key, s.maxObjectBytes)
	}

	body := &managedBody{
		reader:  &boundedReader{ctx: opCtx, src: file, key: key, max: s.maxObjectBytes},
		closer:  file,
		cleanup: done,
	}
	body.onClose = s.removeBody
	s.stateMu.Lock()
	if s.closed {
		s.stateMu.Unlock()
		_ = body.Close()
		return nil, ErrClosed
	}
	s.bodies[body] = struct{}{}
	s.stateMu.Unlock()
	keep = true
	return &Object{Info: localObjectInfo(key, info), Body: body}, nil
}

func (s *localStore) removeBody(body *managedBody) {
	s.stateMu.Lock()
	delete(s.bodies, body)
	s.stateMu.Unlock()
}

func (s *localStore) Delete(ctx context.Context, key string) error {
	if err := validateKey(key); err != nil {
		return err
	}
	opCtx, done, err := s.begin(ctx)
	if err != nil {
		return err
	}
	defer done()
	if err := opCtx.Err(); err != nil {
		return err
	}
	parent, base, releaseParent, err := s.openParent(key, false)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("objectstorage: delete %q: %w", key, err)
	}
	defer releaseParent()
	info, err := parent.Lstat(base)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("objectstorage: delete %q: %w", key, err)
	}
	if info.Mode()&fs.ModeSymlink != 0 {
		return invalidKey(key, "symbolic links are forbidden")
	}
	if !info.Mode().IsRegular() {
		return notFound(key, errors.New("not a regular file"))
	}
	if err := parent.Remove(base); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("objectstorage: delete %q: %w", key, err)
	}
	return nil
}

func (s *localStore) Stat(ctx context.Context, key string) (ObjectInfo, error) {
	if err := validateKey(key); err != nil {
		return ObjectInfo{}, err
	}
	opCtx, done, err := s.begin(ctx)
	if err != nil {
		return ObjectInfo{}, err
	}
	defer done()
	if err := opCtx.Err(); err != nil {
		return ObjectInfo{}, err
	}
	file, info, releaseParent, err := s.openRegular(key)
	if err != nil {
		return ObjectInfo{}, fmt.Errorf("objectstorage: stat %q: %w", key, err)
	}
	closeErr := file.Close()
	releaseParent()
	if closeErr != nil {
		return ObjectInfo{}, fmt.Errorf("objectstorage: close stat handle %q: %w", key, closeErr)
	}
	if info.Size() > s.maxObjectBytes {
		return ObjectInfo{}, objectTooLarge(key, s.maxObjectBytes)
	}
	return localObjectInfo(key, info), nil
}

func (s *localStore) List(ctx context.Context, options ListOptions) (ListResult, error) {
	if err := validatePrefix(options.Prefix); err != nil {
		return ListResult{}, err
	}
	limit, err := listLimit(options.Limit, s.maxListItems)
	if err != nil {
		return ListResult{}, err
	}
	opCtx, done, err := s.begin(ctx)
	if err != nil {
		return ListResult{}, err
	}
	defer done()

	objects := make([]ObjectInfo, 0, limit+1)
	_, err = walkLocalDirectory(opCtx, s.root, "", options, limit, &objects)
	if err != nil {
		return ListResult{}, fmt.Errorf("objectstorage: list local objects: %w", err)
	}
	result := ListResult{Objects: objects}
	if len(result.Objects) > limit {
		result.Objects = result.Objects[:limit]
		result.NextCursor = result.Objects[len(result.Objects)-1].Key
	}
	return result, nil
}

type localWalkEntry struct {
	name  string
	order string
	info  fs.FileInfo
}

// walkLocalDirectory never reopens a descendant by an unchecked path. Every
// directory and regular file is opened from its verified parent handle and
// compared with lstat results on both sides of open, so replacement races fail
// the List instead of traversing an attacker-selected entry.
func walkLocalDirectory(ctx context.Context, directory *os.Root, prefix string, options ListOptions, limit int, objects *[]ObjectInfo) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	handle, err := directory.Open(".")
	if err != nil {
		return false, err
	}
	entries, readErr := handle.ReadDir(-1)
	closeErr := handle.Close()
	if readErr != nil || closeErr != nil {
		return false, errors.Join(readErr, closeErr)
	}

	verified := make([]localWalkEntry, 0, len(entries))
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		name := entry.Name()
		if strings.HasPrefix(name, temporaryComponentPrefix) {
			continue
		}
		info, err := directory.Lstat(name)
		if err != nil {
			return false, err
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			continue
		}
		order := name
		if info.IsDir() {
			order += "/"
		}
		verified = append(verified, localWalkEntry{name: name, order: order, info: info})
	}
	sort.Slice(verified, func(i, j int) bool { return verified[i].order < verified[j].order })

	for _, entry := range verified {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		key := entry.name
		if prefix != "" {
			key = prefix + "/" + entry.name
		}
		if entry.info.IsDir() {
			next, err := openVerifiedDirectory(directory, entry.name, key, entry.info)
			if err != nil {
				return false, err
			}
			stop, walkErr := walkLocalDirectory(ctx, next, key, options, limit, objects)
			verifyErr := verifyOpenedDirectory(directory, next, entry.name, key, entry.info)
			closeErr := next.Close()
			if walkErr != nil || verifyErr != nil || closeErr != nil {
				return false, errors.Join(walkErr, verifyErr, closeErr)
			}
			if stop {
				return true, nil
			}
			continue
		}
		if !entry.info.Mode().IsRegular() || !strings.HasPrefix(key, options.Prefix) || key <= options.Cursor {
			continue
		}
		file, info, err := openVerifiedRegular(directory, entry.name, key, entry.info)
		if err != nil {
			return false, err
		}
		if err := file.Close(); err != nil {
			return false, err
		}
		*objects = append(*objects, localObjectInfo(key, info))
		if len(*objects) > limit {
			return true, nil
		}
	}
	return false, nil
}

func listLimit(requested, maximum int) (int, error) {
	if requested < 0 {
		return 0, fmt.Errorf("objectstorage: list limit must not be negative")
	}
	if requested == 0 {
		return maximum, nil
	}
	if requested > maximum {
		return 0, fmt.Errorf("objectstorage: requested %d items, maximum is %d: %w", requested, maximum, ErrListLimitExceeded)
	}
	return requested, nil
}

func localObjectInfo(key string, info fs.FileInfo) ObjectInfo {
	return ObjectInfo{Key: key, Size: info.Size(), ModifiedAt: info.ModTime()}
}

func (s *localStore) Close() error {
	s.closeOnce.Do(func() {
		s.stateMu.Lock()
		s.closed = true
		s.cancel()
		bodies := make([]*managedBody, 0, len(s.bodies))
		for body := range s.bodies {
			bodies = append(bodies, body)
		}
		s.stateMu.Unlock()

		for _, body := range bodies {
			if err := body.Close(); err != nil {
				s.closeErr = errors.Join(s.closeErr, fmt.Errorf("objectstorage: close local reader: %w", err))
			}
		}
		s.operations.Lock()
		if err := s.root.Close(); err != nil {
			s.closeErr = errors.Join(s.closeErr, fmt.Errorf("objectstorage: close local root: %w", err))
		}
		s.operations.Unlock()
	})
	return s.closeErr
}
