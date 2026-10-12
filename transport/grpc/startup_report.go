package grpc

import (
	"fmt"
	"strings"

	"github.com/xbcio/xbc/log"
	"github.com/xbcio/xbc/plugin"
)

// renderStartupReport prints what a boot decided about its ingress: TLS
// termination when the section declares it, the interceptor chains, the soft
// ordering misses, the method table, and -- when enabled -- reflection, which
// is served but owned by no registrant. OpenTraffic prints it just before the
// gate opens, naming the bound address; Preflight prints the same report with
// an empty address for a process that will never serve, and the report says
// so rather than leaving the reader to infer it from the command.
func renderStartupReport(logger log.Logger, boundAddr string, a assembly) {
	logger.Info(renderListener(a.cfg.Addr, boundAddr))
	if line := renderTLS(a.cfg.TLS); line != "" {
		logger.Info(line)
	}
	logger.Info(renderUnaryChain(a.unary))
	logger.Info(renderStreamChain(a.stream))
	if len(a.misses) > 0 {
		logger.Info(renderSoftMisses(a.misses))
	}
	logger.Info(renderMethodTable(a.services))
	if a.cfg.Reflection.Enabled {
		logger.Info(renderReflection())
	}
	if a.auth != nil {
		logger.Info(renderAuthentication(a.auth))
		if a.auth.permitAll {
			logger.Warn("grpc: security default is permit; every method no rule or declaration covers is served without authentication")
		}
	}
}

// renderAuthentication reports the arbitration order and every method's
// decision with the tier that made it, so an operator can see that an
// application rule overrode a plugin's declaration without reading its source.
func renderAuthentication(stage *authenticationStage) string {
	var b strings.Builder
	fmt.Fprintf(&b, "grpc: authentication order: %s", formatSchemeList(stage.schemes))
	width := 0
	for _, decision := range stage.decisions {
		width = max(width, len(decision.fullMethod))
	}
	for _, decision := range stage.decisions {
		policy := decision.policy
		outcome := "permit"
		if !policy.permit {
			outcome = "authenticate "
			if policy.selection.UsesDefault() {
				outcome += "(any registered scheme)"
			} else {
				outcome += formatSchemeList(policy.selection.Schemes())
			}
		}
		tier := policy.tier.String()
		if policy.tier == tierApplicationRule {
			tier = fmt.Sprintf("%s #%d", tier, policy.ruleIndex)
		}
		fmt.Fprintf(&b, "\n  %-*s  %s  [%s]", width, decision.fullMethod, outcome, tier)
	}
	return b.String()
}

func renderListener(configured, bound string) string {
	if bound == "" {
		return fmt.Sprintf("grpc: listener %s (not bound: validate run)", configured)
	}
	return fmt.Sprintf("grpc: listener bound on %s", bound)
}

func renderUnaryChain(chain []plugin.Entry[UnaryInterceptor]) string {
	orders := make([]chainLine, len(chain))
	for i, entry := range chain {
		orders[i] = chainLine{identity: entry.Identity, order: entry.Value.Order()}
	}
	return renderChain(unaryChain, orders)
}

func renderStreamChain(chain []plugin.Entry[StreamInterceptor]) string {
	orders := make([]chainLine, len(chain))
	for i, entry := range chain {
		orders[i] = chainLine{identity: entry.Identity, order: entry.Value.Order()}
	}
	return renderChain(streamChain, orders)
}

type chainLine struct {
	identity plugin.Identity
	order    Order
}

func renderChain(kind chainKind, lines []chainLine) string {
	width := 0
	for _, line := range lines {
		width = max(width, len(line.identity.String()))
	}
	var b strings.Builder
	fmt.Fprintf(&b, "grpc: %s interceptor chain (%d)", kind, len(lines))
	for i, line := range lines {
		text := fmt.Sprintf("  %d. %-*s  [%s]", i+1, width, line.identity, line.order.Phase)
		if len(line.order.After) > 0 {
			text += "  after=" + joinOrderRefs(line.order.After)
		}
		if len(line.order.Before) > 0 {
			text += "  before=" + joinOrderRefs(line.order.Before)
		}
		b.WriteString("\n")
		b.WriteString(text)
	}
	return b.String()
}

func joinOrderRefs(refs []OrderRef) string {
	values := make([]string, len(refs))
	for i, ref := range refs {
		values[i] = ref.String()
	}
	return strings.Join(values, ",")
}

func renderSoftMisses(misses []InterceptorOrderMiss) string {
	var b strings.Builder
	b.WriteString("grpc: optional ordering preferences with no target (chain unaffected)")
	for _, miss := range misses {
		fmt.Fprintf(&b,
			"\n  %s interceptor %s prefers to run %s %s, which contributes no %s interceptor",
			miss.Chain, miss.Interceptor, miss.Direction, miss.Reference, miss.Chain)
	}
	return b.String()
}

// renderReflection states the one fact reflection adds beside the method
// table: the standard services are served, their methods resolve public
// because no registrant declared them, and an application rule can still
// tighten that.
func renderReflection() string {
	names := make([]string, len(reflectionServiceDescs))
	for i, desc := range reflectionServiceDescs {
		names[i] = desc.ServiceName
	}
	return fmt.Sprintf("grpc: server reflection enabled: %s are served; their methods are public unless a grpc.security rule covers them",
		strings.Join(names, " and "))
}

func renderMethodTable(services []*registeredService) string {
	count := 0
	width := 0
	for _, service := range services {
		count += len(service.methods)
		for _, method := range service.methods {
			width = max(width, len(method.fullMethod))
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "grpc: method table (%d services, %d methods)", len(services), count)
	for _, service := range services {
		for _, method := range service.methods {
			fmt.Fprintf(&b, "\n  %-*s  %-13s  %s", width, method.fullMethod, method.kind, service.contributor)
		}
	}
	return b.String()
}
