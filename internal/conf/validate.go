// internal/conf/validate.go
package conf

import (
	"fmt"
	"reflect"
	"strconv"
	"strings"

	"github.com/go-playground/validator/v10"
)

// ValidationError carries one line per violation, already rendered but not
// yet column-aligned -- alignment happens lazily in Error() so that Append
// (merging violations collected from several plugins) still produces a
// correctly aligned block.
type ValidationError struct {
	Lines []string // each entry is "<path>\t<message>"
}

func (e *ValidationError) Error() string {
	if len(e.Lines) == 0 {
		return "xbc: 配置错误"
	}

	paths := make([]string, len(e.Lines))
	msgs := make([]string, len(e.Lines))
	width := 0
	for i, line := range e.Lines {
		parts := strings.SplitN(line, "\t", 2)
		paths[i] = parts[0]
		if len(parts) > 1 {
			msgs[i] = parts[1]
		}
		if n := len([]rune(parts[0])); n > width {
			width = n
		}
	}

	var b strings.Builder
	b.WriteString("xbc: 配置错误")
	for i := range paths {
		pad := width - len([]rune(paths[i])) + 2
		b.WriteString("\n  ")
		b.WriteString(paths[i])
		b.WriteString(strings.Repeat(" ", pad))
		b.WriteString(msgs[i])
	}
	return b.String()
}

// Append merges other's violations into e. Alignment is recomputed on the
// next Error() call, over the full merged Lines.
func (e *ValidationError) Append(other *ValidationError) {
	if other == nil {
		return
	}
	e.Lines = append(e.Lines, other.Lines...)
}

// Validate runs go-playground/validator over out and renders every violation
// as a Chinese line prefixed with its full config path (path + the field's
// own yaml-tag path, dot-joined).
func Validate(out any, path string) error {
	v := validator.New()
	err := v.Struct(out)
	if err == nil {
		return nil
	}

	verrs, ok := err.(validator.ValidationErrors)
	if !ok {
		return fmt.Errorf("xbc: 配置校验失败：%w", err)
	}

	root := reflect.TypeOf(out)
	ve := &ValidationError{}
	for _, fe := range verrs {
		p := joinPath(path, yamlPath(root, fe.StructNamespace()))
		ve.Lines = append(ve.Lines, p+"\t"+renderViolation(fe))
	}
	return ve
}

func joinPath(path, sub string) string {
	switch {
	case path == "":
		return sub
	case sub == "":
		return path
	default:
		return path + "." + sub
	}
}

// yamlPath translates validator's Go-field StructNamespace (e.g.
// "GormConfig.Pool.MaxOpenConn") into the yaml-tag path (e.g.
// "pool.max_open_conn") by walking the same struct schema Leaves/walkLeaves
// uses -- both must agree on the field-name-to-tag-name mapping, or the
// rendered error path won't match what's actually in the config file.
func yamlPath(root reflect.Type, structNamespace string) string {
	for root.Kind() == reflect.Ptr {
		root = root.Elem()
	}

	segments := strings.Split(structNamespace, ".")
	if len(segments) <= 1 {
		return ""
	}
	segments = segments[1:] // drop the leading struct type name

	t := root
	out := make([]string, 0, len(segments))
	for _, seg := range segments {
		name := seg
		if idx := strings.IndexByte(seg, '['); idx >= 0 {
			name = seg[:idx] // strip a slice/map index suffix, e.g. "Items[0]"
		}
		f, ok := t.FieldByName(name)
		if !ok {
			return strings.ToLower(strings.Join(segments, "."))
		}
		out = append(out, yamlTagName(f))

		ft := f.Type
		for ft.Kind() == reflect.Ptr {
			ft = ft.Elem()
		}
		t = ft
	}
	return strings.Join(out, ".")
}

func renderViolation(fe validator.FieldError) string {
	switch fe.Tag() {
	case "required":
		return "必填项缺失"
	case "min":
		return fmt.Sprintf("不能小于 %s，得到 %s", fe.Param(), quoteValue(fe.Value()))
	case "max":
		return fmt.Sprintf("不能大于 %s，得到 %s", fe.Param(), quoteValue(fe.Value()))
	case "hostname_port":
		return fmt.Sprintf("不是合法的 host:port —— 得到 %s", quoteValue(fe.Value()))
	case "oneof":
		return fmt.Sprintf("必须是 %s 之一，得到 %s", fe.Param(), quoteValue(fe.Value()))
	default:
		return fmt.Sprintf("未通过校验规则 %q，得到 %s", fe.Tag(), quoteValue(fe.Value()))
	}
}

func quoteValue(v any) string {
	return strconv.Quote(fmt.Sprint(v))
}
