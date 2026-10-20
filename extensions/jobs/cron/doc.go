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
// # TLS
//
// The client cron owns can speak TLS:
//
//	plugins:
//	  cron:
//	    distributed:
//	      enabled: true
//	      redis:
//	        addr: "redis.internal:6379"
//	        tls:
//	          ca_file: /etc/xbc/redis/ca.pem
//	          cert_file: /etc/xbc/redis/client.pem   # optional, for mTLS
//	          key_file: /etc/xbc/redis/client-key.pem
//	          server_name: redis.internal
//	          min_version: "1.2"
//
// The section is off by default and the connection stays in cleartext until one
// of ca_file, cert_file, key_file, server_name, or insecure_skip_verify is set.
// ca_file replaces the system roots; cert_file and key_file must be configured
// together. min_version accepts "1.2" or "1.3" only. The material is read when
// the Plugin is constructed, so an unreadable file fails construction rather
// than the first lock operation. insecure_skip_verify disables certificate
// verification: it exists for development endpoints and must not be used in
// production.
//
// The section belongs to the client cron owns. When distributed.redis_instance
// selects a client exported by another plugin, that plugin owns its TLS
// configuration and configuring distributed.redis.tls here is rejected rather
// than silently ignored. The same applies when distributed mode is enabled but
// neither distributed.redis.addr nor distributed.redis_instance is set: the
// lock then comes from an externally exported lease.Locker, cron opens no Redis
// connection of its own, and configuring distributed.redis.tls is rejected with
// an error saying the section has nowhere to take effect. When distributed mode
// is disabled the whole distributed.redis section is unused, TLS included.
//
// Definition returns the canonical declaration and Bundle is side-effect free.
// Executables that deliberately choose process-wide composition may import the
// autoload subpackage. Every runner and lease-renewal manager is admitted during
// Start and waits for XBC's traffic gate before scheduling. Drain stops
// scheduling new invocations and waits for the running ones within the drain
// budget, without cancelling them or their lease renewal. Job contexts are
// canceled by Stop or on lease loss; jobs should honor cancellation and keep
// externally visible work idempotent because a lease cannot fence code that
// ignores it.
//
// A process whose Plugins contribute no job is not an error: it starts,
// schedules nothing, and provides the long-lived task the runtime requires of a
// running process, which is the shape a standby between roles has.
//
// Every invocation charges one unit of the workload its contributor belongs to
// (workloads.<key>.max_goroutines) for as long as it runs, taken before the
// distributed lock so a replica waiting for quota does not hold the job's lock
// while it waits. That is what keeps work the scheduler runs on a workload's
// behalf inside the budget the workload declared; an invocation whose
// contributor belongs to no workload charges nothing. An invocation that cannot
// take a unit waits rather than skipping the occurrence, and a wait that ends
// with the shutdown abandons that invocation.
package cron
