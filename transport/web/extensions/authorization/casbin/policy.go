package casbin

import (
	"encoding/csv"
	"errors"
	"fmt"
	"os"
	"strings"

	casbinlib "github.com/casbin/casbin/v2"
	"github.com/casbin/casbin/v2/model"
	"github.com/casbin/casbin/v2/persist"
)

const permissionModel = `[request_definition]
r = sub, perm

[policy_definition]
p = sub, perm

[role_definition]
g = _, _

[policy_effect]
e = some(where (p.eft == allow))

[matchers]
m = (r.sub == p.sub || g(r.sub, p.sub)) && r.perm == p.perm
`

const pathMethodModel = `[request_definition]
r = sub, obj, act

[policy_definition]
p = sub, obj, act

[role_definition]
g = _, _

[policy_effect]
e = some(where (p.eft == allow))

[matchers]
m = (r.sub == p.sub || g(r.sub, p.sub)) && keyMatch2(r.obj, p.obj) && regexMatch(r.act, p.act)
`

var errReadOnlyPolicySource = errors.New("casbin: configured policy source is read-only; mutate in memory with autosave disabled or update the source and call LoadPolicy")

func buildSourceEnforcer(cfg normalizedConfig) (*casbinlib.SyncedEnforcer, error) {
	m, err := loadAndValidateModel(cfg)
	if err != nil {
		return nil, err
	}

	enforcer, err := casbinlib.NewSyncedEnforcer(m, &sourceAdapter{
		inline: cfg.policy,
		file:   cfg.policyFile,
	})
	if err != nil {
		return nil, fmt.Errorf("casbin: load policy: %w", err)
	}
	// Configuration sources are deliberately read-only. Business code may
	// still use the SyncedEnforcer mutation API for in-memory policy changes;
	// explicit LoadPolicy atomically restores the configured source.
	enforcer.EnableAutoSave(false)
	return enforcer, nil
}

// buildProviderEnforcer deliberately constructs the enforcer without passing
// adapter to NewSyncedEnforcer. Casbin loads an adapter supplied to its
// constructor immediately, which would run before XBC's Migrate stage. The
// first external policy load belongs to Plugin.start, after migrations.
func buildProviderEnforcer(cfg normalizedConfig, adapter persist.Adapter) (*casbinlib.SyncedEnforcer, error) {
	m, err := loadAndValidateModel(cfg)
	if err != nil {
		return nil, err
	}
	enforcer, err := casbinlib.NewSyncedEnforcer(m)
	if err != nil {
		return nil, fmt.Errorf("casbin: build enforcer: %w", err)
	}
	enforcer.SetAdapter(adapter)
	enforcer.EnableAutoSave(true)
	return enforcer, nil
}

func loadAndValidateModel(cfg normalizedConfig) (model.Model, error) {
	m, err := loadModel(cfg)
	if err != nil {
		return nil, err
	}
	if err := validateRequestShape(m, cfg.requestConvention); err != nil {
		return nil, err
	}
	return m, nil
}

func loadModel(cfg normalizedConfig) (model.Model, error) {
	if cfg.modelFile != "" {
		m, err := model.NewModelFromFile(cfg.modelFile)
		if err != nil {
			return nil, fmt.Errorf("casbin: load model_file %q: %w", cfg.modelFile, err)
		}
		return m, nil
	}

	text := cfg.model
	if strings.TrimSpace(text) == "" {
		if cfg.requestConvention == ConventionPathMethod {
			text = pathMethodModel
		} else {
			text = permissionModel
		}
	}
	m, err := model.NewModelFromString(text)
	if err != nil {
		return nil, fmt.Errorf("casbin: load model: %w", err)
	}
	return m, nil
}

func validateRequestShape(m model.Model, convention RequestConvention) error {
	request, ok := m["r"]["r"]
	if !ok || request == nil {
		return fmt.Errorf("casbin: model must define request r")
	}
	want := 2
	if convention == ConventionPathMethod {
		want = 3
	}
	if len(request.Tokens) != want {
		return fmt.Errorf("casbin: model request r has %d fields, request_convention %q requires %d", len(request.Tokens), convention, want)
	}
	return nil
}

// sourceAdapter reloads either an immutable inline policy or the current file
// contents. SyncedEnforcer supplies the lock that makes LoadPolicy atomic with
// concurrent Enforce calls.
type sourceAdapter struct {
	inline string
	file   string
}

var _ persist.Adapter = (*sourceAdapter)(nil)

func (a *sourceAdapter) LoadPolicy(m model.Model) error {
	text := a.inline
	if a.file != "" {
		contents, err := os.ReadFile(a.file)
		if err != nil {
			return fmt.Errorf("read policy_file %q: %w", a.file, err)
		}
		text = string(contents)
	}
	return loadPolicyText(text, m)
}

func (*sourceAdapter) SavePolicy(model.Model) error { return errReadOnlyPolicySource }
func (*sourceAdapter) AddPolicy(string, string, []string) error {
	return errReadOnlyPolicySource
}
func (*sourceAdapter) RemovePolicy(string, string, []string) error {
	return errReadOnlyPolicySource
}
func (*sourceAdapter) RemoveFilteredPolicy(string, string, int, ...string) error {
	return errReadOnlyPolicySource
}

func loadPolicyText(text string, m model.Model) error {
	for index, raw := range strings.Split(text, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		reader := csv.NewReader(strings.NewReader(line))
		reader.TrimLeadingSpace = true
		reader.FieldsPerRecord = -1
		rule, err := reader.Read()
		if err != nil {
			return fmt.Errorf("policy line %d: %w", index+1, err)
		}
		for i := range rule {
			rule[i] = strings.TrimSpace(rule[i])
		}
		if len(rule) < 2 || rule[0] == "" {
			return fmt.Errorf("policy line %d: expected policy type and fields", index+1)
		}
		ptype := rule[0]
		section := ptype[:1]
		assertions, ok := m[section]
		if !ok {
			return fmt.Errorf("policy line %d: unknown policy section %q", index+1, section)
		}
		assertion, ok := assertions[ptype]
		if !ok || assertion == nil {
			return fmt.Errorf("policy line %d: unknown policy type %q", index+1, ptype)
		}
		if got, want := len(rule)-1, len(assertion.Tokens); got != want {
			return fmt.Errorf("policy line %d: policy type %q has %d fields, want %d", index+1, ptype, got, want)
		}
		if err := persist.LoadPolicyArray(rule, m); err != nil {
			return fmt.Errorf("policy line %d: %w", index+1, err)
		}
	}
	return nil
}
