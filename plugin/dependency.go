package plugin

import (
	"fmt"

	pluginmodel "github.com/xbcio/xbc/plugin/model"
)

// Entry retains the identity of the Plugin that exported Value.
type Entry[T any] struct {
	Identity Identity
	Value    T
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
func RefTo[T any](key Key) Ref[T] { return RefToInstance[T](key, DefaultInstance) }

// RefToInstance creates an exact typed reference to one normalized instance.
func RefToInstance[T any](key Key, instance string) Ref[T] {
	return Ref[T]{token: newInputToken[T](pluginmodel.QueryRef, key, instance)}
}

// RequireOne creates a query requiring exactly one exporter of T.
func RequireOne[T any]() One[T] {
	return One[T]{token: newInputToken[T](pluginmodel.QueryOne, "", DefaultInstance)}
}

// OptionalOne creates a query accepting zero or one exporter of T.
func OptionalOne[T any]() Optional[T] {
	return Optional[T]{token: newInputToken[T](pluginmodel.QueryOptional, "", DefaultInstance)}
}

// Collect creates a query for every exporter of T.
func Collect[T any]() Many[T] {
	return Many[T]{token: newInputToken[T](pluginmodel.QueryMany, "", DefaultInstance)}
}

func newInputToken[T any](kind pluginmodel.QueryKind, key Key, instance string) pluginmodel.InputToken {
	return pluginmodel.NewInputToken(kind, typeOf[T](), pluginmodel.Key(key), instance, callerOrigin(2))
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
		entries[i] = Entry[T]{Identity: fromInternalIdentity(entry.Identity), Value: value}
	}
	return entries
}
