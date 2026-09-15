package gin

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/plugin/assembly"
	"github.com/xbcio/xbc/transport/web"
)

// probeKey identifies a minimal test-only consumer Definition that mirrors
// transport/web/plugin.go's own engineInput: a plugin.RequireOne[web.EngineFactory]()
// input, read via Inputs.Get during construction. This is the mechanism a
// broken Exports declaration on Bundle() actually breaks. A bare type
// assertion against a concretely-typed Primary() would not catch that
// breakage, because the concrete Factory type already implements
// web.EngineFactory at the Go level, independent of whatever the Definition
// declares as its plugin-assembly contract.
const probeKey plugin.Key = "engines-gin-test-requireone-probe"

// probe captures whatever web.EngineFactory the assembly graph resolved for
// engineProbeInput, exactly as web.Server's own factory captures engineInput
// in transport/web/plugin.go.
type probe struct {
	factory web.EngineFactory
}

var engineProbeInput = plugin.RequireOne[web.EngineFactory]()

var probeDefinition = plugin.Define(
	probeKey,
	func(ctx plugin.BuildContext) (*probe, error) {
		return &probe{factory: engineProbeInput.Get(ctx).Value}, nil
	},
	plugin.Options[*probe]{
		Inputs: plugin.Inputs(engineProbeInput),
	},
)

// TestBundleSatisfiesRequireOneEngineFactoryConsumer proves Bundle() resolves
// as the exporter for a plugin.RequireOne[web.EngineFactory]() input -- the
// exact consumption shape transport/web/plugin.go's engineInput uses. This is
// the assembly-layer contract Bundle()'s Exports declaration exists to
// satisfy: if that declaration is ever lost, planning fails with "requires
// exactly one web.EngineFactory exporter, found none" before this test's
// probe consumer can even be constructed.
func TestBundleSatisfiesRequireOneEngineFactoryConsumer(t *testing.T) {
	built, err := assembly.BuildPlan(assembly.PlanOptions{
		Bundles: []plugin.Bundle{Bundle(), plugin.BundleOf(probeDefinition)},
	})
	require.NoError(t, err, "BuildPlan() 应能规划 gin 引擎 Bundle 与探针消费者")

	constructed, err := assembly.Construct(built, assembly.ConstructOptions{})
	require.NoError(t, err, "Construct() 应能构造 gin 引擎与探针消费者")

	instance, found := constructed.Instance(plugin.Identity{Plugin: probeKey, Instance: plugin.DefaultInstance})
	require.True(t, found, "未能构造出探针消费者实例")

	consumer, ok := instance.Primary().(*probe)
	require.True(t, ok, "primary = %T，探针消费者类型不符", instance.Primary())
	require.NotNil(t, consumer.factory, "RequireOne[web.EngineFactory]() 未解析到 gin 引擎导出的契约")

	engine, err := consumer.factory.NewEngine(web.Options{})
	require.NoError(t, err, "NewEngine() 不应返回错误")
	require.NotNil(t, engine, "NewEngine() 不应返回 nil web.Engine")
}
