package runtime

import (
	"strings"

	"github.com/xbcio/xbc/internal/assembly"
)

func (a *App) reportPlan(plan *assembly.Plan, migrate bool) {
	order := plan.Order()
	labels := make([]string, len(order))
	for index, identity := range order {
		labels[index] = identity.String()
	}
	a.log().Info("xbc: assembly plan complete",
		"instances", len(order),
		"order", strings.Join(labels, ","),
		"migration", migrate,
	)
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
