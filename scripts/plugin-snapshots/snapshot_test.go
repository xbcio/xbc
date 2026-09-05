package main

import (
	"encoding/json"
	"testing"
)

func TestSnapshotsAreDeterministic(t *testing.T) {
	descriptorsA, err := collectDescriptors()
	if err != nil {
		t.Fatalf("collectDescriptors: %v", err)
	}
	descriptorsB, err := collectDescriptors()
	if err != nil {
		t.Fatalf("collectDescriptors: %v", err)
	}

	identityA, err := marshalSnapshot(buildIdentitySnapshot(descriptorsA))
	if err != nil {
		t.Fatalf("marshal identity snapshot: %v", err)
	}
	identityB, err := marshalSnapshot(buildIdentitySnapshot(descriptorsB))
	if err != nil {
		t.Fatalf("marshal identity snapshot: %v", err)
	}
	if string(identityA) != string(identityB) {
		t.Fatalf("identity snapshot is not deterministic:\nfirst:\n%s\nsecond:\n%s", identityA, identityB)
	}

	defaultA, err := marshalSnapshot(buildDefaultSnapshot(descriptorsA))
	if err != nil {
		t.Fatalf("marshal default snapshot: %v", err)
	}
	defaultB, err := marshalSnapshot(buildDefaultSnapshot(descriptorsB))
	if err != nil {
		t.Fatalf("marshal default snapshot: %v", err)
	}
	if string(defaultA) != string(defaultB) {
		t.Fatalf("default snapshot is not deterministic:\nfirst:\n%s\nsecond:\n%s", defaultA, defaultB)
	}
}

func TestSessionDescriptorRoundTrips(t *testing.T) {
	descriptors, err := collectDescriptors()
	if err != nil {
		t.Fatalf("collectDescriptors: %v", err)
	}
	identity := buildIdentitySnapshot(descriptors)

	var session *identityPlugin
	for i := range identity.Plugins {
		if identity.Plugins[i].Key == "session" {
			session = &identity.Plugins[i]
		}
	}
	if session == nil {
		t.Fatal("identity snapshot has no \"session\" plugin")
	}
	if session.ConfigPath != "plugins.session" {
		t.Errorf("session.ConfigPath = %q, want %q", session.ConfigPath, "plugins.session")
	}
	if session.Cardinality != "single" {
		t.Errorf("session.Cardinality = %q, want %q", session.Cardinality, "single")
	}

	wantContracts := map[string]bool{
		"github.com/xbcio/xbc/transport/web.Middleware":                   false,
		"github.com/xbcio/xbc/transport/web/integrations/session.Manager": false,
	}
	if len(session.Contracts) != len(wantContracts) {
		t.Fatalf("session.Contracts = %v, want exactly %v", session.Contracts, wantContracts)
	}
	for _, contract := range session.Contracts {
		if _, ok := wantContracts[contract]; !ok {
			t.Errorf("session.Contracts has unexpected entry %q", contract)
			continue
		}
		wantContracts[contract] = true
	}
	for contract, found := range wantContracts {
		if !found {
			t.Errorf("session.Contracts is missing %q", contract)
		}
	}
}

func TestPluginsWithoutConfigSpecAreAbsentFromDefaultSnapshot(t *testing.T) {
	descriptors, err := collectDescriptors()
	if err != nil {
		t.Fatalf("collectDescriptors: %v", err)
	}

	foundInIdentity := false
	for _, descriptor := range descriptors {
		if descriptor.Key.String() == "gracefulshutdown" {
			foundInIdentity = true
			if descriptor.Config != nil {
				t.Fatalf("gracefulshutdown unexpectedly has a ConfigSpec; the no-ConfigSpec fixture assumption is stale, pick another plugin")
			}
		}
	}
	if !foundInIdentity {
		t.Fatal("gracefulshutdown is missing from collected descriptors")
	}

	defaults := buildDefaultSnapshot(descriptors)
	for _, entry := range defaults.Plugins {
		if entry.Key == "gracefulshutdown" {
			t.Fatalf("gracefulshutdown must be absent from the default snapshot, found entry: %+v", entry)
		}
	}

	// Marshaling must not panic even though gracefulshutdown's Config is nil.
	if _, err := marshalSnapshot(defaults); err != nil {
		t.Fatalf("marshal default snapshot: %v", err)
	}
}

func TestDefaultSnapshotPluginCountIsFewerThanIdentity(t *testing.T) {
	descriptors, err := collectDescriptors()
	if err != nil {
		t.Fatalf("collectDescriptors: %v", err)
	}
	identity := buildIdentitySnapshot(descriptors)
	defaults := buildDefaultSnapshot(descriptors)
	if len(defaults.Plugins) >= len(identity.Plugins) {
		t.Fatalf("expected fewer default-snapshot plugins (%d) than identity-snapshot plugins (%d)", len(defaults.Plugins), len(identity.Plugins))
	}
}

func TestMarshalSnapshotIndentAndTrailingNewline(t *testing.T) {
	contents, err := marshalSnapshot(identitySnapshot{Plugins: []identityPlugin{{Key: "x", ConfigPath: "plugins.x", Cardinality: "single", Contracts: []string{}}}})
	if err != nil {
		t.Fatalf("marshalSnapshot: %v", err)
	}
	if contents[len(contents)-1] != '\n' {
		t.Fatalf("marshalSnapshot output does not end with a newline: %q", contents)
	}
	var decoded identitySnapshot
	if err := json.Unmarshal(contents, &decoded); err != nil {
		t.Fatalf("marshalSnapshot output does not round-trip through json.Unmarshal: %v", err)
	}
}
