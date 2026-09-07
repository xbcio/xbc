package plugin

import pluginmodel "github.com/xbcio/xbc/plugin/model"

// Key is the stable, configuration-facing identity of a Definition. It is
// never derived from a Go package path or concrete type.
type Key = pluginmodel.Key

// DefaultInstance is the canonical instance name used when no named instance
// was requested.
const DefaultInstance = pluginmodel.DefaultInstance

// NormalizeInstance folds the empty spelling onto DefaultInstance.
func NormalizeInstance(instance string) string {
	return pluginmodel.NormalizeInstance(instance)
}

// Identity names one enabled plugin instance.
type Identity struct {
	Plugin   Key
	Instance string
}

// Normalized returns an identity with a non-empty canonical instance name.
func (i Identity) Normalized() Identity {
	i.Instance = NormalizeInstance(i.Instance)
	return i
}

// String renders "gorm" for the default instance and "gorm[readonly]" for a
// named instance.
func (i Identity) String() string {
	i = i.Normalized()
	if i.Instance == DefaultInstance {
		return i.Plugin.String()
	}
	return i.Plugin.String() + "[" + i.Instance + "]"
}

// CompareIdentity provides the canonical (Key, normalized instance) order.
func CompareIdentity(left, right Identity) int {
	return pluginmodel.CompareIdentity(toInternalIdentity(left), toInternalIdentity(right))
}

// ValidateName validates a textual plugin key before conversion to Key.
func ValidateName(value string) error { return Key(value).Validate() }

// ValidateInstanceName validates a configured instance name.
func ValidateInstanceName(value string) error {
	return pluginmodel.ValidateInstanceName(value)
}

func toInternalIdentity(identity Identity) pluginmodel.Identity {
	return pluginmodel.Identity{Plugin: pluginmodel.Key(identity.Plugin), Instance: identity.Instance}
}

func fromInternalIdentity(identity pluginmodel.Identity) Identity {
	return Identity{Plugin: Key(identity.Plugin), Instance: identity.Instance}
}
