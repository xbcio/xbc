package objectstorage

import (
	"path"
	"path/filepath"
	"strings"
)

const temporaryComponentPrefix = ".xbc-objectstorage-"

func validateKey(key string) error {
	if err := validatePathText(key, false); err != nil {
		return err
	}
	if strings.HasSuffix(key, "/") {
		return invalidKey(key, "object keys must not end with a slash")
	}
	return nil
}

func validatePrefix(prefix string) error {
	if prefix == "" {
		return nil
	}
	return validatePathText(strings.TrimSuffix(prefix, "/"), false)
}

func validatePathText(value string, allowEmpty bool) error {
	if value == "" {
		if allowEmpty {
			return nil
		}
		return invalidKey(value, "empty path")
	}
	if strings.ContainsRune(value, 0) {
		return invalidKey(value, "NUL bytes are forbidden")
	}
	if strings.Contains(value, `\`) {
		return invalidKey(value, "backslashes are forbidden")
	}
	if path.IsAbs(value) || filepath.IsAbs(value) || windowsAbsolute(value) {
		return invalidKey(value, "absolute paths are forbidden")
	}
	for _, component := range strings.Split(value, "/") {
		switch {
		case component == "":
			return invalidKey(value, "empty path components are forbidden")
		case component == "." || component == "..":
			return invalidKey(value, "dot path components are forbidden")
		case strings.HasPrefix(component, temporaryComponentPrefix):
			return invalidKey(value, "reserved internal path component")
		}
	}
	return nil
}

func windowsAbsolute(value string) bool {
	return len(value) >= 2 && ((value[0] >= 'A' && value[0] <= 'Z') || (value[0] >= 'a' && value[0] <= 'z')) && value[1] == ':'
}
