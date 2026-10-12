package metrics

import (
	"fmt"
	"runtime"
	"runtime/debug"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/xbcio/xbc/plugin"
)

// buildInfoName is the metric name the identity series is published under,
// joined under the configured namespace where one is set: "orders" yields
// "orders_build_info". It is a plain series rather than an instrument with a
// lifecycle because it never changes while the process runs.
const buildInfoName = "build_info"

// readBuildInfo is the injectable seam over the Go toolchain's build
// metadata, the same shape runtime's settings use: a variable so tests can
// hand the revision lookup a documented build instead of whatever this
// repository carries, and nil for a build stripped of its metadata. Nothing
// outside this package may reassign it.
var readBuildInfo = func() *debug.BuildInfo {
	info, _ := debug.ReadBuildInfo()
	return info
}

// registerBuildInfo publishes the one build_info series every scrape gets:
// value 1, carrying the process identity as labels. A dashboard joins it onto
// any other series to answer "which build do these numbers come from", which
// is why it is registered by the plugin rather than left for applications to
// build -- every application exporting metrics should have it.
//
// The labels are service, version, revision and go_version in the spirit of
// the Prometheus build_info convention. revision is the VCS revision the
// build recorded, in full: it is a label selected on, not a version shown to
// a human, and truncating it would defeat the one comparison it exists for
// ("is the failed replica really on the same commit as the healthy ones").
// Deliberately absent: an instance label. Instance is the scrape target's
// business, supplied by the collector as a target label; baking it into the
// series would multiply every other series by the replica count.
func registerBuildInfo(registry *Registry, namespace string, service plugin.ServiceIdentity) error {
	gauge := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      buildInfoName,
		Help:      "Build identity of the running service: always 1, described by its labels.",
	}, []string{"service", "version", "revision", "go_version"})
	gauge.WithLabelValues(service.Name, service.Version, buildRevision(), runtime.Version()).Set(1)
	if err := registry.Register(gauge); err != nil {
		return fmt.Errorf("metrics: register %s: %w", gaugeName(namespace), err)
	}
	return nil
}

// buildRevision reads the VCS revision the build recorded, and answers the
// empty string for a build that recorded none -- a module installed by
// `go install`, a build outside version control. The label then documents
// "not recorded" rather than inventing a value.
func buildRevision() string {
	info := readBuildInfo()
	if info == nil {
		return ""
	}
	for _, setting := range info.Settings {
		if setting.Key == "vcs.revision" {
			return setting.Value
		}
	}
	return ""
}

// gaugeName renders the registered series name for error reporting, joined
// the way prometheus joins a namespace onto a metric name.
func gaugeName(namespace string) string {
	if namespace == "" {
		return buildInfoName
	}
	return namespace + "_" + buildInfoName
}
