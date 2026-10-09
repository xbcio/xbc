package plugin

import (
	"fmt"
	"strings"

	pluginmodel "github.com/xbcio/xbc/plugin/model"
)

// Entry retains the identity of the Plugin that exported Value, together with
// the workload that Plugin belongs to.
type Entry[T any] struct {
	Identity Identity
	Value    T
	// Workload is the workload the producer belongs to, or "" when it belongs
	// to none. It describes the occurrence that actually produced Value rather
	// than what the producer's own declaration says in isolation, so an
	// application can account for a value against the workload that owns it.
	//
	// It is the producer's membership, not the submitter's, and both matter to
	// a budget. Work a Plugin starts for itself -- Context.Go, or a unit taken
	// from Context.Admission -- is charged to its own workload, whatever a
	// value it read belonged to. Work it runs on a workload's behalf is charged
	// to that workload's quota instead, through Context.AdmissionFor, whose key
	// comes from exactly this field: that is how a shared integration serving
	// several workloads keeps each one's work inside the budget it declared.
	Workload WorkloadKey
}

// Input is the sealed common type accepted by Inputs. Tokens remain strongly
// typed at their Get call sites.
type Input interface {
	inputToken() pluginmodel.InputToken
}

// InputSet is an immutable declaration set used by a Definition or Plan.
type InputSet struct {
	tokens []pluginmodel.InputToken
}

// Inputs collects the exact tokens a factory is allowed to read.
func Inputs(inputs ...Input) InputSet {
	tokens := make([]pluginmodel.InputToken, len(inputs))
	for i, input := range inputs {
		if input == nil {
			panic(fmt.Sprintf("xbc: plugin.Inputs item %d is nil", i))
		}
		tokens[i] = input.inputToken()
	}
	return InputSet{tokens: tokens}
}

func (inputs InputSet) tokensCopy() []pluginmodel.InputToken {
	return append([]pluginmodel.InputToken(nil), inputs.tokens...)
}

// Ref selects one exact Definition key and normalized instance exporting T.
type Ref[T any] struct{ token pluginmodel.InputToken }

// One selects exactly one Plugin exporting T.
type One[T any] struct{ token pluginmodel.InputToken }

// Optional selects zero or one Plugin exporting T.
type Optional[T any] struct{ token pluginmodel.InputToken }

// Many selects every Plugin exporting T.
type Many[T any] struct{ token pluginmodel.InputToken }

// RefTo creates an exact typed reference to key's default instance.
func RefTo[T any](key Key) Ref[T] { return refTo[T](key, DefaultInstance) }

// RefToInstance creates an exact typed reference to one normalized instance.
// The instance must be a name the configuration side would accept, so a typo is
// rejected here rather than surfacing later as a missing exact producer. The
// empty string stays valid and normalizes to the default instance.
func RefToInstance[T any](key Key, instance string) Ref[T] { return refTo[T](key, instance) }

// refTo is shared by RefTo and RefToInstance so both record the declaration
// that called them: the two wrappers sit at the same depth above refTo, so one
// depth reaches the caller for either.
func refTo[T any](key Key, instance string) Ref[T] {
	if instance != "" {
		if err := pluginmodel.ValidateInstanceName(instance); err != nil {
			panic("xbc: plugin.RefToInstance " + strings.TrimPrefix(err.Error(), "xbc: "))
		}
	}
	return Ref[T]{token: newInputToken[T](pluginmodel.QueryRef, key, instance, 3)}
}

// RequireOne creates a query requiring exactly one exporter of T.
func RequireOne[T any]() One[T] {
	return One[T]{token: newInputToken[T](pluginmodel.QueryOne, "", DefaultInstance, 2)}
}

// OptionalOne creates a query accepting zero or one exporter of T.
func OptionalOne[T any]() Optional[T] {
	return Optional[T]{token: newInputToken[T](pluginmodel.QueryOptional, "", DefaultInstance, 2)}
}

// Collect creates a query for every exporter of T.
func Collect[T any]() Many[T] {
	return Many[T]{token: newInputToken[T](pluginmodel.QueryMany, "", DefaultInstance, 2)}
}

// newInputToken takes the callerOrigin depth explicitly: the query constructors
// call it directly, so 2 reaches their caller, while RefTo and RefToInstance
// reach it one frame deeper through refTo and pass 3.
func newInputToken[T any](kind pluginmodel.QueryKind, key Key, instance string, callerSkip int) pluginmodel.InputToken {
	return pluginmodel.NewInputToken(kind, typeOf[T](), pluginmodel.Key(key), instance, callerOrigin(callerSkip))
}

func (input Ref[T]) inputToken() pluginmodel.InputToken      { return input.token }
func (input One[T]) inputToken() pluginmodel.InputToken      { return input.token }
func (input Optional[T]) inputToken() pluginmodel.InputToken { return input.token }
func (input Many[T]) inputToken() pluginmodel.InputToken     { return input.token }

// Get reads the exact pre-bound producer. It performs no live lookup.
func (input Ref[T]) Get(context BuildContext) Entry[T] {
	return oneEntry[T](context, input.token, "Ref")
}

// Get reads the unique pre-bound producer. It performs no live lookup.
func (input One[T]) Get(context BuildContext) Entry[T] {
	return oneEntry[T](context, input.token, "One")
}

// Get reads the optional pre-bound producer.
func (input Optional[T]) Get(context BuildContext) (Entry[T], bool) {
	entries := readEntries[T](context, input.token)
	switch len(entries) {
	case 0:
		return Entry[T]{}, false
	case 1:
		return entries[0], true
	default:
		panic(fmt.Sprintf("xbc: internal invariant: Optional token %d has %d bindings", input.token.ID, len(entries)))
	}
}

// Get returns a fresh deterministic slice of all pre-bound producers.
func (input Many[T]) Get(context BuildContext) []Entry[T] {
	return readEntries[T](context, input.token)
}

func oneEntry[T any](context BuildContext, token pluginmodel.InputToken, kind string) Entry[T] {
	entries := readEntries[T](context, token)
	if len(entries) != 1 {
		panic(fmt.Sprintf("xbc: internal invariant: %s token %d has %d bindings", kind, token.ID, len(entries)))
	}
	return entries[0]
}

func readEntries[T any](context BuildContext, token pluginmodel.InputToken) []Entry[T] {
	erased := pluginmodel.ReadBuildSlot(pluginmodel.BuildContext(context), token)
	entries := make([]Entry[T], len(erased))
	for i, entry := range erased {
		value, ok := entry.Value.(T)
		if !ok {
			panic(fmt.Sprintf("xbc: internal invariant: input token %d bound %T, want %s", token.ID, entry.Value, typeOf[T]()))
		}
		entries[i] = Entry[T]{
			Identity: fromInternalIdentity(entry.Identity),
			Value:    value,
			Workload: entry.Workload,
		}
	}
	return entries
}
