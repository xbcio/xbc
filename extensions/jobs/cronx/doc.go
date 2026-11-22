// Package cronx supplies XBC's managed cron scheduler in two forms: the cron
// Plugin, which schedules the immutable job snapshot contributed by other
// Definitions, and a standalone Scheduler for a host that owns its scheduling
// lifecycle without an XBC application.
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
//
// # Standalone scheduler
//
// A host that owns its scheduling lifecycle without an XBC application builds
// a Scheduler directly. New needs no plugin context and depends on no runtime:
// it parses specs with the configured parser, evaluates them in the configured
// location, and reports diagnostics through the configured logger, the log
// facade's process logger by default.
//
//	scheduler, err := cronx.New(
//		cronx.WithSecondsOptional(true), // five or six fields, like the legacy facade
//		cronx.WithLocation(time.Local),
//		cronx.WithLogger(myLogger),      // optional; nil uses the facade's process logger
//	)
//	if err != nil {
//		return err
//	}
//	scheduler.Start()
//	defer func() { _ = scheduler.Stop(context.Background()) }()
//
//	id, err := scheduler.Add("*/30 * * * * *", func() {
//		// work
//	})
//	if err != nil {
//		return err
//	}
//	scheduler.Remove(id)
//
// The defaults are time.Local, the standard five-field parser with descriptors
// (Parser(false)), and the log facade's process logger. WithSecondsOptional(true)
// selects the permissive Parser(true), which accepts five or six fields; the
// plugin's Config.Seconds is deliberately different, because a configured
// section states up front that it requires six. Parser is exported for callers
// that need the same parse rules outside a Scheduler.
//
// Add and Remove are safe at runtime and before Start. Add after Stop returns
// ErrStopped; Start is idempotent and a no-op after Stop; Stop stops scheduling,
// waits for invocations already running within the caller's context, and is
// idempotent. An expired Stop context leaves the running work alone, and a later
// Stop can wait for it again.
//
// Time zones are applied as the instance location, never by rewriting a spec
// with a CRON_TZ= prefix, so a bare five-field spec keeps meaning the local
// wall clock. A spec that already carries CRON_TZ= or TZ= keeps its declared
// zone. A nil location, or no WithLocation at all, selects time.Local.
//
// Two options configure diagnostics, and they set one slot: the option
// supplied last wins. WithLogger takes a log.Logger and adapts it with
// LogAdapter; it is the form for a host already logging through the facade.
// WithCronLogger takes a robfig/cron Logger directly and is the narrower form
// for a host with its own logging system: robfig/cron calls only Info and
// Error, so that host implements two methods instead of the facade's seven. A
// nil logger from either option selects the default, the facade's process
// logger resolved at each call, so a scheduler built before log.Init still
// follows the initialized backend. LogAdapter remains exported for a host that
// wraps a facade logger itself.
//
// The standalone form is not a substitute for every plugin capability: it has
// no Job/JobContributor snapshot, distributed lease, workload quota, traffic
// gate, or skip/delay concurrency policy. Every occurrence runs in its own
// goroutine, so overlapping occurrences of one entry may run concurrently, and
// a panicking job is not recovered. Compose Bundle when those behaviors are
// required.
package cronx
