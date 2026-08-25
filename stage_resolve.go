package xbc

import (
	"errors"
	"fmt"
	"reflect"
	"strings"

	"github.com/xbcio/xbc/internal/graph"
	"github.com/xbcio/xbc/internal/inject"
)

// product is one declared output: a concrete type paired with the instance
// that declared it. Built once per resolve() call from every instance's
// merged provide-side; backs both the exact (type, instance) index and the
// interface-assignability scan.
type product struct {
	typ   reflect.Type
	owner *instance
}

// mergeDeps combines the two dependency-declaration paths from spec §5.7:
// the "inject" tags scanned into inst.fields by pass 0 above, and the
// optional Declarer.Dependencies() method. A plugin may use either, or mix
// both on the same instance (AuditPlugin in the spec: DB field via tag,
// Plugins via Dependencies()) -- this function is where the two paths become
// one Deps value, so everything downstream treats them identically.
func mergeDeps(inst *instance) Deps {
	var out Deps
	for _, fs := range inst.fields {
		if fs.Kind != inject.KindInject {
			continue
		}
		out.Types = append(out.Types, Dep{Type: fs.Type, Instance: fs.Instance, Optional: fs.Optional})
	}
	if d, ok := inst.plugin.(Declarer); ok {
		explicit := d.Dependencies()
		out.Types = append(out.Types, explicit.Types...)
		out.Plugins = append(out.Plugins, explicit.Plugins...)
		out.After = append(out.After, explicit.After...)
		out.Before = append(out.Before, explicit.Before...)
	}
	return out
}

// mergeProvides mirrors mergeDeps on the producing side of the graph:
// "provide" tags plus the optional Provider.Provides() method. A provide
// tag's FieldSpec.Instance is always "" (inject.Scan rejects "name=" on a
// provide tag, per the API contract) -- the produced instance name is always
// the owning instance's own name, decided later by the caller, never by the
// tag itself. That is why this function never reads fs.Instance.
func mergeProvides(inst *instance) []Dep {
	var out []Dep
	for _, fs := range inst.fields {
		if fs.Kind != inject.KindProvide {
			continue
		}
		out = append(out, Dep{Type: fs.Type})
	}
	if p, ok := inst.plugin.(Provider); ok {
		out = append(out, p.Provides()...)
	}
	return out
}

// resolve is stage 4. It merges both ends of the dependency graph, builds
// the product index, wires every hard and soft edge, and returns the
// topological order. Every problem that would block startup is collected
// and reported together as one "xbc: 依赖检查失败" batch -- fixing the first
// line reported should not just uncover a second one on the next run.
func (a *App) resolve(insts []*instance) ([]*instance, []graph.Miss, error) {
	g := graph.New()
	byID := make(map[string]*instance, len(insts))
	byPluginName := make(map[string][]*instance, len(insts))
	for _, inst := range insts {
		g.AddNode(inst.id())
		byID[inst.id()] = inst
		byPluginName[inst.name] = append(byPluginName[inst.name], inst)
	}

	// Pass 0: scan xbc tags. This is the only place production code fills
	// inst.fields -- stage 2 builds instances but has no business reading
	// tags, and stage 5 needs the scan to have already happened so it can
	// inject and harvest without re-scanning. A malformed tag aborts
	// immediately rather than joining the errLines batch: the batch exists
	// to report every *configuration* problem at once, and a tag typo is a
	// compile-time-shaped mistake whose own message already says exactly
	// which field is wrong.
	for _, inst := range insts {
		fs, err := inject.Scan(inst.plugin)
		if err != nil {
			return nil, nil, fmt.Errorf("xbc: 插件 %s 的 xbc tag 有误：%w", inst.label(), err)
		}
		inst.fields = fs
	}

	// Pass 1: merge tag + explicit declarations once per instance, caching
	// the result on the instance itself so stage 5's harvest-and-verify step
	// does not need to re-scan tags or re-call Dependencies()/Provides().
	for _, inst := range insts {
		inst.deps = mergeDeps(inst)
		inst.provides = mergeProvides(inst)
	}

	// Pass 2: build the product index; a (type, instance) collision is a
	// startup-blocking error just like a missing dependency, so it goes into
	// the same errLines batch instead of aborting immediately.
	produced := make(map[registryKey]*instance)
	var allProducts []product
	productsByInstance := make(map[string][]product)
	var errLines []string
	for _, inst := range insts {
		instName := normInstance(inst.instance)
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
					other.label(), inst.label(), dep.String()))
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
			if line := resolveRef(g, inst, ref, insts); line != "" {
				errLines = append(errLines, line)
			}
		}
	}

	if len(errLines) > 0 {
		return nil, nil, fmt.Errorf("xbc: 依赖检查失败\n%s", strings.Join(errLines, "\n"))
	}

	// Pass 5: only once every hard requirement is satisfiable do we wire the
	// soft After/Before edges. After/Before name a *plugin*, and one plugin
	// name can fan out to several instances (multi-instance plugin) -- that
	// fan-out has to happen here, in Go, before anything reaches the graph,
	// because graph.AddEdge only ever sees one literal node id per call and
	// has no notion of "this name means N nodes". A name that matches no
	// instance at all becomes a Miss we build ourselves, not something we
	// hand to the graph package to detect.
	var misses []graph.Miss
	for _, inst := range insts {
		for _, name := range inst.deps.After {
			targets, ok := byPluginName[name]
			if !ok {
				misses = append(misses, graph.Miss{Node: inst.id(), Ref: name, Dir: "after"})
				continue
			}
			for _, target := range targets {
				g.AddEdge(target.id(), inst.id(), false)
			}
		}
		for _, name := range inst.deps.Before {
			targets, ok := byPluginName[name]
			if !ok {
				misses = append(misses, graph.Miss{Node: inst.id(), Ref: name, Dir: "before"})
				continue
			}
			for _, target := range targets {
				g.AddEdge(inst.id(), target.id(), false)
			}
		}
	}

	order, sortMisses, err := g.Sort()
	if err != nil {
		var cycle *graph.CycleError
		if errors.As(err, &cycle) {
			return nil, nil, fmt.Errorf("xbc: 依赖成环\n  %s", strings.Join(cycle.Path, " → "))
		}
		// Every hard edge we added above only ever names an endpoint we had
		// already confirmed exists, so a *graph.MissingNodeError here would
		// mean a bug in this file, not a user-facing configuration mistake.
		return nil, nil, err
	}
	misses = append(misses, sortMisses...)

	sorted := make([]*instance, len(order))
	for i, id := range order {
		sorted[i] = byID[id]
	}
	return sorted, misses, nil
}

// resolveTypeDep resolves one Dep from Deps.Types against the product
// index. It adds a hard edge and returns "" on success (including the
// "optional and absent" case, which is success too), or returns one
// already-indented, ready-to-join error line otherwise.
func resolveTypeDep(g *graph.Graph, inst *instance, dep Dep, produced map[registryKey]*instance, allProducts []product, productsByInstance map[string][]product) string {
	instName := normInstance(dep.Instance)

	if dep.Type.Kind() == reflect.Interface {
		candidates := matchInterface(productsByInstance[instName], dep.Type)
		switch len(candidates) {
		case 1:
			g.AddEdge(candidates[0].owner.id(), inst.id(), true)
			return ""
		case 0:
			if dep.Optional {
				return ""
			}
			closest, missing := closestProductMatch(allProducts, dep.Type)
			if closest == nil {
				return fmt.Sprintf("  插件 %s 需要 %s，无任何插件提供\n    → 是否忘了 import 提供该类型的插件包",
					inst.label(), dep.Type.String())
			}
			return fmt.Sprintf("  插件 %s 需要 %s，无任何插件提供\n    最接近的是 %s，缺少方法：%s",
				inst.label(), dep.Type.String(), closest.String(), strings.Join(missing, "、"))
		default:
			var b strings.Builder
			fmt.Fprintf(&b, "  插件 %s 需要 %s，有 %d 个候选\n", inst.label(), dep.Type.String(), len(candidates))
			for _, c := range candidates {
				fmt.Fprintf(&b, "    %s[%s]\n", c.typ.String(), instName)
			}
			b.WriteString(`    → 用 xbc:"inject,name=xxx" 指定实例消歧`)
			return b.String()
		}
	}

	key := registryKey{typ: dep.Type, instance: instName}
	if owner, ok := produced[key]; ok {
		g.AddEdge(owner.id(), inst.id(), true)
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
			alt = append(alt, fmt.Sprintf("%s[%s]", pkgName(dep.Type), normInstance(p.owner.instance)))
		}
	}
	if len(alt) == 0 {
		return fmt.Sprintf("  插件 %s 需要 %s，无任何插件提供\n    → 是否忘了 import github.com/xbcio/xbc/plugins/%s",
			inst.label(), dep.Type.String(), pkgName(dep.Type))
	}
	return fmt.Sprintf("  插件 %s 依赖 %s[%s]，当前只有 %s\n    → 在 plugins.%s 下添加 %s 实例",
		inst.label(), pkgName(dep.Type), instName, strings.Join(alt, "、"), pkgName(dep.Type), instName)
}

// resolveRef resolves one Ref from Deps.Plugins: is a matching plugin type
// present, and -- if the Ref was narrowed with .Instance -- present under
// that exact instance name. Matching is by reflect.Type of the concrete
// plugin value, never by a hand-written string: a typo'd RefOf[*Foo]() fails
// to compile, whereas a typo'd plugin-name string would silently never match.
func resolveRef(g *graph.Graph, inst *instance, ref Ref, insts []*instance) string {
	var matches []*instance
	var sameType []*instance
	for _, other := range insts {
		if reflect.TypeOf(other.plugin) != ref.typ {
			continue
		}
		sameType = append(sameType, other)
		if ref.instance != "" && other.instance != normInstance(ref.instance) {
			continue
		}
		matches = append(matches, other)
	}
	if len(matches) > 0 {
		for _, m := range matches {
			g.AddEdge(m.id(), inst.id(), true)
		}
		return ""
	}

	name := refPluginName(ref)
	if ref.instance == "" || len(sameType) == 0 {
		return fmt.Sprintf("  插件 %s 依赖插件 %s，但 %s 未启用\n    → 在 application.yml 中添加 plugins.%s 配置节",
			inst.label(), name, name, name)
	}

	alt := make([]string, 0, len(sameType))
	for _, other := range sameType {
		alt = append(alt, fmt.Sprintf("%s[%s]", name, normInstance(other.instance)))
	}
	return fmt.Sprintf("  插件 %s 依赖插件 %s[%s]，当前只有 %s\n    → 在 plugins.%s 下添加 %s 实例",
		inst.label(), name, normInstance(ref.instance), strings.Join(alt, "、"), name, normInstance(ref.instance))
}

// matchInterface scans products already scoped to one instance name and
// returns every one whose concrete type is assignable to want. Scoping by
// instance mirrors registry.lookup's own semantics (§5.6) -- the static
// check at this stage and the runtime lookup at stage 5+ read the same way
// to a plugin author, because they run the same rule.
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
//
// Named distinctly from registry.go's closestMatch (Task 4): that one scans
// []registryKey scoped to a single instance for the runtime lookup path,
// this one scans []product across every instance for the static resolve-time
// diagnostic path -- same idea, different input shape, and Go does not allow
// two package-level functions to share a name regardless of signature.
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
// too (spec §5.6 registry semantics: "按方法名比对，签名不符也算缺").
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
// has no receiver at all. The off-by-one is why this helper exists instead
// of a plain reflect.Type equality check.
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
// pointer type. It backs two purely best-effort hints: "did you forget to
// import" on a fully-missing type, and the plugin-name-shaped prefix used in
// "当前只有 x[y]" when the type exists under a different instance. Neither
// hint is load-bearing; both are just where to look first.
//
// isMajorVersionSegment is not redeclared here: plugin.go's deriveName
// already defines a package-level helper with the exact same name and rule
// ("v2", "v10", ... trailing module path segments), and Go does not allow a
// second function of the same name in the same package. pkgName reuses that
// existing helper directly instead of shadowing it.
func pkgName(typ reflect.Type) string {
	t := typ
	for t.Kind() == reflect.Ptr {
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

// zeroPluginOf builds a zero-value Plugin of the given concrete type so we
// can name a plugin that was never registered at all -- t is always a
// pointer-to-struct in practice (RefOf[T Plugin]() requires T to satisfy
// Plugin, and every plugin in this framework is a pointer receiver), but the
// non-pointer branch is kept for defensiveness rather than assuming that
// convention is enforced elsewhere.
func zeroPluginOf(t reflect.Type) Plugin {
	if t.Kind() == reflect.Ptr {
		return reflect.New(t.Elem()).Interface().(Plugin)
	}
	return reflect.New(t).Elem().Interface().(Plugin)
}

// refPluginName recovers a Ref's plugin name purely from its type, with no
// instance required -- this is what makes the "但 x 未启用" message possible
// even when x was never registered at all. It deliberately mirrors
// deriveName's package-path rule rather than calling zeroPluginOf(t).Name():
// Name() can be overridden to anything (spec §5.4a, "想覆盖就自己写"), but the
// config section a user is told to add is always keyed by the package-
// derived name, not by whatever Name() happens to return.
func refPluginName(r Ref) string {
	name, err := deriveName(zeroPluginOf(r.typ))
	if err != nil {
		return pkgName(r.typ)
	}
	return name
}
