package plugin

import (
	"fmt"
	"strings"
)

// Factory constructs one fresh live value for one enabled instance. The
// runtime calls it exactly once per enabled instance and never for a disabled
// definition or instance. It must return a non-nil value.
type Factory func() Plugin

// Cardinality declares a Definition's static instance shape.
type Cardinality uint8

const (
	// SingleInstance is the zero-value policy: plugins.<key> configures one
	// default instance.
	SingleInstance Cardinality = iota
	// MultipleInstances expands map keys under plugins.<key> into named
	// instances.
	MultipleInstances
)

func (c Cardinality) String() string {
	switch c {
	case SingleInstance:
		return "single"
	case MultipleInstances:
		return "multiple"
	default:
		return fmt.Sprintf("cardinality(%d)", c)
	}
}

// Definition is the immutable, side-effect-free description placed in a
// catalog by a linked package.
type Definition struct {
	Key        Key
	Factory    Factory
	Instances  Cardinality
	Activation Activation
}

// Validate checks all invariants decidable without invoking Factory.
func (d Definition) Validate() error {
	if err := d.Key.Validate(); err != nil {
		return err
	}
	if d.Factory == nil {
		return fmt.Errorf("xbc: 插件 %q 的 Definition.Factory 不能为空", d.Key)
	}
	if d.Instances != SingleInstance && d.Instances != MultipleInstances {
		return fmt.Errorf("xbc: 插件 %q 的 Definition.Instances 无效：%d", d.Key, d.Instances)
	}
	if err := d.Activation.validate(); err != nil {
		return fmt.Errorf("xbc: 插件 %q 的 Activation 无效：%w", d.Key, err)
	}
	return nil
}

// Activation is an immutable enablement policy. Its zero value and Always
// both mean enabled unless explicitly vetoed with enabled:false.
type Activation string

const (
	// Always is the default activation policy. Unlike a package variable it
	// cannot be reassigned by an importing package.
	Always           Activation = "always"
	configuredPrefix Activation = "configured:"
)

// Configured enables a definition only when path exists.
func Configured(path string) Activation { return configuredPrefix + Activation(path) }

func (a Activation) validate() error {
	switch {
	case a == "", a == Always:
		return nil
	case strings.HasPrefix(string(a), string(configuredPrefix)):
		if strings.TrimPrefix(string(a), string(configuredPrefix)) == "" {
			return fmt.Errorf("Configured 的配置路径不能为空")
		}
		return nil
	default:
		return fmt.Errorf("未知策略 %q", a)
	}
}

// RequiresConfigSection exposes the configured-section gate to the runtime.
func (a Activation) RequiresConfigSection() (path string, required bool) {
	if strings.HasPrefix(string(a), string(configuredPrefix)) {
		return strings.TrimPrefix(string(a), string(configuredPrefix)), true
	}
	return "", false
}

func (a Activation) String() string {
	if a == "" || a == Always {
		return "always"
	}
	if path, ok := a.RequiresConfigSection(); ok {
		return fmt.Sprintf("configured(%s)", path)
	}
	return string(a)
}
