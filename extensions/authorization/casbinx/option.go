package casbinx

import (
	"errors"
	"fmt"
	"strings"

	"github.com/casbin/casbin/v2/persist"
	casbinrbac "github.com/casbin/casbin/v2/rbac"
)

// Option customizes direct Plugin construction through New. Options have no
// configuration-file form -- an adapter and a matching function are values,
// not YAML -- so Definition/Bundle composition, whose factory receives only
// Config, cannot apply them. A composed plugin keeps the defaults described
// here.
type Option func(*options)

// options is the direct-construction state Options collect before New builds
// the enforcer.
type options struct {
	adapter     persist.Adapter
	adapterSet  bool
	autoSave    bool
	autoSaveSet bool
	matching    []namedMatchingFunc
}

// namedMatchingFunc is one deferred role matching function registration.
type namedMatchingFunc struct {
	ptype string
	name  string
	fn    casbinrbac.MatchingFunc
}

// WithAdapter uses adapter as the plugin's external policy source for a
// directly constructed Plugin. It is mutually exclusive with Config.Policy,
// Config.PolicyFile, and a configured Config.Adapter provider reference: the
// adapter is the source.
//
// The adapter is attached and loaded during New, because direct construction
// has no XBC Migrate stage and no mandatory Start to defer to; Enforce answers
// from adapter contents as soon as New returns. Autosave is enabled by
// default, matching the configured-provider path; use WithAutoSave(false) to
// keep mutations in memory only. LoadPolicy reloads from the adapter in both
// modes. The Plugin never closes or migrates the adapter: its lifecycle and
// schema remain the caller's.
//
// A configured adapter provider selected through Definition/Bundle is
// unchanged: it is attached during construction, first loaded in Start after
// migrations, and always autosaves.
func WithAdapter(adapter persist.Adapter) Option {
	return func(o *options) {
		o.adapter = adapter
		o.adapterSet = true
	}
}

// WithAutoSave sets Casbin autosave for an adapter supplied through
// WithAdapter. It requires WithAdapter and is rejected otherwise, because
// inline and file policy sources are deliberately read-only. The default for
// an external adapter is enabled: every policy mutation is written through to
// the adapter before it is applied in memory. With autosave disabled, Casbin
// mutations live only in memory until the next LoadPolicy restores the
// adapter's contents.
func WithAutoSave(enabled bool) Option {
	return func(o *options) {
		o.autoSave = enabled
		o.autoSaveSet = true
	}
}

// WithRoleMatchingFunc registers fn as Casbin's named role matching function
// for ptype, with the semantics of Casbin's Enforcer.AddNamedMatchingFunc.
// ptype names the model's role definition ("g" for the default one) and must
// exist in the model; name is the registration name Casbin records. Casbin's
// default role manager keeps one matching function per role definition, so a
// later registration for the same ptype replaces an earlier one.
//
// The function is registered after the enforcer and model are built but
// before the first policy load, so the role links that load builds already
// consult it; LoadPolicy never drops it. This is the hook that makes wildcard
// role subjects work, for example registering KeyMatch2 so a grouping rule
// naming reader:1 satisfies a policy addressed to reader:*. It applies to both
// policy sources the direct constructor supports: inline/file policy and an
// adapter supplied through WithAdapter.
func WithRoleMatchingFunc(ptype, name string, fn casbinrbac.MatchingFunc) Option {
	return func(o *options) {
		o.matching = append(o.matching, namedMatchingFunc{ptype: ptype, name: name, fn: fn})
	}
}

// validate checks the option set that normalizeConfig cannot see. It runs
// before any enforcer is built, so a rejected option never touches the
// adapter.
func (o options) validate() error {
	if o.adapterSet && isNil(o.adapter) {
		return errors.New("casbin: WithAdapter requires a non-nil adapter")
	}
	if o.autoSaveSet && !o.adapterSet {
		return errors.New("casbin: WithAutoSave requires WithAdapter; inline and file policy sources are read-only")
	}
	for _, matching := range o.matching {
		if strings.TrimSpace(matching.ptype) == "" {
			return errors.New("casbin: WithRoleMatchingFunc requires a non-empty ptype")
		}
		if strings.TrimSpace(matching.name) == "" {
			return fmt.Errorf("casbin: WithRoleMatchingFunc for ptype %q requires a non-empty name", matching.ptype)
		}
		if matching.fn == nil {
			return fmt.Errorf("casbin: WithRoleMatchingFunc %q for ptype %q requires a non-nil function", matching.name, matching.ptype)
		}
	}
	return nil
}

// autoSaveEnabled reports the autosave value for an adapter-backed enforcer:
// enabled unless WithAutoSave said otherwise.
func (o options) autoSaveEnabled() bool {
	if o.autoSaveSet {
		return o.autoSave
	}
	return true
}
