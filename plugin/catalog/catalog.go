package catalog

import (
	"fmt"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"sync"

	"github.com/xbcio/xbc/plugin"
)

// Catalog is a mutable set of plugin definitions. Declare writes into it;
// Freeze closes it and yields the immutable Snapshot assembly consumes.
//
// Declare deliberately does no validation of its own -- see Freeze for why:
// a panic raised from inside a linked package's init() has a useless stack
// trace (it points at Go's own init-ordering machinery, not at the call
// site a human wrote), so every check that can produce a rich diagnostic is
// deferred to the one place that runs under a real call stack the caller
// controls.
type Catalog struct {
	mu     sync.Mutex
	defs   []plugin.Definition
	frozen bool

	once     sync.Once
	snapshot Snapshot
	err      error
}

// New creates an empty, unfrozen Catalog. Tests use this (plus Declare and
// Freeze) to build a private definition set that never touches the
// process-wide default catalog -- see Declare/Freeze below for that one.
func New() *Catalog {
	return &Catalog{}
}

// Declare records d in the catalog. It does not validate d -- see the type
// doc comment -- and it panics once the catalog has been frozen: nothing
// declared after Freeze has run could ever reach the Snapshot that
// assembly already consumed, so silently accepting it would just hide a
// bug (a plugin package's init() running too late, or a test declaring into
// an already-frozen catalog by mistake) behind a Declare call that looks
// like it worked.
func (c *Catalog) Declare(d plugin.Definition) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.frozen {
		panic("xbc: plugin directory is frozen, no more plugins can be declared after init()")
	}
	c.defs = append(c.defs, d)
}

// Freeze closes the catalog and validates every definition it holds:
// per-Definition validity (Definition.Validate: key shape, non-nil Factory,
// valid cardinality and activation) and cross-Definition duplicate keys. It is idempotent -- the
// first call does the work and caches both the resulting Snapshot and the
// resulting error (nil or not); every later call returns that exact same
// pair without touching c.defs again, so a caller can call Freeze from
// several places (or several goroutines) without worrying about it
// re-running validation or re-ordering definitions differently the second
// time.
//
// Declare's frozen flag is set the moment Freeze starts -- inside the
// mutex, before sync.Once even begins the (potentially slower) validation
// work -- so a Declare racing a Freeze that is already underway is
// rejected immediately rather than sometimes sneaking into c.defs after
// the snapshot has already been read out.
func (c *Catalog) Freeze() (Snapshot, error) {
	c.once.Do(func() {
		c.mu.Lock()
		c.frozen = true
		defs := make([]plugin.Definition, len(c.defs))
		copy(defs, c.defs)
		c.mu.Unlock()

		c.snapshot, c.err = freeze(defs)
	})
	return c.snapshot, c.err
}

// freeze validates and sorts defs into a Snapshot, or returns an aggregated
// error describing every problem found. It is a free function (not a
// Catalog method) because it operates purely on the []plugin.Definition
// value Freeze already copied out from under the mutex -- there is nothing
// left for it to need from *Catalog itself.
func freeze(defs []plugin.Definition) (Snapshot, error) {
	sorted := make([]plugin.Definition, len(defs))
	copy(sorted, defs)
	// Sorting by Key before doing anything else is what makes the result
	// "not dependent on init() order" (package-layout design §3.3 rule 3):
	// two Catalogs fed the same definitions in opposite Declare order must
	// produce byte-for-byte the same Snapshot.
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Key < sorted[j].Key })

	var problems []string
	seen := make(map[plugin.Key]plugin.Definition, len(sorted))
	ordered := make([]plugin.Definition, 0, len(sorted))

	for _, d := range sorted {
		if err := d.Validate(); err != nil {
			problems = append(problems, err.Error())
			continue
		}
		if prev, dup := seen[d.Key]; dup {
			problems = append(problems, fmt.Sprintf(
				"plugin key %q is declared more than once: %s conflicts with %s",
				d.Key, describeDefinition(prev), describeDefinition(d)))
			continue
		}
		seen[d.Key] = d
		ordered = append(ordered, d)
	}

	if len(problems) > 0 {
		return Snapshot{}, fmt.Errorf("xbc: failed to freeze plugin directory, found %d issues:\n  → %s",
			len(problems), strings.Join(problems, "\n  → "))
	}
	return Snapshot{defs: ordered}, nil
}

// describeDefinition renders enough about a Definition's origin to tell two
// conflicting declarations apart in an error message: its key and the
// source file/line its Factory was defined at. Using the Factory's own
// file/line (rather than, say, a pointer address) is what makes the
// description actually mean something to the person reading the error --
// it points them straight at the two init() call sites that collided.
func describeDefinition(d plugin.Definition) string {
	if d.Factory == nil {
		return fmt.Sprintf("%s (Factory is nil)", d.Key)
	}
	fn := runtime.FuncForPC(reflect.ValueOf(d.Factory).Pointer())
	if fn == nil {
		return d.Key.String()
	}
	file, line := fn.FileLine(fn.Entry())
	return fmt.Sprintf("%s (factory defined at %s:%d)", d.Key, file, line)
}

// Snapshot is an immutable, deterministically ordered set of definitions.
// Its zero value is the empty snapshot. There is no way to mutate one, and
// no way to build one except through Freeze.
type Snapshot struct {
	defs []plugin.Definition // sorted by Key; never mutated after freeze() builds it
}

// Len returns the number of definitions in the snapshot.
func (s Snapshot) Len() int { return len(s.defs) }

// Definitions returns a defensive copy of the snapshot's definitions,
// sorted by Key. Callers are free to mutate or reorder the returned slice
// without affecting this Snapshot or any other call to Definitions.
func (s Snapshot) Definitions() []plugin.Definition {
	out := make([]plugin.Definition, len(s.defs))
	copy(out, s.defs)
	return out
}

// Lookup returns the definition identified by key, if the snapshot has one. Since
// s.defs is always sorted by Key, this is a binary search rather than a
// linear scan.
func (s Snapshot) Lookup(key plugin.Key) (plugin.Definition, bool) {
	i := sort.Search(len(s.defs), func(i int) bool { return s.defs[i].Key >= key })
	if i < len(s.defs) && s.defs[i].Key == key {
		return s.defs[i], true
	}
	return plugin.Definition{}, false
}

// defaultCatalog is the process-wide catalog that Declare/Freeze operate
// on. Plugin modules call the package-level Declare from their init()
// functions; the root package's New/Run freezes it exactly once per
// process. Tests that need an isolated definition set use New() plus their
// own Catalog value instead of touching this one -- see Catalog's doc
// comment.
var defaultCatalog = New()

// Declare writes into the process-wide default catalog. Plugin modules call
// this from init(). Panics after the default catalog has been frozen.
func Declare(d plugin.Definition) {
	defaultCatalog.Declare(d)
}

// Freeze freezes the process-wide default catalog. Idempotent.
func Freeze() (Snapshot, error) {
	return defaultCatalog.Freeze()
}
