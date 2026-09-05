package plugin

import (
	"bytes"
	"fmt"
	"io"
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xbcio/xbc/internal/pluginmodel"
)

func TestTypeOfRetainsInterfaceType(t *testing.T) {
	var reader io.Reader = bytes.NewBufferString("x")
	assert.NotEqual(t, reflect.Interface, reflect.TypeOf(reader).Kind())
	assert.Equal(t, reflect.Interface, typeOf[io.Reader]().Kind())
	assert.Equal(t, "fmt.Stringer", typeOf[fmt.Stringer]().String())
}

func TestTypedInputConstructorsCreateFinalDistinctQueries(t *testing.T) {
	ref := RefTo[io.Reader]("source")
	named := RefToInstance[io.Reader]("source", "readonly")
	one := RequireOne[io.Reader]()
	optional := OptionalOne[io.Reader]()
	many := Collect[io.Reader]()

	queries := []pluginmodel.InputToken{
		ref.inputToken(), named.inputToken(), one.inputToken(), optional.inputToken(), many.inputToken(),
	}
	ids := map[uint64]bool{}
	for _, query := range queries {
		require.NotZero(t, query.ID)
		assert.False(t, ids[query.ID], "each constructor creates a distinct immutable token")
		ids[query.ID] = true
		assert.Equal(t, typeOf[io.Reader](), query.Type)
	}
	assert.Equal(t, pluginmodel.QueryRef, queries[0].Kind)
	assert.Equal(t, DefaultInstance, queries[0].Instance)
	assert.Equal(t, "readonly", queries[1].Instance)
	assert.Equal(t, pluginmodel.QueryOne, queries[2].Kind)
	assert.Equal(t, pluginmodel.QueryOptional, queries[3].Kind)
	assert.Equal(t, pluginmodel.QueryMany, queries[4].Kind)
}

func TestInputsDefensivelyCopiesTokens(t *testing.T) {
	input := Collect[io.Reader]()
	set := Inputs(input)
	first := set.tokensCopy()
	first[0].Key = "changed"
	second := set.tokensCopy()
	assert.Empty(t, second[0].Key)
}

func TestIdentityNormalizationAndCanonicalOrder(t *testing.T) {
	assert.Equal(t, DefaultInstance, NormalizeInstance(""))
	assert.Equal(t, "source", (Identity{Plugin: "source"}).String())
	assert.Equal(t, "source[readonly]", (Identity{Plugin: "source", Instance: "readonly"}).String())
	assert.Less(t, CompareIdentity(Identity{Plugin: "a", Instance: "z"}, Identity{Plugin: "b", Instance: "a"}), 0)
	assert.Less(t, CompareIdentity(Identity{Plugin: "a"}, Identity{Plugin: "a", Instance: "readonly"}), 0)
}
