package config

import (
	"reflect"
	"sort"

	"github.com/knadh/koanf/v2"
)

// ChangedPaths returns the sorted leaf paths whose configured value differs
// between two Environments: each path whose value changed, plus each path
// only one of the two carries, so a section that appeared or disappeared is
// reported by the leaves it added or removed rather than by its root. A nil
// Environment counts as an empty configuration.
//
// It compares the trees the sources produced, before any Bind call synced
// default and zero values back into them (see Environment.loaded): a bound
// tree carries leaves nobody configured, so comparing bound views would
// report every defaulted leaf a later load reproduces as a change.
//
// Only paths are returned, never values. What sits at a changed path may be a
// credential -- plugin schemas mask their sensitive leaves -- so the result
// stays safe to print; a caller that wants to name the source of a change
// asks the same Environment for OriginsUnder(path).
func ChangedPaths(before, after *Environment) []string {
	beforeLeaves := loadedLeaves(before)
	afterLeaves := loadedLeaves(after)
	var changed []string
	for path, beforeValue := range beforeLeaves {
		if afterValue, ok := afterLeaves[path]; !ok || !reflect.DeepEqual(beforeValue, afterValue) {
			changed = append(changed, path)
		}
	}
	for path := range afterLeaves {
		if _, ok := beforeLeaves[path]; !ok {
			changed = append(changed, path)
		}
	}
	sort.Strings(changed)
	return changed
}

// loadedLeaves is one Environment's pre-bind snapshot; a nil Environment has
// none, which reads as the empty configuration everywhere above.
func loadedLeaves(env *Environment) map[string]any {
	if env == nil {
		return nil
	}
	return env.loaded
}

// captureLoaded deep-copies the flattened leaf values of a just-merged tree.
//
// The copy is taken through cloneCollections rather than through koanf's own
// copying helpers so that values keep their exact loaded types: the snapshot
// is compared with DeepEqual, and a value that came back as a different type
// on either side of a reload -- an int rendered as a float, say -- would read
// as a change that never happened.
func captureLoaded(k *koanf.Koanf) map[string]any {
	keys := k.Keys()
	loaded := make(map[string]any, len(keys))
	for _, path := range keys {
		loaded[path] = cloneCollections(k.Get(path))
	}
	return loaded
}
