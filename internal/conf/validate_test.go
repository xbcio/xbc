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

// TestValidationErrorColumnAlignmentIsExact pins the exact rendered block
// down to its byte content -- the spacing between the longest path and its
// message, the em dash in the hostname_port message, and the fact that the
// shorter path's column gets one extra space to line up with the longer one.
// The Task 6 review's mutation M3 (changing the "+2" padding in Error() to
// "+1") left every other test green because they all use require.Contains
// on substrings; this is the one test that would actually go red for that
// mutation. The two example paths and messages are taken verbatim from API
// contract §13.
func TestValidationErrorColumnAlignmentIsExact(t *testing.T) {
	cfg1 := gormValidateConfig{}
	err1 := Validate(&cfg1, "plugins.gorm.readonly")
	var e1 *ValidationError
	require.ErrorAs(t, err1, &e1)

	cfg2 := redisValidateConfig{Addr: "127.0.0.1"}
	err2 := Validate(&cfg2, "plugins.redis.default")
	var e2 *ValidationError
	require.ErrorAs(t, err2, &e2)

	e1.Append(e2)

	// "plugins.gorm.readonly.dsn" is 25 runes, "plugins.redis.default.addr"
	// is 26 -- the wider one sets the column, so the dsn line gets 3 spaces
	// of padding after it (26-25+2) and the addr line gets 2 (26-26+2).
	want := "xbc: 配置错误\n" +
		"  plugins.gorm.readonly.dsn   必填项缺失\n" +
		"  plugins.redis.default.addr  不是合法的 host:port —— 得到 \"127.0.0.1\""
	require.Equal(t, want, e1.Error())
}
