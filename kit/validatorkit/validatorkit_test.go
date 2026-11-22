package validatorkit

import (
	"reflect"
	"testing"

	"github.com/go-playground/validator/v10"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type translatedSample struct {
	Name string `json:"name" form:"name" validate:"required"`
	Age  int    `json:"age" validate:"gte=18"`
}

func TestSetupChinese(t *testing.T) {
	engine := validator.New()
	trans, err := Setup(engine, LanguageChinese)
	require.NoError(t, err)
	require.NotNil(t, trans, "Setup must return the translator it registered messages in")

	err = engine.Struct(translatedSample{})
	require.Error(t, err)
	validationErrors, ok := err.(validator.ValidationErrors)
	require.True(t, ok)

	messages := make([]string, 0, len(validationErrors))
	for _, fieldError := range validationErrors {
		messages = append(messages, fieldError.Translate(trans))
	}
	assert.Equal(t, []string{"name为必填字段", "age必须大于或等于18"}, messages)
}

func TestSetupEnglish(t *testing.T) {
	engine := validator.New()
	trans, err := Setup(engine, LanguageEnglish)
	require.NoError(t, err)
	require.NotNil(t, trans)

	err = engine.Struct(translatedSample{})
	require.Error(t, err)
	validationErrors, ok := err.(validator.ValidationErrors)
	require.True(t, ok)
	require.Len(t, validationErrors, 2)
	assert.Equal(t, "name is a required field", validationErrors[0].Translate(trans))
	assert.Equal(t, "age must be 18 or greater", validationErrors[1].Translate(trans))
}

func TestSetupRejectsUnsupportedLanguage(t *testing.T) {
	_, err := Setup(validator.New(), "fr")
	require.Error(t, err)
	assert.Contains(t, err.Error(), `"fr"`)
	assert.Contains(t, err.Error(), `"en"`)
	assert.Contains(t, err.Error(), `"zh"`)
}

func TestSetupRejectsNilEngine(t *testing.T) {
	trans, err := Setup(nil, LanguageChinese)
	require.Error(t, err)
	assert.Nil(t, trans)
	assert.Contains(t, err.Error(), "engine is nil")
}

func TestSetupRegistersTagNameResolution(t *testing.T) {
	engine := validator.New()
	_, err := Setup(engine, LanguageChinese)
	require.NoError(t, err)

	type tagged struct {
		JSON        string `json:"json_name,omitempty" validate:"required"`
		Form        string `form:"form_name" validate:"required"`
		URI         string `uri:"uri_name" validate:"required"`
		Header      string `header:"header_name" validate:"required"`
		JSONWins    string `json:"json_name2" form:"form_name2" validate:"required"`
		SkipJSON    string `json:"-" validate:"required"`
		SkipToForm  string `json:"-" form:"after_skip" validate:"required"`
		NoPublicTag string `validate:"required"`
	}

	err = engine.Struct(tagged{})
	require.Error(t, err)
	validationErrors, ok := err.(validator.ValidationErrors)
	require.True(t, ok)

	want := map[string]bool{
		"json_name":   true,
		"form_name":   true,
		"uri_name":    true,
		"header_name": true,
		"json_name2":  true,
		"SkipJSON":    true,
		"after_skip":  true,
		"NoPublicTag": true,
	}
	got := make(map[string]bool, len(validationErrors))
	for _, fieldError := range validationErrors {
		got[fieldError.Field()] = true
	}
	assert.Equal(t, want, got, "each field must resolve through its first non-empty json, form, uri, or header tag")
}

func TestTagName(t *testing.T) {
	tests := []struct {
		name string
		tag  string
		want string
	}{
		{name: "json tag", tag: `json:"value,omitempty"`, want: "value"},
		{name: "form fallback", tag: `form:"value_form"`, want: "value_form"},
		{name: "uri fallback", tag: `uri:"value_uri"`, want: "value_uri"},
		{name: "header fallback", tag: `header:"X-Value"`, want: "X-Value"},
		{name: "json wins over form", tag: `json:"value_json" form:"value_form"`, want: "value_json"},
		{name: "dash skips to form", tag: `json:"-" form:"value_form"`, want: "value_form"},
		{name: "dash only keeps Go name", tag: `json:"-"`, want: "Value"},
		{name: "no tag keeps Go name", tag: ``, want: "Value"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert.Equal(t, test.want, TagName(structFieldWithTag(test.tag)))
		})
	}
}

// structFieldWithTag builds one exported string field carrying tag through
// reflect.StructOf, so TagName is exercised on the exact StructField shape the
// validator sees.
func structFieldWithTag(tag string) reflect.StructField {
	typ := reflect.StructOf([]reflect.StructField{{
		Name: "Value",
		Type: reflect.TypeOf(""),
		Tag:  reflect.StructTag(tag),
	}})
	return typ.Field(0)
}
