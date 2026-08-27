package config

// Scope returns a read-only View rooted at prefix. Paths passed to the
// returned View are relative to that prefix. Scope never grants access above
// its prefix; an empty relative path refers to the scoped root itself.
//
// Mutable maps and slices are recursively defensive-copied at this boundary,
// even when the supplied View has a weaker implementation than Environment.
func Scope(view View, prefix string) View {
	if scoped, ok := view.(*scopedView); ok {
		return &scopedView{source: scoped.source, prefix: joinPath(scoped.prefix, prefix)}
	}
	return &scopedView{source: view, prefix: prefix}
}

type scopedView struct {
	source View
	prefix string
}

func (v *scopedView) Get(path string) any {
	if v == nil || v.source == nil {
		return nil
	}
	return cloneCollections(v.source.Get(v.path(path)))
}

func (v *scopedView) Exists(path string) bool {
	if v == nil || v.source == nil {
		return false
	}
	return v.source.Exists(v.path(path))
}

func (v *scopedView) Sub(path string) map[string]any {
	if v == nil || v.source == nil {
		return nil
	}
	value := cloneCollections(v.source.Sub(v.path(path)))
	if value == nil {
		return nil
	}
	return value.(map[string]any)
}

func (v *scopedView) path(path string) string {
	return joinPath(v.prefix, path)
}
