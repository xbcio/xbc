module github.com/xbcio/xbc/extensions/coordination/raft

go 1.25.0

// The XBC core module is intentionally absent until it has a real release tag.
// Repository development resolves it through go.work; this publishable module
// contains neither a local replace nor a synthetic XBC requirement.
require (
	github.com/hashicorp/raft v1.7.3
	github.com/hashicorp/raft-boltdb/v2 v2.3.1
)

require (
	github.com/armon/go-metrics v0.4.1 // indirect
	github.com/boltdb/bolt v1.3.1 // indirect
	github.com/fatih/color v1.13.0 // indirect
	github.com/hashicorp/go-hclog v1.6.2 // indirect
	github.com/hashicorp/go-immutable-radix v1.0.0 // indirect
	github.com/hashicorp/go-metrics v0.5.4 // indirect
	github.com/hashicorp/go-msgpack/v2 v2.1.2 // indirect
	github.com/hashicorp/go-uuid v1.0.2 // indirect
	github.com/hashicorp/golang-lru v0.5.0 // indirect
	github.com/mattn/go-colorable v0.1.12 // indirect
	github.com/mattn/go-isatty v0.0.24 // indirect
	github.com/stretchr/testify v1.12.1 // indirect
	go.etcd.io/bbolt v1.3.5 // indirect
	golang.org/x/sys v0.45.0 // indirect
)
