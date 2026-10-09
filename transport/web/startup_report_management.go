package web

import (
	"fmt"
	"strings"
)

// managementMark labels a report row that belongs to the management plane.
//
// The two planes share one route table, and the table is what the report
// prints, so without the mark a row an operator reads as reachable on the
// serving listener may in fact be served -- and only served -- on the second
// one. The mark is appended after the row's own columns, like the other row
// marks, so it cannot disturb their alignment.
func managementMark(route RouteInfo) string {
	if route.Management {
		return "  [management]"
	}
	return ""
}

// renderManagementPlane states what the deployment decided about the second
// listener and how many rows that decision moved off the serving one. It
// returns nothing when no management address is configured, so the default
// report stays byte-identical to what it was before the plane existed.
//
// boundAddr distinguishes the two reports the same decision is printed in and
// names the listener in the one that has it: a boot passes the address it
// actually bound -- not the configured spelling, which for a port chosen by the
// kernel is not the port it is serving on -- while the validate command
// assembles the same pipeline, binds nothing, and passes empty. Saying which of
// the two the reader holds is the point: a validate output must not read as
// evidence that the port is being served.
func renderManagementPlane(cfg ManagementConfig, routes []RouteInfo, boundAddr string) string {
	if strings.TrimSpace(cfg.Addr) == "" {
		return ""
	}

	addr := boundAddr
	state := "bound"
	if addr == "" {
		// The configured spelling is still the resolved one, so a hostless
		// address reports the loopback address it would bind. See
		// ManagementConfig.bindAddr.
		addr = cfg.bindAddr()
		state = "configured, not bound by this report"
	}
	served := "no routes registered"
	if rows := managementRows(routes); rows == 1 {
		served = "1 row marked [management]"
	} else if rows > 1 {
		served = fmt.Sprintf("%d rows marked [management]", rows)
	}
	return fmt.Sprintf("web: management listener %s (%s; %s)", addr, state, served)
}

// managementRows counts the rows the management plane owns.
func managementRows(routes []RouteInfo) int {
	rows := 0
	for _, route := range routes {
		if route.Management {
			rows++
		}
	}
	return rows
}
