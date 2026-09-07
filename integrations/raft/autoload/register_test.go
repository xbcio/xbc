package autoload

import (
	"reflect"
	"testing"

	raftplugin "github.com/xbcio/xbc/integrations/raft"
	"github.com/xbcio/xbc/plugin"
	pluginautoload "github.com/xbcio/xbc/plugin/autoload"
)

func TestImportDeclaresRaftBundle(t *testing.T) {
	got := pluginautoload.Freeze()
	if reflect.DeepEqual(got, plugin.Bundle{}) {
		t.Fatal("autoload import did not declare the Raft Bundle")
	}
	if !reflect.DeepEqual(got, raftplugin.Bundle()) {
		t.Fatal("autoload declared composition other than raft.Bundle()")
	}
}
