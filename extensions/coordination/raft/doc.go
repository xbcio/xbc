// Package raft integrates HashiCorp Raft with XBC.
//
// # Usage
//
// Definition constructs a *KVFSM-backed node by default. A business Plugin
// requires the published Node contract, encodes mutations for that state
// machine, and synchronizes reads with Barrier:
//
//	type ReplicatedStore struct {
//		node raft.Node
//	}
//
//	var nodeRef = plugin.RequireOne[raft.Node]()
//
//	var definition = plugin.Define(
//		"replicated-store",
//		func(ctx plugin.BuildContext) (*ReplicatedStore, error) {
//			return &ReplicatedStore{node: nodeRef.Get(ctx).Value}, nil
//		},
//		plugin.Options[*ReplicatedStore]{
//			Inputs: plugin.Inputs(nodeRef),
//		},
//	)
//
//	func (s *ReplicatedStore) Set(ctx context.Context, key string, value []byte) error {
//		command, err := raft.EncodeSet(key, value)
//		if err != nil {
//			return err
//		}
//		_, err = s.node.Apply(ctx, command)
//		return err
//	}
//
//	func (s *ReplicatedStore) Get(ctx context.Context, key string) ([]byte, bool, error) {
//		if err := s.node.Barrier(ctx); err != nil {
//			return nil, false, err
//		}
//		fsm, ok := s.node.FSM().(*raft.KVFSM)
//		if !ok {
//			return nil, false, errors.New("raft: unexpected FSM type")
//		}
//		value, found := fsm.Get(key)
//		return value, found, nil
//	}
//
// An application with a domain-specific state machine composes it the same
// way cron composes a custom Locker: export raft.FSM from a Plugin that is
// wired ahead of raft.Definition, and Definition's factory picks it up in
// place of KVFSM. FSM.Apply must be deterministic, and Snapshot and Restore
// must encode the complete recoverable state.
//
//	var definition = plugin.Define(
//		"domain-fsm",
//		func(plugin.BuildContext) (*domainFSM, error) { return newDomainFSM(), nil },
//		plugin.Options[*domainFSM]{
//			Exports: plugin.Contracts(
//				plugin.ExportAs[raft.FSM](func(fsm *domainFSM) raft.FSM { return fsm }),
//			),
//		},
//	)
//
// The plugin owns one real TCP transport, log/stable stores, snapshot store,
// and Raft node. File-backed storage is the production default. Bootstrap is
// deliberately disabled by default so merely enabling the plugin cannot create
// competing clusters; bootstrap a new cluster explicitly, and never bootstrap
// a node joining an existing cluster.
//
// The built-in TCP transport does not add authentication or TLS. Bind it only
// to a trusted network or place it behind authenticated encryption. Stop is
// concurrent-safe and shuts down Raft before closing its transport and
// stores. Importing this package has no autoload side effects; import the
// autoload subpackage only for the default composition.
package raft
