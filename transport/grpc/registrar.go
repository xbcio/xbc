package grpc

import (
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"

	"google.golang.org/grpc"

	"github.com/xbcio/xbc/plugin"
)

// ServiceRegistrar is the consumer-owned contract for a plugin that contributes
// gRPC services. RegisterServices is called once per assembly with a Registrar
// scoped to the contributing plugin; generated registration functions accept it
// directly:
//
//	func (p *Plugin) RegisterServices(r *xbcgrpc.Registrar) error {
//		greeterpb.RegisterGreeterServer(r, p.greeter)
//		return nil
//	}
//
// A returned error fails startup, attributed to the contributor.
type ServiceRegistrar interface {
	RegisterServices(*Registrar) error
}

// Registrar records the services one contributor registers. It implements
// grpc.ServiceRegistrar so generated code needs no adapter, but it does not
// forward to a grpc.Server: registrations are collected, checked, and frozen
// before any server exists, and only then installed. That ordering is what lets
// a duplicate service name or a mistyped implementation fail startup -- and the
// validate command -- with an error, instead of reaching grpc-go's own checks,
// which terminate the process.
//
// A Registrar is valid only for the duration of the RegisterServices call it
// was passed to. Registering through one retained past that call panics: the
// service table it fed has been frozen, and a service added after the freeze
// would be served without having been reported or checked.
type Registrar struct {
	table       *serviceTable
	contributor plugin.Identity
	closed      bool
}

var _ grpc.ServiceRegistrar = (*Registrar)(nil)

// RegisterService records desc as implemented by impl. It satisfies
// grpc.ServiceRegistrar, whose signature has no error result, so a rejected
// registration is recorded and fails assembly once the contributor's
// RegisterServices returns.
func (r *Registrar) RegisterService(desc *grpc.ServiceDesc, impl any) {
	if r == nil || r.table == nil {
		panic("xbc: gRPC Registrar used outside the Server that created it")
	}
	r.table.add(r, desc, impl)
}

// MethodKind describes the call shape of one RPC.
type MethodKind int

const (
	// Unary is a single request answered by a single response.
	Unary MethodKind = iota
	// ServerStream is a single request answered by a stream of responses.
	ServerStream
	// ClientStream is a stream of requests answered by a single response.
	ClientStream
	// BidiStream streams in both directions.
	BidiStream
)

func (k MethodKind) String() string {
	switch k {
	case Unary:
		return "unary"
	case ServerStream:
		return "server-stream"
	case ClientStream:
		return "client-stream"
	case BidiStream:
		return "bidi-stream"
	default:
		return fmt.Sprintf("kind(%d)", int(k))
	}
}

// methodInfo is one entry of the frozen method table. auth is the method's
// effective registration-time declaration -- its own, or its service's -- and
// nil when neither declared one. perm is the effective permission declared
// for authorization, its own or its service's, and empty when neither
// declared one.
type methodInfo struct {
	fullMethod string
	name       string
	kind       MethodKind
	auth       *AuthPolicy
	perm       string
}

// registeredService is one entry of the frozen service table.
type registeredService struct {
	desc        *grpc.ServiceDesc
	impl        any
	contributor plugin.Identity
	methods     []methodInfo
	serviceAuth *AuthPolicy
	servicePerm string
}

// DuplicateServiceError reports a service name registered more than once. A
// gRPC server routes by full method name, so two registrations of one service
// cannot both be served; grpc-go itself treats the condition as fatal.
type DuplicateServiceError struct {
	Service       string
	First, Second plugin.Identity
}

func (e *DuplicateServiceError) Error() string {
	if e.First == e.Second {
		return fmt.Sprintf("xbc: gRPC service %q is registered more than once by %s", e.Service, e.First)
	}
	return fmt.Sprintf("xbc: gRPC service %q is registered by both %s and %s; a service has exactly one implementation", e.Service, e.First, e.Second)
}

// serviceTable collects registrations from every contributor of one assembly.
// Its mutex also guards each Registrar's closed flag, so a contributor that
// registers from a goroutine of its own -- inside RegisterServices or, wrongly,
// after it returned -- is either recorded or refused, never raced.
type serviceTable struct {
	mu               sync.Mutex
	services         map[string]*registeredService
	declarations     []authDeclaration
	permDeclarations []permDeclaration
	errs             []error
}

func newServiceTable() *serviceTable {
	return &serviceTable{services: make(map[string]*registeredService)}
}

func (t *serviceTable) add(registrar *Registrar, desc *grpc.ServiceDesc, impl any) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if registrar.closed {
		panic("xbc: gRPC service table is frozen; register services only inside RegisterServices")
	}
	contributor := registrar.contributor
	service, err := checkService(contributor, desc, impl)
	if err != nil {
		t.errs = append(t.errs, err)
		return
	}
	if existing, ok := t.services[desc.ServiceName]; ok {
		t.errs = append(t.errs, &DuplicateServiceError{
			Service: desc.ServiceName,
			First:   existing.contributor,
			Second:  contributor,
		})
		return
	}
	t.services[desc.ServiceName] = service
}

func (t *serviceTable) declare(registrar *Registrar, declaration authDeclaration) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if registrar.closed {
		panic("xbc: gRPC service table is frozen; declare method options only inside RegisterServices")
	}
	t.declarations = append(t.declarations, declaration)
}

func (t *serviceTable) declarePerm(registrar *Registrar, declaration permDeclaration) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if registrar.closed {
		panic("xbc: gRPC service table is frozen; declare method options only inside RegisterServices")
	}
	t.permDeclarations = append(t.permDeclarations, declaration)
}

// checkService applies, as errors, the checks grpc-go applies as process
// termination, plus the ones it does not apply at all: a nil handler or a
// method name used twice within one service would otherwise surface as a panic
// or a silently unreachable method on the first matching call.
func checkService(contributor plugin.Identity, desc *grpc.ServiceDesc, impl any) (*registeredService, error) {
	if desc == nil {
		return nil, fmt.Errorf("xbc: plugin %s registered a gRPC service with a nil ServiceDesc", contributor)
	}
	name := desc.ServiceName
	if name == "" || strings.ContainsAny(name, "/ \t\r\n") {
		return nil, fmt.Errorf("xbc: plugin %s registered a gRPC service with invalid name %q", contributor, name)
	}
	if impl == nil {
		return nil, fmt.Errorf("xbc: plugin %s registered gRPC service %q with a nil implementation", contributor, name)
	}
	handlerType := reflect.TypeOf(desc.HandlerType)
	if handlerType == nil || handlerType.Kind() != reflect.Pointer || handlerType.Elem().Kind() != reflect.Interface {
		return nil, fmt.Errorf("xbc: plugin %s registered gRPC service %q whose HandlerType is not a pointer to an interface", contributor, name)
	}
	if implType := reflect.TypeOf(impl); !implType.Implements(handlerType.Elem()) {
		return nil, fmt.Errorf("xbc: plugin %s registered gRPC service %q with %v, which does not implement %v", contributor, name, implType, handlerType.Elem())
	}

	seen := make(map[string]bool, len(desc.Methods)+len(desc.Streams))
	methods := make([]methodInfo, 0, len(desc.Methods)+len(desc.Streams))
	record := func(method string, handlerMissing bool, kind MethodKind) error {
		if method == "" || strings.ContainsAny(method, "/ \t\r\n") {
			return fmt.Errorf("xbc: gRPC service %q registered by %s has a method with invalid name %q", name, contributor, method)
		}
		if seen[method] {
			return fmt.Errorf("xbc: gRPC service %q registered by %s declares method %q more than once", name, contributor, method)
		}
		if handlerMissing {
			return fmt.Errorf("xbc: gRPC method /%s/%s registered by %s has a nil handler", name, method, contributor)
		}
		seen[method] = true
		methods = append(methods, methodInfo{fullMethod: "/" + name + "/" + method, name: method, kind: kind})
		return nil
	}
	for _, method := range desc.Methods {
		if err := record(method.MethodName, method.Handler == nil, Unary); err != nil {
			return nil, err
		}
	}
	for _, stream := range desc.Streams {
		kind := BidiStream
		switch {
		case stream.ServerStreams && !stream.ClientStreams:
			kind = ServerStream
		case stream.ClientStreams && !stream.ServerStreams:
			kind = ClientStream
		}
		if err := record(stream.StreamName, stream.Handler == nil, kind); err != nil {
			return nil, err
		}
	}
	slices.SortFunc(methods, func(a, b methodInfo) int { return strings.Compare(a.fullMethod, b.fullMethod) })

	// Copy the descriptor: generated code passes a package-level value, and
	// the frozen table must not change if a caller later edits theirs.
	copied := *desc
	copied.Methods = slices.Clone(desc.Methods)
	copied.Streams = slices.Clone(desc.Streams)
	return &registeredService{desc: &copied, impl: impl, contributor: contributor, methods: methods}, nil
}

// collectServices runs every contributor against a fresh table and freezes it.
// Each contributor gets its own Registrar, closed as soon as its call returns,
// so a retained Registrar cannot feed a table that has already been checked.
func collectServices(contributors []plugin.Entry[ServiceRegistrar]) ([]*registeredService, error) {
	table := newServiceTable()
	for _, entry := range contributors {
		registrar := &Registrar{table: table, contributor: entry.Identity}
		err := entry.Value.RegisterServices(registrar)
		table.mu.Lock()
		registrar.closed = true
		table.mu.Unlock()
		if err != nil {
			return nil, fmt.Errorf("xbc: plugin %s RegisterServices failed: %w", entry.Identity, err)
		}
	}
	table.mu.Lock()
	defer table.mu.Unlock()
	if err := errors.Join(table.errs...); err != nil {
		return nil, err
	}
	if err := applyDeclarations(table.services, table.declarations); err != nil {
		return nil, err
	}
	if err := applyPermDeclarations(table.services, table.permDeclarations); err != nil {
		return nil, err
	}
	services := make([]*registeredService, 0, len(table.services))
	for _, service := range table.services {
		services = append(services, service)
	}
	slices.SortFunc(services, func(a, b *registeredService) int {
		return strings.Compare(a.desc.ServiceName, b.desc.ServiceName)
	})
	return services, nil
}
