package gin

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/plugin/assembly"
	"github.com/xbcio/xbc/transport/web"
)

// TestBundleAssemblesAndSatisfiesEngineFactoryContract proves Bundle() is
// itself plannable and constructible, and that the instance it produces
// actually satisfies web.EngineFactory -- not just that construction
// succeeds. This closes a real coverage gap: transport/web's own tests never
// exercise this Definition (they use an in-package test double), so a broken
// primary/contract declaration here previously went undetected until a real
// composition root (examples) assembled it.
func TestBundleAssemblesAndSatisfiesEngineFactoryContract(t *testing.T) {
	built, err := assembly.BuildPlan(assembly.PlanOptions{
		Bundles: []plugin.Bundle{Bundle()},
	})
	require.NoError(t, err, "BuildPlan() 应能规划 gin 引擎 Bundle")

	constructed, err := assembly.Construct(built, assembly.ConstructOptions{})
	require.NoError(t, err, "Construct() 应能构造 gin 引擎实例")

	instance, found := constructed.Instance(plugin.Identity{Plugin: Key, Instance: plugin.DefaultInstance})
	require.True(t, found, "未能构造出 gin 引擎实例")

	factory, ok := instance.Primary().(web.EngineFactory)
	require.True(t, ok, "primary = %T，未满足 web.EngineFactory 契约", instance.Primary())

	engine, err := factory.NewEngine(web.Options{})
	require.NoError(t, err, "NewEngine() 不应返回错误")
	require.NotNil(t, engine, "NewEngine() 不应返回 nil web.Engine")
}
