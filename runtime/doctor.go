package runtime

import (
	"fmt"
	"io"
	"strings"

	"github.com/xbcio/xbc/plugin/assembly"
)

// doctorSubcommand names the read-only diagnostic command.
const doctorSubcommand = "doctor"

// reportDoctor writes the outcome of a complete, read-only assembly: which
// plugins the configuration turned on, which section each instance binds, why
// the remaining Definitions stayed off, and which sources contributed.
//
// Everything printed here is a path, an identity or a source label. No
// configured value is ever written, which is what makes the command safe to
// run against a production environment full of secrets. Sections are separated
// by a blank line so a later command can append its own without reformatting
// the ones above it.
func (a *App) reportDoctor(plan *assembly.Plan, migrate bool) {
	out := a.diagnostics()

	fmt.Fprintln(out, "xbc doctor")
	fmt.Fprintln(out)

	fmt.Fprintln(out, "configuration")
	sources := a.env.Sources()
	if len(sources) == 0 {
		fmt.Fprintln(out, "  sources  (none; every value comes from a declared default)")
	} else {
		fmt.Fprintf(out, "  sources  %s\n", strings.Join(sources, ", "))
	}
	fmt.Fprintf(out, "  roots    %s\n", strings.Join(a.env.Roots(), ", "))
	fmt.Fprintln(out)

	order := plan.Order()
	disabled := plan.DisabledDetail()
	fmt.Fprintln(out, "plugins")
	fmt.Fprintf(out, "  declared %d, enabled instances %d, disabled %d\n",
		plan.DefinitionCount(), len(order), len(disabled))
	fmt.Fprintln(out)

	fmt.Fprintln(out, "enabled instances, in start order")
	if len(order) == 0 {
		fmt.Fprintln(out, "  (none)")
	}
	for _, identity := range order {
		path := plan.InstanceConfigPath(identity)
		fmt.Fprintf(out, "  %-28s config %-28s from %s\n",
			identity.String(), path, describeOrigins(a.env.OriginsUnder(path)))
	}
	fmt.Fprintln(out)

	fmt.Fprintln(out, "disabled plugins")
	if len(disabled) == 0 {
		fmt.Fprintln(out, "  (none)")
	}
	for _, entry := range disabled {
		fmt.Fprintf(out, "  %-28s %s\n", entry.Key.String(), entry.Reason)
	}
	fmt.Fprintln(out)

	fmt.Fprintln(out, "migration")
	fmt.Fprintf(out, "  this run would migrate: %t\n", migrate)
}

// describeOrigins renders where a section's values came from. A section nobody
// configured is reported as such rather than left blank, so that an ENV-only
// activation is visible at a glance.
func describeOrigins(origins []string) string {
	if len(origins) == 0 {
		return "declared defaults only"
	}
	return strings.Join(origins, ", ")
}

// diagnostics is where read-only command output goes. Tests replace it; the
// process adapter owns the real stream.
func (a *App) diagnostics() io.Writer {
	if a.out != nil {
		return a.out
	}
	return stdout
}
