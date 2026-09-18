// Package cron supplies XBC's managed cron scheduler.
//
// # Usage
//
// Define each job set as a canonical Plugin that explicitly exports
// JobContributor, then compose its Bundle with cron.Bundle:
//
//	type cleanupJob struct{}
//
//	func (cleanupJob) Name() string                  { return "session_cleanup" }
//	func (cleanupJob) Spec() string                  { return "@daily" }
//	func (cleanupJob) Run(context.Context) error     { return nil }
//
//	type sessionJobs struct{}
//
//	func (*sessionJobs) Jobs() []cron.Job {
//		return []cron.Job{cleanupJob{}}
//	}
//
//	var definition = plugin.Define(
//		"session-jobs",
//		func(plugin.BuildContext) (*sessionJobs, error) { return &sessionJobs{}, nil },
//		plugin.Options[*sessionJobs]{
//			Exports: plugin.Contracts(
//				plugin.ExportAs[cron.JobContributor](
//					func(jobs *sessionJobs) cron.JobContributor { return jobs },
//				),
//			),
//		},
//	)
//
//	func Definition() plugin.Definition { return definition }
//
//	func Bundle() plugin.Bundle { return plugin.BundleOf(Definition()) }
//
//	func newApp() (*xbc.App, error) {
//		return xbc.New(xbc.WithBundles(
//			cron.Bundle(),
//			Bundle(),
//		))
//	}
//
// Cron collects contributors as a typed input while constructing its primary
// value. Jobs is called once, and the resulting snapshot cannot change after
// construction. A custom distributed lock backend is composed the same way by
// exporting lease.Locker from another Definition, for example the one
// redis.Bundle() selects through plugins.redis-lease. Alternatively,
// distributed.redis_instance selects a named Redis Definition instance and
// distributed.redis.addr lets cron own its client.
//
// Definition returns the canonical declaration and Bundle is side-effect free.
// Executables that deliberately choose process-wide composition may import the
// autoload subpackage. Every runner and lease-renewal manager is admitted during
// Start and waits for XBC's traffic gate before scheduling. Job contexts are
// canceled on shutdown or lease loss; jobs should honor cancellation and keep
// externally visible work idempotent because a lease cannot fence code that
// ignores it.
package cron
