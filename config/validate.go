// config/validate.go
package config

import (
	"fmt"
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
// with the yaml path produced by the same schema walker Bind uses. out must be
// a non-nil pointer to a struct, matching Bind's output contract.
func Validate(out any, path string) error {
	_, schema, schemaErr := schemaFor(out)
	if schemaErr != nil {
		return schemaErr
	}

	v := validator.New()
	err := validateStruct(v, out, schema, path)
	if err == nil {
		return nil
	}

	verrs, ok := err.(validator.ValidationErrors)
	if !ok {
		return fmt.Errorf("xbc: 配置校验失败：%w", err)
	}

	ve := &ValidationError{}
	for _, fieldError := range verrs {
		yamlPath := schema.yamlPath(fieldError.StructNamespace())
		fieldPath := joinPath(path, yamlPath)
		ve.Lines = append(ve.Lines, fieldPath+"\t"+renderViolation(fieldError))
	}
	return ve
}

// validateStruct is the trust boundary around validation tags supplied by a
// plugin's Config schema. go-playground/validator panics for unknown rules and
// malformed rule parameters; those are schema errors and must not take down
// the process during assembly.
func validateStruct(v *validator.Validate, out any, schema *configSchema, path string) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf(
				"xbc: 配置 schema %s 在配置节 %s 的 validate tag 无效：%v",
				schema.Root,
				displayPath(path),
				recovered,
			)
		}
	}()
	return v.Struct(out)
}

func (s *configSchema) yamlPath(structNamespace string) string {
	segments := strings.Split(structNamespace, ".")
	if len(segments) <= 1 {
		return ""
	}
	segments = segments[1:]

	goSegments := make([]string, len(segments))
	suffixes := make([]string, len(segments))
	for i, segment := range segments {
		goSegments[i], suffixes[i] = splitNamespaceSegment(segment)
	}

	for n := len(goSegments); n > 0; n-- {
		path, ok := s.YAMLByGoPath[strings.Join(goSegments[:n], ".")]
		if !ok {
			continue
		}
		path = appendNamespaceSuffixes(s, goSegments[:n], suffixes[:n], path)
		for i := n; i < len(goSegments); i++ {
			path = joinPath(path, strings.ToLower(goSegments[i])+suffixes[i])
		}
		return path
	}
	for i := range goSegments {
		goSegments[i] = strings.ToLower(goSegments[i]) + suffixes[i]
	}
	return strings.Join(goSegments, ".")
}

func appendNamespaceSuffixes(s *configSchema, goSegments, suffixes []string, path string) string {
	for i, suffix := range suffixes {
		if suffix == "" {
			continue
		}
		parentPath := ""
		if i > 0 {
			parentPath = s.YAMLByGoPath[strings.Join(goSegments[:i], ".")]
		}
		segmentPath := s.YAMLByGoPath[strings.Join(goSegments[:i+1], ".")]
		segmentName := strings.TrimPrefix(segmentPath, parentPath)
		segmentName = strings.TrimPrefix(segmentName, ".")
		needle := joinPath(parentPath, segmentName)
		path = strings.Replace(path, needle, needle+suffix, 1)
	}
	return path
}

func splitNamespaceSegment(segment string) (name, suffix string) {
	if index := strings.IndexByte(segment, '['); index >= 0 {
		return segment[:index], segment[index:]
	}
	return segment, ""
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
