package autoload

import (
	"reflect"
	"testing"

	raftplugin "github.com/xbcio/xbc/integrations/raft"
	internalautoload "github.com/xbcio/xbc/internal/autoload"
	"github.com/xbcio/xbc/plugin"
)

func TestImportDeclaresRaftBundle(t *testing.T) {
	got := internalautoload.Freeze()
	if reflect.DeepEqual(got, plugin.Bundle{}) {
		t.Fatal("autoload import did not declare the Raft Bundle")
	}
	if !reflect.DeepEqual(got, raftplugin.Bundle()) {
		t.Fatal("autoload declared composition other than raft.Bundle()")
	}
}
