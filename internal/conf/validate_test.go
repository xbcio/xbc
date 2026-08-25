// internal/conf/validate_test.go
package conf

import (
	"testing"

	"github.com/stretchr/testify/require"
)

type gormValidateConfig struct {
	DSN string `yaml:"dsn" validate:"required"`
}

type redisValidateConfig struct {
	Addr string `yaml:"addr" validate:"required,hostname_port"`
}

type poolLeaf struct {
	MaxOpenConn int `yaml:"max_open_conn" validate:"min=1,max=100"`
}

type nestedValidateConfig struct {
	Pool poolLeaf `yaml:"pool"`
}

type oneofValidateConfig struct {
	Mode string `yaml:"mode" validate:"oneof=daily size"`
}

type fallbackValidateConfig struct {
	Ratio float64 `yaml:"ratio" validate:"gt=0,lt=1"`
}

type multiFieldConfig struct {
	DSN  string `yaml:"dsn" validate:"required"`
	Addr string `yaml:"addr" validate:"required,hostname_port"`
}

func TestValidateRequiredMessage(t *testing.T) {
	cfg := gormValidateConfig{}
	err := Validate(&cfg, "plugins.gorm.readonly")
	require.Error(t, err)
	require.Contains(t, err.Error(), "plugins.gorm.readonly.dsn")
	require.Contains(t, err.Error(), "必填项缺失")
}

func TestValidateHostnamePortMessageIncludesActualValue(t *testing.T) {
	cfg := redisValidateConfig{Addr: "127.0.0.1"}
	err := Validate(&cfg, "plugins.redis.default")
	require.Error(t, err)
	require.Contains(t, err.Error(), `不是合法的 host:port —— 得到 "127.0.0.1"`)
}

func TestValidateUnknownTagFallsBackToGenericMessage(t *testing.T) {
	cfg := fallbackValidateConfig{Ratio: 5}
	err := Validate(&cfg, "plugins.sampler")
	require.Error(t, err)
	require.Contains(t, err.Error(), "未通过校验规则")
}

func TestValidateOneofMessage(t *testing.T) {
	cfg := oneofValidateConfig{Mode: "weekly"}
	err := Validate(&cfg, "log.file")
	require.Error(t, err)
	require.Contains(t, err.Error(), "必须是")
	require.Contains(t, err.Error(), `"weekly"`)
}

func TestValidateNestedStructPath(t *testing.T) {
	cfg := nestedValidateConfig{Pool: poolLeaf{MaxOpenConn: 0}}
	err := Validate(&cfg, "plugins.gorm.default")
	require.Error(t, err)
	require.Contains(t, err.Error(), "plugins.gorm.default.pool.max_open_conn")
}

func TestValidateMultipleViolationsOrderedAndAligned(t *testing.T) {
	cfg := multiFieldConfig{}
	err := Validate(&cfg, "plugins.gorm.readonly")
	require.Error(t, err)
	var ve *ValidationError
	require.ErrorAs(t, err, &ve)
	require.Len(t, ve.Lines, 2)
	require.Contains(t, err.Error(), "plugins.gorm.readonly.dsn")
	require.Contains(t, err.Error(), "plugins.gorm.readonly.addr")
}

func TestValidationErrorAppendMergesAndRealigns(t *testing.T) {
	cfg1 := gormValidateConfig{}
	err1 := Validate(&cfg1, "plugins.gorm.readonly")
	var e1 *ValidationError
	require.ErrorAs(t, err1, &e1)

	cfg2 := redisValidateConfig{Addr: "127.0.0.1"}
	err2 := Validate(&cfg2, "plugins.redis.default")
	var e2 *ValidationError
	require.ErrorAs(t, err2, &e2)

	e1.Append(e2)
	require.Len(t, e1.Lines, 2)
	require.Contains(t, e1.Error(), "plugins.gorm.readonly.dsn")
	require.Contains(t, e1.Error(), "plugins.redis.default.addr")
}

func TestValidatePassesWithNoViolations(t *testing.T) {
	cfg := gormValidateConfig{DSN: "user:pass@/db"}
	err := Validate(&cfg, "plugins.gorm.default")
	require.NoError(t, err)
}
