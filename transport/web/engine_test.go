package web_test

import (
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/xbcio/xbc/transport/web"
)

// TestEnginePortShapeIsFrozen pins the Engine port's width to the six methods
// the W1-W4 sequence fixed it at. The port is the seam every HTTP engine
// adapter implements and every registration path goes through, so its shape is
// an architectural decision rather than an implementation detail: W1 grew it
// from five methods to six by adding Mount, precisely because a subtree's
// syntax is the engine's own and could not be expressed through Handle.
//
// The two shipped adapters carry compile-time assertions, so a method they
// fail to implement breaks their build -- but the interface can still shrink
// silently: removing a method, or renaming one, leaves an adapter that still
// has the method compiling as a satisfier, while the port itself narrows.
// This guard holds the published surface to its declared size in both
// directions.
//
// If this test goes red, read it as "the engine port just changed shape," not
// as "update the list below."
func TestEnginePortShapeIsFrozen(t *testing.T) {
	portType := reflect.TypeOf((*web.Engine)(nil)).Elem()

	frozen := map[string]bool{
		"Handle": true, "Mount": true, "NoRoute": true,
		"NoMethod": true, "Serve": true, "Shutdown": true,
	}
	for i := range portType.NumMethod() {
		name := portType.Method(i).Name
		assert.True(t, frozen[name], "unexpected method %s on Engine", name)
	}
	assert.Equal(t, len(frozen), portType.NumMethod())
}
