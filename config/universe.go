package config

import (
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/knadh/koanf/v2"
)

// SectionKind classifies how the framework interprets one owned configuration
// section. It decides both how environment variables under the section are
// spelled and whether the section accepts keys the framework does not know.
type SectionKind int

const (
	// SectionTyped is a section decoded from a single struct schema. Every
	// environment variable under it must name a leaf of that schema.
	SectionTyped SectionKind = iota
	// SectionInstanced repeats one struct schema under instance names that
	// only the configuration itself knows, so every environment variable
	// under it must carry an instance segment.
	SectionInstanced
	// SectionFreeform is a section the framework never interprets. It has no
	// schema and therefore no environment-variable spelling.
	SectionFreeform
	// SectionNamespace owns a path whose children are declared as sections in
	// their own right. It claims the prefix without accepting any key itself.
	SectionNamespace
)

// Section declares one owned configuration path. Declaring the complete set of
// Sections up front is what lets Load reject an unowned top-level key and what
// gives the environment layer a schema to resolve variable names against.
type Section struct {
	// Path is the dotted configuration path this section owns, for example
	// "log", "plugins" or "plugins.gorm".
	Path string
	// Owner names whoever claims Path, for diagnostics only.
	Owner string
	// Kind decides how Path is interpreted.
	Kind SectionKind
	// Schema is the struct type backing a SectionTyped or SectionInstanced
	// section. A nil Schema declares a section that carries no fields of its
	// own, which is legal for a plugin without a configuration struct.
	Schema reflect.Type
	// Toggle marks a section that carries the framework-owned "enabled" flag
	// beside its own schema.
	Toggle bool
}

// Universe is the frozen set of configuration sections a composition root
// owns. It is built once, before the configuration tree is assembled.
type Universe struct {
	sections []resolvedSection
	roots    []string
	// byPath answers "is this exact path a declared section", and prefixes
	// answers "is this path an unclaimed segment on the way to one". Together
	// they let the ownership walk descend a dotted ConfigPath such as
	// "plugins.group.actual" without any declared section at "plugins.group".
	byPath   map[string]Section
	prefixes map[string]bool
}

// resolvedSection is a Section with its environment-variable tables computed.
type resolvedSection struct {
	Section
	prefix string // environment name prefix, without the process prefix
	leaves []envLeaf
}

// envLeaf is one schema leaf addressable through an environment variable.
type envLeaf struct {
	suffix      string // "DSN", "POOL_MAX_IDLE"
	path        string // "dsn", "pool.max_idle", relative to the section
	typ         reflect.Type
	expressible bool
}

const enabledKey = "enabled"

var boolType = reflect.TypeOf(false)

// reservedEnvSuffixes are process-level variables that live under the same
// prefix as configuration but address the loader itself rather than a section.
var reservedEnvSuffixes = map[string]bool{"PROFILE": true}

// NewUniverse validates and freezes the declared sections.
func NewUniverse(sections ...Section) (*Universe, error) {
	universe := &Universe{
		byPath:   make(map[string]Section, len(sections)),
		prefixes: make(map[string]bool),
	}
	rootSet := make(map[string]bool)

	for _, section := range sections {
		if err := validateSectionPath(section.Path); err != nil {
			return nil, err
		}
		if previous, exists := universe.byPath[section.Path]; exists {
			return nil, fmt.Errorf("xbc: configuration section %s is claimed by both %s and %s",
				section.Path, previous.Owner, section.Owner)
		}
		universe.byPath[section.Path] = section
	}

	for _, section := range sections {
		if err := checkSectionParent(section, universe.byPath); err != nil {
			return nil, err
		}
		resolved, err := resolveSection(section)
		if err != nil {
			return nil, err
		}
		universe.sections = append(universe.sections, resolved)

		for cut := strings.LastIndexByte(section.Path, '.'); cut >= 0; cut = strings.LastIndexByte(section.Path[:cut], '.') {
			universe.prefixes[section.Path[:cut]] = true
		}
		if strings.ContainsRune(section.Path, '.') {
			// A nested section's root is claimed by the ancestor namespace it
			// had to nest inside, so it never registers a root of its own.
			continue
		}
		rootSet[section.Path] = true
	}

	for root := range rootSet {
		universe.roots = append(universe.roots, root)
	}
	sort.Strings(universe.roots)
	sort.Slice(universe.sections, func(i, j int) bool {
		return universe.sections[i].Path < universe.sections[j].Path
	})
	return universe, nil
}

func validateSectionPath(path string) error {
	if path == "" || strings.HasPrefix(path, ".") || strings.HasSuffix(path, ".") || strings.Contains(path, "..") {
		return fmt.Errorf("xbc: configuration section path must be a non-empty dotted path, got %q", path)
	}
	return nil
}

// checkSectionParent keeps nested sections confined to declared namespaces, so
// that a plugin cannot quietly graft a custom ConfigPath onto a closed schema.
func checkSectionParent(section Section, byPath map[string]Section) error {
	index := strings.LastIndexByte(section.Path, '.')
	if index < 0 {
		return nil
	}
	for parent := section.Path[:index]; ; {
		if owner, exists := byPath[parent]; exists {
			if owner.Kind != SectionNamespace {
				return fmt.Errorf("xbc: configuration section %s cannot nest inside %s, which is owned by %s",
					section.Path, parent, owner.Owner)
			}
			return nil
		}
		cut := strings.LastIndexByte(parent, '.')
		if cut < 0 {
			return fmt.Errorf("xbc: configuration section %s has no declared owner for its root %q",
				section.Path, parent)
		}
		parent = parent[:cut]
	}
}

func resolveSection(section Section) (resolvedSection, error) {
	resolved := resolvedSection{Section: section, prefix: envSegment(section.Path) + "_"}
	if section.Schema == nil {
		return resolved, nil
	}
	if section.Kind != SectionTyped && section.Kind != SectionInstanced {
		return resolvedSection{}, fmt.Errorf("xbc: configuration section %s declares a schema but is not a typed section",
			section.Path)
	}
	schema, err := schemaForType(section.Schema)
	if err != nil {
		return resolvedSection{}, fmt.Errorf("xbc: configuration section %s: %w", section.Path, err)
	}
	for _, item := range schema.Leaves {
		resolved.leaves = append(resolved.leaves, envLeaf{
			suffix:      envSegment(item.Path),
			path:        item.Path,
			typ:         item.Type,
			expressible: envExpressible(item.Type),
		})
	}
	return resolved, nil
}

// envSegment maps a configuration path fragment onto its environment-variable
// spelling. Dots and dashes both become underscores, which is what makes a
// hyphenated plugin key such as "plugins.request-id" addressable at all.
func envSegment(path string) string {
	return strings.ToUpper(strings.NewReplacer(".", "_", "-", "_").Replace(path))
}

// envExpressible reports whether a single environment variable can carry a
// complete value of typ. Anything else has to be configured in a file.
func envExpressible(typ reflect.Type) bool {
	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	if typ == reflect.TypeOf(time.Duration(0)) {
		return true
	}
	if typ.Implements(textUnmarshalerType) || reflect.PointerTo(typ).Implements(textUnmarshalerType) {
		return true
	}
	switch typ.Kind() {
	case reflect.String, reflect.Bool,
		reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64:
		return true
	case reflect.Slice:
		return typ.Elem().Kind() == reflect.String
	default:
		return false
	}
}

// Roots returns the declared top-level configuration keys, sorted.
func (u *Universe) Roots() []string {
	if u == nil {
		return nil
	}
	return append([]string(nil), u.roots...)
}

// checkOwnership rejects every configuration key no declared Section claims,
// at any depth. It is the single answer to "who does this block belong to",
// which is why a plugin renaming its section through a dotted ConfigPath gets
// the same protection as one sitting at a conventional path.
//
// The walk stops at the first declared section that is not a namespace: what
// lives inside a typed, instanced or freeform section is that section's own
// business, and strict bind rather than this walk answers for it.
func (u *Universe) checkOwnership(k *koanf.Koanf) error {
	if u == nil || k == nil {
		return nil
	}
	unowned := u.collectUnowned("", k.Raw())
	if len(unowned) == 0 {
		return nil
	}
	sort.Strings(unowned)

	var b strings.Builder
	b.WriteString("xbc: configuration contains a key that no plugin or framework section owns")
	byParent := make(map[string][]string)
	var parents []string
	for _, path := range unowned {
		parent := parentPath(path)
		if _, seen := byParent[parent]; !seen {
			parents = append(parents, parent)
		}
		byParent[parent] = append(byParent[parent], path)
	}
	sort.Strings(parents)
	for _, parent := range parents {
		for _, path := range byParent[parent] {
			fmt.Fprintf(&b, "\n  %s", path)
		}
		fmt.Fprintf(&b, "\n  %s", u.describeDeclared(parent))
	}
	return errors.New(b.String())
}

// collectUnowned walks one already-owned node and returns the full dotted paths
// of the keys below it that no section claims.
func (u *Universe) collectUnowned(parent string, node map[string]any) []string {
	var unowned []string
	for key, value := range node {
		path := key
		if parent != "" {
			path = parent + "." + key
		}
		section, declared := u.byPath[path]
		if declared && section.Kind != SectionNamespace {
			continue
		}
		if !declared && !u.prefixes[path] {
			unowned = append(unowned, path)
			continue
		}
		if value == nil {
			// "plugins:" with nothing under it claims no key at all.
			continue
		}
		child, nested := value.(map[string]any)
		if !nested {
			// A namespace holds sections, never a value of its own.
			unowned = append(unowned, path)
			continue
		}
		unowned = append(unowned, u.collectUnowned(path, child)...)
	}
	return unowned
}

// describeDeclared names the sections an unowned key could have meant: the ones
// declared directly beside it, which is the list a reader can act on.
func (u *Universe) describeDeclared(parent string) string {
	var siblings []string
	for _, section := range u.sections {
		if parentPath(section.Path) == parent {
			siblings = append(siblings, section.Path)
		}
	}
	scope := "top-level sections"
	if parent != "" {
		scope = "sections under " + parent
	}
	if len(siblings) == 0 {
		return "no " + scope + " are declared"
	}
	return "declared " + scope + ": " + strings.Join(siblings, ", ")
}

func parentPath(path string) string {
	if cut := strings.LastIndexByte(path, '.'); cut >= 0 {
		return path[:cut]
	}
	return ""
}

// envCandidate is one configuration path an environment variable could name.
type envCandidate struct {
	path        string
	typ         reflect.Type
	expressible bool
	hint        string // set when the shape cannot be expressed at this spelling
	// section and instance are set only for a path inside a SectionInstanced
	// section, and only when the instance name was discovered from the
	// variable name. They carry what checkInstanceCollision needs.
	section  string
	instance string
}

// envOverlay resolves every variable in environ that carries prefix into the
// configuration path it names. A variable that lands inside a declared section
// but matches no field, or whose shape has no unambiguous single-variable
// spelling, is an error rather than a silent no-op.
//
// existing is the tree merged from the lower layers, consulted only to reject
// an instance name that would fork rather than override. It may be nil.
func (u *Universe) envOverlay(prefix string, environ []string, existing *koanf.Koanf) (map[string]any, error) {
	values := make(map[string]any)
	var failures []error

	for _, entry := range environ {
		separator := strings.IndexByte(entry, '=')
		if separator < 0 || !strings.HasPrefix(entry, prefix) {
			continue
		}
		name, raw := entry[:separator], entry[separator+1:]
		rest := name[len(prefix):]
		if rest == "" || reservedEnvSuffixes[rest] {
			continue
		}

		candidates, claimed := u.resolve(prefix, rest)
		if !claimed {
			failures = append(failures, fmt.Errorf(
				"xbc: environment variable %s uses the reserved %s prefix but names no declared configuration section (%s)",
				name, prefix, strings.Join(u.roots, ", ")))
			continue
		}
		if err := checkInstanceCollision(name, candidates, existing); err != nil {
			failures = append(failures, err)
			continue
		}
		value, err := interpret(name, raw, candidates)
		if err != nil {
			failures = append(failures, err)
			continue
		}
		values[candidates[0].path] = value
	}

	if len(failures) > 0 {
		sort.Slice(failures, func(i, j int) bool { return failures[i].Error() < failures[j].Error() })
		return nil, errors.Join(failures...)
	}
	return values, nil
}

// checkInstanceCollision rejects an environment variable whose discovered
// instance name would fork an instance the configuration already declares
// instead of overriding it.
//
// Instance names may legally contain a dash, but envSegment maps a dash and an
// underscore onto the same environment spelling, so "my-db" and "my_db" are
// indistinguishable from the environment and instanceOf can only ever produce
// the underscore form. Without this check XBC_PLUGINS_STORE_MY_DB_DSN would
// silently create a *second* instance beside a file-declared "my-db" -- the
// exact class of silent failure the environment layer exists to remove.
func checkInstanceCollision(name string, candidates []envCandidate, existing *koanf.Koanf) error {
	if existing == nil {
		return nil
	}
	for _, candidate := range candidates {
		if candidate.instance == "" {
			continue
		}
		siblings := declaredInstances(existing, candidate.section)
		if siblings[candidate.instance] {
			continue // an exact match: the variable overrides it, as intended
		}
		var collisions []string
		for sibling := range siblings {
			if sibling != candidate.instance && envSegment(sibling) == envSegment(candidate.instance) {
				collisions = append(collisions, strconv.Quote(sibling))
			}
		}
		if len(collisions) == 0 {
			continue
		}
		sort.Strings(collisions)
		return fmt.Errorf(
			"xbc: environment variable %s names instance %q under %s, but the configuration already declares %s, which no environment variable can spell distinctly; rename that instance to %q or configure it in a file instead",
			name, candidate.instance, candidate.section, strings.Join(collisions, " and "), candidate.instance)
	}
	return nil
}

// declaredInstances lists the instance names already present under an
// instanced section in the lower configuration layers.
func declaredInstances(k *koanf.Koanf, sectionPath string) map[string]bool {
	names := make(map[string]bool)
	for key, value := range k.Cut(sectionPath).Raw() {
		if key == enabledKey {
			continue
		}
		if _, nested := value.(map[string]any); nested {
			names[key] = true
		}
	}
	return names
}

// interpret turns one raw environment value into a typed configuration value,
// once exactly one candidate path survives.
func interpret(name, raw string, candidates []envCandidate) (any, error) {
	switch {
	case len(candidates) == 0:
		return nil, fmt.Errorf("xbc: environment variable %s names no configuration field", name)
	case len(candidates) > 1:
		paths := make([]string, len(candidates))
		for index, candidate := range candidates {
			paths[index] = candidate.path
		}
		sort.Strings(paths)
		return nil, fmt.Errorf("xbc: environment variable %s is ambiguous, it could name %s; configure it in a file instead",
			name, strings.Join(paths, " or "))
	}

	candidate := candidates[0]
	if candidate.hint != "" {
		return nil, fmt.Errorf("xbc: environment variable %s names %s, %s", name, candidate.path, candidate.hint)
	}
	if !candidate.expressible {
		return nil, fmt.Errorf(
			"xbc: environment variable %s names %s, whose type %s cannot be carried by a single environment variable; configure it in a file instead",
			name, candidate.path, candidate.typ)
	}

	typ := candidate.typ
	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	target := reflect.New(typ).Elem()
	if err := setScalar(target, typ, raw); err != nil {
		// Neither the raw value nor the underlying parse error is included:
		// environment variables are the usual home of secrets, and every
		// strconv error quotes the input it rejected. Name plus expected type
		// is enough to diagnose the mistake.
		return nil, fmt.Errorf("xbc: environment variable %s cannot be parsed as %s", name, typ)
	}
	return target.Interface(), nil
}

// resolve maps a prefix-stripped variable name onto the configuration paths it
// could name, and reports whether it fell inside any declared section at all.
// prefix is the process-level environment prefix, carried only so that a
// diagnostic can spell a variable the way an operator would actually set it.
func (u *Universe) resolve(prefix, rest string) ([]envCandidate, bool) {
	var candidates []envCandidate
	claimed := false
	seen := make(map[string]bool)
	add := func(candidate envCandidate) {
		if seen[candidate.path] {
			return
		}
		seen[candidate.path] = true
		candidates = append(candidates, candidate)
	}

	for _, section := range u.sections {
		if !strings.HasPrefix(rest, section.prefix) {
			continue
		}
		claimed = true
		tail := rest[len(section.prefix):]
		if tail == "" {
			continue
		}
		switch section.Kind {
		case SectionNamespace:
			// The namespace claims the prefix; its children answer for it.
		case SectionFreeform:
			add(envCandidate{
				path: section.Path + "." + strings.ToLower(tail),
				hint: "which is a freeform section the framework never interprets; configure it in a file instead",
			})
		case SectionTyped:
			section.matchTyped(tail, add)
		case SectionInstanced:
			section.matchInstanced(prefix, tail, add)
		}
	}
	return candidates, claimed
}

func (section resolvedSection) matchTyped(tail string, add func(envCandidate)) {
	for _, item := range section.leaves {
		if tail == item.suffix {
			add(envCandidate{path: section.Path + "." + item.path, typ: item.typ, expressible: item.expressible})
		}
	}
	if section.Toggle && tail == "ENABLED" {
		add(envCandidate{path: section.Path + "." + enabledKey, typ: boolType, expressible: true})
	}
}

func (section resolvedSection) matchInstanced(prefix, tail string, add func(envCandidate)) {
	if section.Toggle && tail == "ENABLED" {
		add(envCandidate{path: section.Path + "." + enabledKey, typ: boolType, expressible: true})
	}
	if section.Toggle {
		if instance, ok := instanceOf(tail, "ENABLED"); ok {
			add(envCandidate{
				path:        section.Path + "." + instance + "." + enabledKey,
				typ:         boolType,
				expressible: true,
				section:     section.Path,
				instance:    instance,
			})
		}
	}
	for _, item := range section.leaves {
		if tail == item.suffix {
			add(envCandidate{
				path: section.Path + "." + item.path,
				typ:  item.typ,
				hint: fmt.Sprintf(
					"but %s holds one section per instance; insert the instance name, as in %s%s<INSTANCE>_%s",
					section.Path, prefix, section.prefix, item.suffix),
			})
			continue
		}
		if instance, ok := instanceOf(tail, item.suffix); ok {
			add(envCandidate{
				path:        section.Path + "." + instance + "." + item.path,
				typ:         item.typ,
				expressible: item.expressible,
				section:     section.Path,
				instance:    instance,
			})
		}
	}
}

// instanceOf splits "PRIMARY_POOL_MAX_IDLE" into instance "primary" for the
// leaf suffix "POOL_MAX_IDLE". This is the schema-independent enumeration
// channel: instance names are discovered from the environment, never from a
// static schema, which is what makes an ENV-only multi-instance deployment
// expressible at all.
//
// The recovered name can only ever use [a-z0-9_]. A dash is legal in an
// instance name declared in a file but has no distinct environment spelling,
// so checkInstanceCollision rejects the fork rather than letting it happen.
func instanceOf(tail, suffix string) (string, bool) {
	if len(tail) <= len(suffix)+1 || !strings.HasSuffix(tail, "_"+suffix) {
		return "", false
	}
	instance := strings.ToLower(tail[:len(tail)-len(suffix)-1])
	for _, r := range instance {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '_' {
			return "", false
		}
	}
	return instance, true
}
