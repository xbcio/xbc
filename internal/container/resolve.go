package container

import (
	"errors"
	"fmt"
	"reflect"
	"strings"

	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/topology"
)

// product is one declared output: a concrete type paired with the Instance
// that declared it. Built once per resolve() call from every instance's
// merged provide-side; backs both the exact (type, instance) index and the
// interface-assignability scan.
type product struct {
	typ   reflect.Type
	owner *Instance
}

// resolve owns dependency resolution. It merges both ends of the dependency
// graph, builds the product index, wires every hard and soft edge, and returns the
// topological order. Every problem that would block startup is collected
// and reported together as one "xbc: 依赖检查失败" batch -- fixing the first
// line reported should not just uncover a second one on the next run.
func (c *Container) resolve(insts []*Instance) ([]*Instance, []topology.Miss, error) {
	g := topology.New()
	byID := make(map[string]*Instance, len(insts))
	byPluginKey := make(map[plugin.Key][]*Instance, len(insts))
	for _, inst := range insts {
		g.AddNode(inst.ID())
		byID[inst.ID()] = inst
		byPluginKey[inst.key] = append(byPluginKey[inst.key], inst)
	}

	// Pass 0: scan xbc tags. This is the only place production code fills
	// inst.fields.
	for _, inst := range insts {
		fs, err := scanPluginFields(inst.plugin)
		if err != nil {
			return nil, nil, fmt.Errorf("xbc: 插件 %s 的 xbc tag 有误：%w", inst.Label(), err)
		}
		inst.fields = fs
	}

	// Pass 1: merge tag + explicit declarations once per instance, caching
	// the result on the instance itself so the caller's inject/harvest step
	// does not need to re-scan tags or re-call Dependencies()/Provides().
	for _, inst := range insts {
		deps, err := mergeDeps(inst)
		if err != nil {
			return nil, nil, err
		}
		provides, err := mergeProvides(inst)
		if err != nil {
			return nil, nil, err
		}
		inst.deps = deps
		inst.provides = provides
	}

	// Pass 2: build the product index; a (type, instance) collision is a
	// startup-blocking error just like a missing dependency, so it goes into
	// the same errLines batch instead of aborting immediately.
	produced := make(map[registryKey]*Instance)
	var allProducts []product
	productsByInstance := make(map[string][]product)
	var errLines []string
	for _, inst := range insts {
		instName := plugin.NormalizeInstance(inst.instance)
		for _, dep := range inst.provides {
			key := registryKey{typ: dep.Type, instance: instName}
			if other, ok := produced[key]; ok {
				if other == inst {
					// Same instance declared the same product twice (e.g. once
					// via tag, once via Provides()) -- harmless duplicate, not
					// a conflict between two different plugins.
					continue
				}
				errLines = append(errLines, fmt.Sprintf(
					"  插件 %s 与插件 %s 都声明产出 %s\n    → 产出同一类型、同一实例名的插件只能有一个",
					other.Label(), inst.Label(), dep.String()))
				continue
			}
			produced[key] = inst
			p := product{typ: dep.Type, owner: inst}
			allProducts = append(allProducts, p)
			productsByInstance[instName] = append(productsByInstance[instName], p)
		}
	}

	// Pass 3: resolve Deps.Types (concrete or interface) against the index.
	for _, inst := range insts {
		for _, dep := range inst.deps.Types {
			if line := resolveTypeDep(g, inst, dep, produced, allProducts, productsByInstance); line != "" {
				errLines = append(errLines, line)
			}
		}
	}

	// Pass 4: resolve Deps.Plugins (Ref) -- presence, optionally narrowed to
	// one named instance.
	for _, inst := range insts {
		for _, ref := range inst.deps.Plugins {
			if line := resolveRef(g, inst, ref, byPluginKey); line != "" {
				errLines = append(errLines, line)
			}
		}
	}

	if len(errLines) > 0 {
		return nil, nil, fmt.Errorf("xbc: 依赖检查失败\n%s", strings.Join(errLines, "\n"))
	}

	// Pass 5: only once every hard requirement is satisfiable do we wire the
	// soft After/Before edges.
	var misses []topology.Miss
	for _, inst := range insts {
		for _, name := range inst.deps.After {
			targets, ok := byPluginKey[name]
			if !ok {
				misses = append(misses, topology.Miss{Node: inst.ID(), Ref: name.String(), Dir: topology.After})
				continue
			}
			for _, target := range targets {
				g.AddSoftEdge(target.ID(), inst.ID())
			}
		}
		for _, name := range inst.deps.Before {
			targets, ok := byPluginKey[name]
			if !ok {
				misses = append(misses, topology.Miss{Node: inst.ID(), Ref: name.String(), Dir: topology.Before})
				continue
			}
			for _, target := range targets {
				g.AddSoftEdge(inst.ID(), target.ID())
			}
		}
	}

	order, sortMisses, err := g.Sort()
	if err != nil {
		var cycle *topology.CycleError
		if errors.As(err, &cycle) {
			return nil, nil, fmt.Errorf("xbc: 依赖成环\n  %s", strings.Join(cycle.Path, " → "))
		}
		// Every hard edge added above only ever names an endpoint already
		// confirmed to exist, so a *topology.MissingNodeError here would mean
		// a bug in this file, not a user-facing configuration mistake.
		return nil, nil, err
	}
	misses = append(misses, sortMisses...)

	sorted := make([]*Instance, len(order))
	for i, id := range order {
		sorted[i] = byID[id]
	}
	return sorted, misses, nil
}

// resolveTypeDep resolves one Dep from Deps.Types against the product
// index. It adds a hard edge and returns "" on success (including the
// "optional and absent" case, which is success too), or returns one
// already-indented, ready-to-join error line otherwise.
func resolveTypeDep(g *topology.Graph, inst *Instance, dep plugin.Dep, produced map[registryKey]*Instance, allProducts []product, productsByInstance map[string][]product) string {
	instName := plugin.NormalizeInstance(dep.Instance)

	if dep.Type.Kind() == reflect.Interface {
		candidates := matchInterface(productsByInstance[instName], dep.Type)
		switch len(candidates) {
		case 1:
			g.AddHardEdge(candidates[0].owner.ID(), inst.ID())
			return ""
		case 0:
			if dep.Optional {
				return ""
			}
			closest, missing := closestProductMatch(allProducts, dep.Type)
			if closest == nil {
				return fmt.Sprintf("  插件 %s 需要 %s，无任何插件提供\n    → 是否忘了 import 提供该类型的插件包",
					inst.Label(), dep.Type.String())
			}
			return fmt.Sprintf("  插件 %s 需要 %s，无任何插件提供\n    最接近的是 %s，缺少方法：%s",
				inst.Label(), dep.Type.String(), closest.String(), strings.Join(missing, "、"))
		default:
			var b strings.Builder
			fmt.Fprintf(&b, "  插件 %s 需要 %s，有 %d 个候选\n", inst.Label(), dep.Type.String(), len(candidates))
			for _, c := range candidates {
				fmt.Fprintf(&b, "    %s[%s]\n", c.typ.String(), instName)
			}
			b.WriteString(`    → 用 xbc:"inject,name=xxx" 指定实例消歧`)
			return b.String()
		}
	}

	key := registryKey{typ: dep.Type, instance: instName}
	if owner, ok := produced[key]; ok {
		g.AddHardEdge(owner.ID(), inst.ID())
		return ""
	}
	if dep.Optional {
		return ""
	}

	// Same concrete type exists, just not under the instance name asked
	// for -- list every instance that does produce it, so the fix is
	// "add that instance", not "guess what's wrong".
	var alt []string
	for _, p := range allProducts {
		if p.typ == dep.Type {
			alt = append(alt, fmt.Sprintf("%s[%s]", pkgName(dep.Type), plugin.NormalizeInstance(p.owner.instance)))
		}
	}
	if len(alt) == 0 {
		return fmt.Sprintf("  插件 %s 需要 %s，无任何插件提供\n    → 是否忘了 import 对应的 provider/autoload 包",
			inst.Label(), dep.Type.String())
	}
	return fmt.Sprintf("  插件 %s 依赖 %s[%s]，当前只有 %s\n    → 在 plugins.%s 下添加 %s 实例",
		inst.Label(), pkgName(dep.Type), instName, strings.Join(alt, "、"), pkgName(dep.Type), instName)
}

// resolveRef resolves one Ref by the stable Definition key. An unspecified
// instance creates an edge from every enabled instance under that key;
// Instance(name) narrows the dependency to one instance. The live
// implementation type is deliberately irrelevant: one type may back several
// keys, and one key may change its implementation without breaking dependants.
func resolveRef(g *topology.Graph, inst *Instance, ref plugin.Ref, byKey map[plugin.Key][]*Instance) string {
	wantKey := ref.Key()
	wantInstance := ref.InstanceName()
	all := byKey[wantKey]

	var matches []*Instance
	for _, other := range all {
		if wantInstance != "" && other.instance != plugin.NormalizeInstance(wantInstance) {
			continue
		}
		matches = append(matches, other)
	}
	if len(matches) > 0 {
		for _, match := range matches {
			g.AddHardEdge(match.ID(), inst.ID())
		}
		return ""
	}

	name := wantKey.String()
	if wantInstance == "" || len(all) == 0 {
		return fmt.Sprintf("  插件 %s 依赖插件 %s，但 %s 未启用\n    → 在 application.yml 中添加 plugins.%s 配置节",
			inst.Label(), name, name, name)
	}

	alt := make([]string, 0, len(all))
	for _, other := range all {
		alt = append(alt, fmt.Sprintf("%s[%s]", name, plugin.NormalizeInstance(other.instance)))
	}
	return fmt.Sprintf("  插件 %s 依赖插件 %s[%s]，当前只有 %s\n    → 在 plugins.%s 下添加 %s 实例",
		inst.Label(), name, plugin.NormalizeInstance(wantInstance), strings.Join(alt, "、"), name, plugin.NormalizeInstance(wantInstance))
}

// matchInterface scans products already scoped to one instance name and
// returns every one whose concrete type is assignable to want.
func matchInterface(products []product, want reflect.Type) []product {
	var out []product
	for _, p := range products {
		if p.typ.AssignableTo(want) {
			out = append(out, p)
		}
	}
	return out
}

// closestProductMatch finds, among every declared product in the whole app
// (deliberately not scoped to one instance -- this is a diagnostic aid, not
// a candidate list), the concrete type implementing the most of want's
// methods by name, and reports which method names it is still missing. Ties
// keep the first-declared type, since allProducts is already in
// registration order.
func closestProductMatch(allProducts []product, want reflect.Type) (reflect.Type, []string) {
	var bestType reflect.Type
	var bestMissing []string
	bestScore := -1
	for _, p := range allProducts {
		missing := missingMethods(p.typ, want)
		score := want.NumMethod() - len(missing)
		if score > bestScore {
			bestScore = score
			bestType = p.typ
			bestMissing = missing
		}
	}
	return bestType, bestMissing
}

// missingMethods lists the methods of iface that typ's method set lacks --
// by name, but a name match with a mismatched signature counts as missing
// too.
func missingMethods(typ, iface reflect.Type) []string {
	var missing []string
	for i := 0; i < iface.NumMethod(); i++ {
		want := iface.Method(i)
		m, ok := typ.MethodByName(want.Name)
		if !ok || !methodSignatureMatches(m, want) {
			missing = append(missing, want.Name)
		}
	}
	return missing
}

// methodSignatureMatches compares a concrete type's method -- whose Type
// includes the receiver as an implicit first input, per reflect's rule for
// any non-interface type -- against an interface method's signature, which
// has no receiver at all.
func methodSignatureMatches(concrete, iface reflect.Method) bool {
	ct := concrete.Type
	if ct.NumIn() < 1 {
		return false
	}
	if ct.NumIn()-1 != iface.Type.NumIn() || ct.NumOut() != iface.Type.NumOut() {
		return false
	}
	for i := 0; i < iface.Type.NumIn(); i++ {
		if ct.In(i+1) != iface.Type.In(i) {
			return false
		}
	}
	for i := 0; i < iface.Type.NumOut(); i++ {
		if ct.Out(i) != iface.Type.Out(i) {
			return false
		}
	}
	return true
}

// pkgName guesses a human name for typ from its package path -- dereferencing
// pointers first, since every product in this framework is provided as a
// pointer type. It operates on a real reflect.Type (plugin.Dep.Type and, via
// Ref.Type(), plugin.Ref are both exported as such), so it can call
// PkgPath() directly with full fidelity.
func pkgName(typ reflect.Type) string {
	t := typ
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	pkgPath := t.PkgPath()
	if pkgPath == "" {
		return t.String()
	}
	segs := strings.Split(pkgPath, "/")
	last := segs[len(segs)-1]
	if len(segs) >= 2 && isMajorVersionSegment(last) {
		last = segs[len(segs)-2]
	}
	return last
}

// isMajorVersionSegment reports whether s looks like a Go major-version path
// element ("v2", "v10", ...). This exists purely to serve pkgName's own
// diagnostics above -- it is container's own responsibility, not a mirror of
// anything in plugin (plugin no longer has an equivalent at all: gap 4 of
// the plugin/container split deleted plugin.deriveName and the
// isMajorVersionSegment helper that only served it, since Definition.Key is
// a plugin's sole identity and package-path-derived naming has no role left
// to play there).
func isMajorVersionSegment(s string) bool {
	if len(s) < 2 || s[0] != 'v' {
		return false
	}
	for _, r := range s[1:] {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
