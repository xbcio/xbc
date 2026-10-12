package grpc

import (
	"fmt"

	"google.golang.org/grpc"
	v1reflection "google.golang.org/grpc/reflection/grpc_reflection_v1"
	v1alphareflection "google.golang.org/grpc/reflection/grpc_reflection_v1alpha"
)

// reflectionServiceDescs are the descriptors of the two standard services
// grpc-go's reflection.Register installs. They are taken from the generated
// packages rather than restated, so the method table compiled into the
// authentication policy and the services actually served cannot disagree.
var reflectionServiceDescs = []*grpc.ServiceDesc{
	&v1reflection.ServerReflection_ServiceDesc,
	&v1alphareflection.ServerReflection_ServiceDesc,
}

// checkReflectionCollisions rejects a frozen table that already owns either
// standard reflection service name. reflection.Register would otherwise reach
// grpc-go's own duplicate-service check, which terminates the process instead
// of failing startup.
func checkReflectionCollisions(services []*registeredService) error {
	for _, desc := range reflectionServiceDescs {
		for _, service := range services {
			if service.desc.ServiceName == desc.ServiceName {
				return fmt.Errorf(
					"xbc: grpc.reflection is enabled, but plugin %s registers a service named %q, which the standard reflection service must own",
					service.contributor, desc.ServiceName)
			}
		}
	}
	return nil
}

// reflectionPolicies resolves the standard reflection methods as the
// authentication policy table would resolve them had they been registered by a
// plugin that declared Public: an application rule still outranks the
// declaration, which is what lets grpc.security tighten reflection without
// disabling it. The services themselves are installed by reflection.Register,
// not through the Registrar, so nothing else compiles their methods.
func reflectionPolicies(policies *policySet) map[string]effectivePolicy {
	public := Public()
	compiled := make(map[string]effectivePolicy, len(reflectionServiceDescs))
	for _, desc := range reflectionServiceDescs {
		for _, method := range desc.Methods {
			info := methodInfo{fullMethod: "/" + desc.ServiceName + "/" + method.MethodName, name: method.MethodName, auth: &public}
			compiled[info.fullMethod] = policies.resolve(desc.ServiceName, info)
		}
		for _, stream := range desc.Streams {
			info := methodInfo{fullMethod: "/" + desc.ServiceName + "/" + stream.StreamName, name: stream.StreamName, auth: &public}
			compiled[info.fullMethod] = policies.resolve(desc.ServiceName, info)
		}
	}
	return compiled
}
