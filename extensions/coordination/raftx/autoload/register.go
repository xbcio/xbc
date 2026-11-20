// Package autoload adds the Raft integration Bundle to XBC's optional
// process-wide composition. Libraries should import the side-effect-free
// parent package and compose its Bundle explicitly.
package autoload

import (
	"github.com/xbcio/xbc/extensions/coordination/raftx"
	"github.com/xbcio/xbc/plugin/autoload"
)

func init() { autoload.Declare(raftx.Bundle()) }
