package xbc

import "github.com/gin-gonic/gin"

// RouteInfo is one entry in the frozen route table. Plan 2 freezes method and
// path only; Plan 3 adds the metadata fields (Public, Name, Doc).
type RouteInfo struct {
	Method string
	Path   string
}

// Router is the chainable route builder plugins receive in RegisterRoutes.
// Task 14 (stage_run.go) adds the Group/Handle/GET/... methods and the
// freeze-after-stage-7 behavior; this task only needs the shape to exist so
// RouteProvider compiles.
type Router struct {
	engine   *gin.Engine
	group    *gin.RouterGroup
	basePath string
	routes   *[]RouteInfo
	frozen   *bool
}
