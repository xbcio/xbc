package plugin

import (
	"context"
	"fmt"
	"reflect"
	"runtime"

	"github.com/xbcio/xbc/internal/pluginmodel"
)

// Definition is an opaque, immutable declaration handle. Packages normally
// evaluate one package-level handle and return it from Definition().
type Definition pluginmodel.Definition

// Cardinality declares a Definition's static instance shape.
type Cardinality = pluginmodel.Cardinality

const (
	SingleInstance    = pluginmodel.SingleInstance
	MultipleInstances = pluginmodel.MultipleInstances
)

// Activation is an immutable enablement policy. Its zero value means always
// enabled unless the instance's configuration explicitly sets enabled:false.
type Activation struct {
	kind pluginmodel.ActivationKind
	path string
}

// WhenConfigured enables a Definition only when path exists in the merged
// environment.
func WhenConfigured(path string) Activation {
	return Activation{kind: pluginmodel.ActivationConfigured, path: path}
}

// ConfigSpec describes fresh defaults and optional pure semantic preparation.
type ConfigSpec[C any] struct {
	Defaults func() C
	Prepare  func(C) (C, error)
}

// Options contains the static metadata shared by all instances of a
// Definition. Its generic parameter ties exports and lifecycle adapters to the
// factory's actual primary result type.
type Options[P any] struct {
	Instances  Cardinality
	Activation Activation
	ConfigPath string
	Inputs     InputSet
	Exports    ContractSet[P]
	Lifecycle  Lifecycle[P]
}

// Lifecycle supplies explicit adapters for a primary value that cannot
// implement XBC's lifecycle interfaces itself. A stage must not be supplied
// both here and on P; catalog freeze rejects that ambiguity.
type Lifecycle[P any] struct {
	Init        func(P, *Context) error
	Migrate     func(P, *Context) error
	Start       func(P, *Context) error
	OpenTraffic func(P, *Context) error
	Stop        func(P, context.Context) error
}

// Contract is one additional interface export tied to primary type P.
type Contract[P any] struct {
	contract pluginmodel.Contract
}

// ContractSet is an immutable set of additional exports.
type ContractSet[P any] struct {
	contracts []pluginmodel.Contract
}

// ExportAs declares interface I as an additional contract of primary type P.
// The witness function makes the Go compiler prove P is assignable to I.
func ExportAs[I any, P any](assign func(P) I) Contract[P] {
	if assign == nil {
		panic("xbc: plugin.ExportAs witness cannot be nil")
	}
	return Contract[P]{contract: pluginmodel.Contract{
		Type:   typeOf[I](),
		Origin: callerOrigin(1),
	}}
}

// Contracts collects additional interface contracts for one primary type.
func Contracts[P any](contracts ...Contract[P]) ContractSet[P] {
	out := make([]pluginmodel.Contract, len(contracts))
	for i, contract := range contracts {
		out[i] = contract.contract
	}
	return ContractSet[P]{contracts: out}
}

// Plan is the pure result of configuration-dependent planning for one instance.
type Plan[P any] struct {
	inputs  InputSet
	factory func(BuildContext) (P, error)
}

// PlanOf creates a final instance plan from its complete input set and factory.
func PlanOf[P any](inputs InputSet, factory func(BuildContext) (P, error)) Plan[P] {
	return Plan[P]{inputs: inputs, factory: factory}
}

// Define declares an unconfigured Plugin with statically known inputs.
func Define[P any](key Key, factory func(BuildContext) (P, error), optional ...Options[P]) Definition {
	options := oneOptions(optional)
	return newDefinitionNoConfig(key, func(any) (Plan[P], error) {
		return PlanOf(options.Inputs, factory), nil
	}, options, callerOrigin(1))
}

// DefineConfigured declares a configured Plugin with statically known inputs.
func DefineConfigured[C any, P any](key Key, spec ConfigSpec[C], factory func(BuildContext, C) (P, error), optional ...Options[P]) Definition {
	options := oneOptions(optional)
	return newDefinition(key, &spec, func(raw any) (Plan[P], error) {
		config, ok := raw.(C)
		if !ok {
			return Plan[P]{}, fmt.Errorf("xbc: internal invariant: prepared config for plugin %s has type %T, want %s", key, raw, typeOf[C]())
		}
		return PlanOf(options.Inputs, func(context BuildContext) (P, error) {
			return factory(context, config)
		}), nil
	}, options, callerOrigin(1))
}

// DefinePlanned declares a configured Plugin whose prepared configuration
// selects its inputs. The planner runs once per enabled instance before graph
// wiring and must not acquire resources.
func DefinePlanned[C any, P any](key Key, spec ConfigSpec[C], planner func(C) (Plan[P], error), optional ...Options[P]) Definition {
	options := oneOptions(optional)
	return newDefinition(key, &spec, func(raw any) (Plan[P], error) {
		config, ok := raw.(C)
		if !ok {
			return Plan[P]{}, fmt.Errorf("xbc: internal invariant: prepared config for plugin %s has type %T, want %s", key, raw, typeOf[C]())
		}
		return planner(config)
	}, options, callerOrigin(1))
}

func oneOptions[P any](options []Options[P]) Options[P] {
	switch len(options) {
	case 0:
		return Options[P]{}
	case 1:
		return options[0]
	default:
		panic("xbc: plugin definition accepts at most one Options value")
	}
}

func newDefinitionNoConfig[P any](key Key, planner func(any) (Plan[P], error), options Options[P], origin string) Definition {
	return finishDefinition(key, nil, planner, options, origin)
}

func newDefinition[C any, P any](key Key, spec *ConfigSpec[C], planner func(any) (Plan[P], error), options Options[P], origin string) Definition {
	var configDescriptor *pluginmodel.ConfigDescriptor
	if spec != nil {
		configDescriptor = &pluginmodel.ConfigDescriptor{Type: typeOf[C]()}
		if spec.Defaults != nil {
			configDescriptor.Defaults = func() any { return spec.Defaults() }
		}
		if spec.Prepare != nil {
			configDescriptor.Prepare = func(raw any) (any, error) {
				config, ok := raw.(C)
				if !ok {
					return nil, fmt.Errorf("xbc: internal invariant: config has type %T, want %s", raw, typeOf[C]())
				}
				return spec.Prepare(config)
			}
		}
	}

	return finishDefinition(key, configDescriptor, planner, options, origin)
}

func finishDefinition[P any](key Key, configDescriptor *pluginmodel.ConfigDescriptor, planner func(any) (Plan[P], error), options Options[P], origin string) Definition {
	descriptor := pluginmodel.DefinitionDescriptor{
		Key:         pluginmodel.Key(key),
		Origin:      origin,
		Primary:     typeOf[P](),
		Cardinality: pluginmodel.Cardinality(options.Instances),
		Activation: pluginmodel.Activation{
			Kind: options.Activation.kind,
			Path: options.Activation.path,
		},
		ConfigPath: options.ConfigPath,
		Contracts:  append([]pluginmodel.Contract(nil), options.Exports.contracts...),
		Config:     configDescriptor,
		Lifecycle:  eraseLifecycle(options.Lifecycle),
	}
	descriptor.Plan = func(raw any) (pluginmodel.InstancePlan, error) {
		plan, err := planner(raw)
		if err != nil {
			return pluginmodel.InstancePlan{}, err
		}
		if plan.factory == nil {
			return pluginmodel.InstancePlan{}, fmt.Errorf("xbc: plugin %s planner returned a nil factory", key)
		}
		return pluginmodel.InstancePlan{
			Inputs: plan.inputs.tokensCopy(),
			Factory: func(context pluginmodel.BuildContext) (any, error) {
				return plan.factory(BuildContext(context))
			},
		}, nil
	}
	return Definition(pluginmodel.NewDefinition(descriptor))
}

func eraseLifecycle[P any](lifecycle Lifecycle[P]) pluginmodel.LifecycleAdapters {
	var erased pluginmodel.LifecycleAdapters
	if lifecycle.Init != nil {
		erased.Init = func(value, context any) error { return lifecycle.Init(value.(P), context.(*Context)) }
	}
	if lifecycle.Migrate != nil {
		erased.Migrate = func(value, context any) error { return lifecycle.Migrate(value.(P), context.(*Context)) }
	}
	if lifecycle.Start != nil {
		erased.Start = func(value, context any) error { return lifecycle.Start(value.(P), context.(*Context)) }
	}
	if lifecycle.OpenTraffic != nil {
		erased.OpenTraffic = func(value, context any) error {
			return lifecycle.OpenTraffic(value.(P), context.(*Context))
		}
	}
	if lifecycle.Stop != nil {
		erased.Stop = func(value any, context context.Context) error { return lifecycle.Stop(value.(P), context) }
	}
	return erased
}

func typeOf[T any]() reflect.Type { return reflect.TypeOf((*T)(nil)).Elem() }

func callerOrigin(extraSkip int) string {
	_, file, line, ok := runtime.Caller(1 + extraSkip)
	if !ok {
		return "unknown"
	}
	return fmt.Sprintf("%s:%d", file, line)
}
