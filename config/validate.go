package config

import (
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"unicode/utf8"

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
		return "xbc: configuration error"
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
	b.WriteString("xbc: configuration error")
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
		return fmt.Errorf("xbc: configuration validation failed: %w", err)
	}

	ve := &ValidationError{}
	for _, fieldError := range verrs {
		yamlPath := schema.yamlPath(fieldError.StructNamespace())
		fieldPath := joinPath(path, yamlPath)
		ve.Lines = append(ve.Lines, fieldPath+"\t"+renderViolation(fieldError, schema.isMasked(fieldError.StructNamespace())))
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
				"xbc: configuration schema %s in section %s has an invalid validate tag: %v",
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

// isMasked reports whether a violation's field, or any struct it is nested
// inside, was tagged mask:"true". Inheritance is what makes the marker safe to
// put on a credentials block: a field added under it later is protected
// without anyone having to remember to tag it too.
func (s *configSchema) isMasked(structNamespace string) bool {
	segments := strings.Split(structNamespace, ".")
	if len(segments) <= 1 {
		return false
	}
	segments = segments[1:]
	for i, segment := range segments {
		segments[i], _ = splitNamespaceSegment(segment)
	}
	for n := 1; n <= len(segments); n++ {
		if s.MaskedGoPaths[strings.Join(segments[:n], ".")] {
			return true
		}
	}
	return false
}

func renderViolation(fe validator.FieldError, masked bool) string {
	got := quoteValue(fe.Value())
	if masked {
		got = redactValue(fe.Value())
	}
	switch fe.Tag() {
	case "required":
		return "required field missing"
	case "min":
		return fmt.Sprintf("cannot be less than %s, got %s", fe.Param(), got)
	case "max":
		return fmt.Sprintf("cannot be greater than %s, got %s", fe.Param(), got)
	case "hostname_port":
		return fmt.Sprintf("not a valid host:port — got %s", got)
	case "oneof":
		return fmt.Sprintf("must be one of %s, got %s", fe.Param(), got)
	default:
		return fmt.Sprintf("failed validation rule %q, got %s", fe.Tag(), got)
	}
}

func quoteValue(v any) string {
	return strconv.Quote(fmt.Sprint(v))
}

// redactValue describes a secret without reproducing it. A string reports its
// length because that is exactly what a min or max violation is about, and the
// rule in the same message already discloses the threshold; anything else
// collapses to a bare marker, since a length says nothing useful about a
// number and its digits would be the secret itself.
func redactValue(v any) string {
	if rv := reflect.ValueOf(v); rv.Kind() == reflect.String {
		return fmt.Sprintf("a %d-character value", utf8.RuneCountInString(rv.String()))
	}
	return "[redacted]"
}
