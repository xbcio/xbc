package ginmount

import (
	"fmt"

	"github.com/gin-gonic/gin"
)

// Controller is the conventional Gin route contributor: Setup registers the
// controller's routes on the group it is given. The interface is declared here
// so a host can keep every existing Setup method unchanged -- existing
// controllers satisfy this interface structurally.
type Controller interface {
	Setup(*gin.RouterGroup)
}

// ControllerRegistrar adapts a controller list to Registrar. It is the
// counterpart of the conventional Gin route-group setup: the same controllers,
// the same group-level handlers, and the same Setup order, but owned by the
// host and mounted by the route table instead of registered during
// package-level setup.
//
// MountPath is the prefix the controllers were grouped under. Under the mount,
// controllers receive a group relative to that prefix, so their Setup methods
// keep declaring the same relative routes they always did.
type ControllerRegistrar struct {
	mountPath   string
	handlers    []gin.HandlerFunc
	controllers []Controller
}

// NewControllerRegistrar builds a Registrar for controllers. It panics on a
// nil controller: a nil entry is a wiring mistake, and skipping it would leave
// its routes served by the framework's 404 with no error at boot.
func NewControllerRegistrar(mountPath string, handlers []gin.HandlerFunc, controllers ...Controller) *ControllerRegistrar {
	for index, controller := range controllers {
		if controller == nil {
			panic(fmt.Sprintf("ginmount: controller %d is nil", index))
		}
	}
	return &ControllerRegistrar{
		mountPath:   mountPath,
		handlers:    handlers,
		controllers: controllers,
	}
}

// MountPath returns the prefix passed to the constructor.
func (r *ControllerRegistrar) MountPath() string { return r.mountPath }

// RegisterGin installs the group-level handlers and calls every controller's
// Setup in the order it was given.
func (r *ControllerRegistrar) RegisterGin(engine *gin.Engine) {
	group := engine.Group("", r.handlers...)
	for _, controller := range r.controllers {
		controller.Setup(group)
	}
}
