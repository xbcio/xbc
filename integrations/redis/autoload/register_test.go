package autoload

import (
	"reflect"
	"testing"

	"github.com/xbcio/xbc/integrations/redis"
	internalautoload "github.com/xbcio/xbc/internal/autoload"
	"github.com/xbcio/xbc/plugin"
)

func TestImportDeclaresRedisBundle(t *testing.T) {
	got := internalautoload.Freeze()
	if reflect.DeepEqual(got, plugin.Bundle{}) {
		t.Fatal("autoload import did not declare the Redis Bundle")
	}
	if !reflect.DeepEqual(got, redis.Bundle()) {
		t.Fatal("autoload declared composition other than redis.Bundle()")
	}
}
