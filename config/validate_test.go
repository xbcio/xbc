package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
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
	require.Contains(t, err.Error(), "required field missing")
}

func TestValidateHostnamePortMessageIncludesActualValue(t *testing.T) {
	cfg := redisValidateConfig{Addr: "127.0.0.1"}
	err := Validate(&cfg, "plugins.redis.default")
	require.Error(t, err)
	require.Contains(t, err.Error(), `not a valid host:port — got "127.0.0.1"`)
}

func TestValidateUnknownTagFallsBackToGenericMessage(t *testing.T) {
	cfg := fallbackValidateConfig{Ratio: 5}
	err := Validate(&cfg, "plugins.sampler")
	require.Error(t, err)
	require.Contains(t, err.Error(), "failed validation rule")
}

func TestValidateOneofMessage(t *testing.T) {
	cfg := oneofValidateConfig{Mode: "weekly"}
	err := Validate(&cfg, "log.file")
	require.Error(t, err)
	require.Contains(t, err.Error(), "must be")
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
	want := "xbc: configuration error\n" +
		"  plugins.gorm.readonly.dsn   required field missing\n" +
		"  plugins.redis.default.addr  not a valid host:port — got \"127.0.0.1\""
	require.Equal(t, want, e1.Error())
}

type collectionValidationItem struct {
	Name string `yaml:"name" validate:"required"`
}

type collectionValidationConfig struct {
	Items []collectionValidationItem          `yaml:"items" validate:"dive"`
	Named map[string]collectionValidationItem `yaml:"named" validate:"dive"`
}

func TestValidatePreservesCollectionIndexesInYAMLPaths(t *testing.T) {
	config := collectionValidationConfig{
		Items: []collectionValidationItem{{}},
		Named: map[string]collectionValidationItem{"primary": {}},
	}
	err := Validate(&config, "service")
	require.Error(t, err)
	require.Contains(t, err.Error(), "service.items[0].name")
	require.Contains(t, err.Error(), "service.named[primary].name")
}

type unknownValidationTagConfig struct {
	Name string `yaml:"name" validate:"definitely_not_registered"`
}

type malformedValidationTagConfig struct {
	Name string `yaml:"name" validate:"required_if=Mode"`
}

func TestValidateConvertsInvalidTagPanicsToContextualErrors(t *testing.T) {
	tests := []struct {
		name       string
		config     any
		schemaName string
		panicText  string
	}{
		{
			name:       "unknown validation",
			config:     &unknownValidationTagConfig{},
			schemaName: "unknownValidationTagConfig",
			panicText:  "Undefined validation function",
		},
		{
			name:       "malformed parameters",
			config:     &malformedValidationTagConfig{},
			schemaName: "malformedValidationTagConfig",
			panicText:  "Bad param number",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var err error
			require.NotPanics(t, func() {
				err = Validate(test.config, "plugins.example.default")
			})
			require.Error(t, err)
			assert.Contains(t, err.Error(), "configuration schema")
			assert.Contains(t, err.Error(), test.schemaName)
			assert.Contains(t, err.Error(), "plugins.example.default")
			assert.Contains(t, err.Error(), "invalid validate tag")
			assert.Contains(t, err.Error(), test.panicText)
		})
	}
}
