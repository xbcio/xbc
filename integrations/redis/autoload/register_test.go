package autoload

import (
	"reflect"
	"testing"

	"github.com/xbcio/xbc/integrations/redis"
	"github.com/xbcio/xbc/plugin"
	pluginautoload "github.com/xbcio/xbc/plugin/autoload"
)

func TestImportDeclaresRedisBundle(t *testing.T) {
	got := pluginautoload.Freeze()
	if reflect.DeepEqual(got, plugin.Bundle{}) {
		t.Fatal("autoload import did not declare the Redis Bundle")
	}
	if !reflect.DeepEqual(got, redis.Bundle()) {
		t.Fatal("autoload declared composition other than redis.Bundle()")
	}
}
