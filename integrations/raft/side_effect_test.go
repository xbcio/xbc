package raft_test

import (
	"reflect"
	"testing"

	raftplugin "github.com/xbcio/xbc/integrations/raft"
	"github.com/xbcio/xbc/internal/autoload"
	"github.com/xbcio/xbc/plugin"
)

func TestOrdinaryImportDefinitionAndBundleHaveNoAutoloadSideEffect(t *testing.T) {
	if raftplugin.Definition() != raftplugin.Definition() {
		t.Fatal("Definition() returned different handles")
	}
	_ = raftplugin.Bundle()
	if got := autoload.Freeze(); !reflect.DeepEqual(got, plugin.Bundle{}) {
		t.Fatal("ordinary Raft import or Bundle() mutated the autoload composition")
	}
}
