package cron

import "context"

// Job is one independently scheduled unit of business work. Name must be a
// stable lowercase identifier accepted by plugin.ValidateName. Run must honor
// ctx cancellation so lease loss and application shutdown stop work promptly.
type Job interface {
	Name() string
	Spec() string
	Run(ctx context.Context) error
}

// JobContributor is a typed contribution contract exported by business
// Definitions. Jobs is called once by cron's primary factory; implementations
// must return a stable snapshot and must not acquire lifecycle-owned resources.
type JobContributor interface {
	Jobs() []Job
}
