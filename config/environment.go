package config

import (
	"fmt"
	"reflect"

	"github.com/knadh/koanf/providers/confmap"
	"github.com/knadh/koanf/v2"
)

// DefaultEnvPrefix is used when Options.EnvPrefix (for Load) or the envPrefix
// argument (for NewEnvironment) is left empty.
const DefaultEnvPrefix = "XBC_"

// Environment is the merged, read-only configuration environment. It is the
// protocol-agnostic, log-independent facade other packages bind against:
// everything reachable through Get/Exists/Sub/Bind comes from the same
// underlying koanf tree, built once by Load or NewEnvironment.
type Environment struct {
	k         *koanf.Koanf
	envPrefix string
}

// Load merges the configured sources and returns the resulting Environment.
func Load(opts Options) (*Environment, error) {
	if opts.EnvPrefix == "" {
		opts.EnvPrefix = DefaultEnvPrefix
	}
	k, err := loadKoanf(opts)
	if err != nil {
		return nil, err
	}
	return &Environment{k: k, envPrefix: opts.EnvPrefix}, nil
}

// NewEnvironment builds an Environment directly from an in-memory map,
// bypassing the file/profile lookup entirely. It is meant for tests and for
// embedding scenarios that already have their configuration as a Go value:
// it does not read any file, nor anything from the environment beyond what
// a later Bind call explicitly asks for.
func NewEnvironment(values map[string]any, envPrefix string) (*Environment, error) {
	if envPrefix == "" {
		envPrefix = DefaultEnvPrefix
	}
	k := koanf.New(".")
	if len(values) > 0 {
		if err := k.Load(confmap.Provider(values, "."), nil); err != nil {
			return nil, fmt.Errorf("xbc: failed to construct in-memory configuration environment: %w", err)
		}
	}
	return &Environment{k: k, envPrefix: envPrefix}, nil
}

// Get returns the value at path, or nil when absent. Maps and slices are
// recursively copied while scalar values keep their normal value semantics;
// mutating a returned collection therefore cannot alter later reads.
func (e *Environment) Get(path string) any {
	if e == nil || e.k == nil {
		return nil
	}
	return cloneCollections(e.k.Get(path))
}

// Exists reports whether path was set by any of the loaded layers.
//
// It answers two different questions depending on whether Bind has ever run
// against the section path falls under, because Bind's syncBack only ever
// writes back the leaves of the struct it was just asked to bind:
//
//   - Under a path that some caller has already run Bind against (the
//     framework's own typed schema): Exists answers "does this config item
//     exist" -- true for every reachable leaf on that schema, because
//     syncBack writes each final value into the underlying koanf tree,
//     including zero values. Leaves below an optional nil pointer are the
//     exception: Bind deliberately does not materialize or sync that subtree
//     unless file, ENV, or a default actually activated it.
//   - Under a path nobody has bound yet (freeform sections such as
//     "app.*" / "plugins.*" before their owner calls Bind): Exists answers
//     "did the user actually set this" -- true only when the file/ENV/
//     Overrides layers actually wrote it, because nothing has synced that
//     section back into the tree the way a bound section is.
//
// Pinned consequence for callers: anything that needs to tell "the user
// configured this" apart from "the framework filled this in with a zero or
// default value" -- a config doctor command, a config echo/dump endpoint,
// etc. -- must NOT use Exists for that purpose on a section that has already
// been Bind-ed. Doing so would report every schema leaf as user-configured.
// Making that distinction properly would require leaf to also carry whether
// a `default` tag exists and whether ENV/file actually hit it, and Bind's
// syncBack to only write back the leaves that were actually set -- a change
// this package deliberately does not make here.
func (e *Environment) Exists(path string) bool {
	if e == nil || e.k == nil {
		return false
	}
	return e.k.Exists(path)
}

// Sub returns an independent map at path, or nil when path is absent or not
// a map. Nested maps and slices are copied recursively, just as they are for
// Get, so mutation at any collection depth cannot leak back into Environment.
func (e *Environment) Sub(path string) map[string]any {
	if e == nil || e.k == nil {
		return nil
	}
	m, ok := cloneCollections(e.k.Get(path)).(map[string]any)
	if !ok {
		return nil
	}
	return m
}

// cloneCollections preserves concrete map and slice types while recursively
// copying their contents. Configuration trees are composed of acyclic values
// produced by koanf; pointers and structs are intentionally left alone rather
// than pretending this boundary is a general-purpose object graph cloner.
func cloneCollections(value any) any {
	if value == nil {
		return nil
	}
	return cloneCollectionValue(reflect.ValueOf(value)).Interface()
}

func cloneCollectionValue(value reflect.Value) reflect.Value {
	switch value.Kind() {
	case reflect.Interface:
		if value.IsNil() {
			return reflect.Zero(value.Type())
		}
		cloned := cloneCollectionValue(value.Elem())
		out := reflect.New(value.Type()).Elem()
		out.Set(cloned)
		return out
	case reflect.Map:
		if value.IsNil() {
			return reflect.Zero(value.Type())
		}
		out := reflect.MakeMapWithSize(value.Type(), value.Len())
		iter := value.MapRange()
		for iter.Next() {
			out.SetMapIndex(iter.Key(), cloneCollectionValue(iter.Value()))
		}
		return out
	case reflect.Slice:
		if value.IsNil() {
			return reflect.Zero(value.Type())
		}
		out := reflect.MakeSlice(value.Type(), value.Len(), value.Len())
		for i := 0; i < value.Len(); i++ {
			out.Index(i).Set(cloneCollectionValue(value.Index(i)))
		}
		return out
	default:
		return value
	}
}

// Bind strictly applies the file/ENV/default chain onto out and syncs the
// resulting leaf values back into the Environment. Unknown fields are
// rejected. out must be a non-nil pointer to a struct.
func (e *Environment) Bind(path string, out any) error {
	return e.BindWithOptions(path, out, BindOptions{})
}

// BindOptions customizes the assembly-only Bind operation.
//
// AllowedKeys contains section-relative paths owned by the framework rather
// than by out's schema. Such keys are accepted by strict unknown-field checks;
// if no field in out matches them, the decoder ignores them. The assembly layer
// can, for example, pass []string{"enabled"} while binding a
// plugin-owned configuration schema.
type BindOptions struct {
	AllowedKeys []string
}

// BindWithOptions is Bind with a narrow allowlist for framework-owned keys.
// Callers should prefer Bind unless their section genuinely mixes schemas.
func (e *Environment) BindWithOptions(path string, out any, options BindOptions) error {
	if e == nil || e.k == nil {
		return fmt.Errorf("xbc: configuration not loaded, cannot bind %s", displayPath(path))
	}
	if err := bind(e.k, path, out, e.envPrefix, options.AllowedKeys...); err != nil {
		return err
	}
	return syncBack(e.k, path, out)
}

// syncBack merges out's fully-bound leaf values back into k under path, so
// that Environment.Get/Exists/Sub -- which only ever read k, never out --
// see the same ENV overlays and `default` tags that Bind just applied
// directly onto out's fields. Without this, a subtree built entirely from
// default tags (no config file, no ENV) would be invisible to
// Exists/Get/Sub even though it is very much part of the merged
// configuration Environment promises to expose.
//
// This walks the same leaves schema bind itself used, so the two agree on
// what a "leaf" is; confmap.Provider with "." as the delimiter is the same
// mechanism loadKoanf already uses to merge Options.Overrides into k.
func syncBack(k *koanf.Koanf, path string, out any) error {
	root, schema, err := schemaFor(out)
	if err != nil {
		return err
	}

	flat := make(map[string]any, len(schema.Leaves))
	for _, item := range schema.rootedLeaves(path) {
		field, exists := fieldByIndex(root, item.Index, false)
		if !exists {
			// Preserve a nil optional subtree: syncBack must not allocate it
			// merely to publish zero values into the Environment.
			continue
		}
		flat[item.Path] = field.Interface()
	}
	if len(flat) == 0 {
		return nil
	}
	return k.Load(confmap.Provider(flat, "."), nil)
}
