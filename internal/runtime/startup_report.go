package runtime

import (
	"fmt"
	"strings"

	"github.com/xbcio/xbc/internal/assembly"
)

// reportDisabled lists declared-but-not-enabled Definitions with the reason
// each one stayed off.
//
// Being disabled is normal and not a warning, but "I selected it and nothing
// happened" is otherwise indistinguishable from "I selected it and it
// silently failed", and this line is what separates the two. Only keys,
// paths and reasons are printed -- never a configured value.
func (a *App) reportDisabled(plan *assembly.Plan) {
	disabled := plan.DisabledDetail()
	if len(disabled) == 0 {
		return
	}
	var b strings.Builder
	fmt.Fprintf(&b, "xbc: disabled plugins (%d)", len(disabled))
	for _, entry := range disabled {
		fmt.Fprintf(&b, "\n  %s — %s", entry.Key, entry.Reason)
	}
	b.WriteString("\n  → Being disabled is not an error; run the doctor subcommand for the full configuration picture")
	a.log().Info(b.String())
}

func (a *App) reportStarted(instances []*assembly.Instance, migrate bool) {
	labels := make([]string, len(instances))
	for index, instance := range instances {
		labels[index] = instance.Identity().String()
	}
	a.log().Info("xbc: application traffic gate released",
		"instances", len(instances),
		"order", strings.Join(labels, ","),
		"migration", migrate,
	)
}
