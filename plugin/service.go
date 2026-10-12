package plugin

import pluginmodel "github.com/xbcio/xbc/plugin/model"

// fromInternalServiceIdentity converts the model layer's mirror of this type.
// The two are structurally identical on purpose: the model layer cannot
// import the facade, so the direction is one-way and the conversion is a
// plain field copy.
func fromInternalServiceIdentity(identity pluginmodel.ServiceIdentity) ServiceIdentity {
	return ServiceIdentity{Name: identity.Name, Version: identity.Version, Instance: identity.Instance}
}

// ServiceIdentity is the process-wide identity a Plugin answers with: which
// service this process runs, at which version, as which instance. It is the
// single source every consumer stamps -- the structured log fields, the
// tracing resource, the build-info metric, the startup report -- so one
// process never presents two spellings of itself.
//
// The zero value is what a Context with no runtime behind it reports, which a
// Plugin under a real application never sees.
type ServiceIdentity struct {
	// Name names the service, shared by every replica of one deployment:
	// xbc.service.name when the deployment set it, otherwise derived by the
	// runtime from the build information or the executable's file name.
	Name string

	// Version names the service build, shared by every replica of one
	// deployment: xbc.service.version when the deployment set it, otherwise
	// derived by the runtime from the build information. "unknown" when
	// nothing identifies the build.
	Version string

	// Instance names this process among the replicas of the service: the
	// configured xbc.instance_id when the deployment gave one, and the
	// runtime's derived default otherwise. It is the same string
	// ProcessInstance reports.
	Instance string
}
