package validatorkit

import (
	"errors"
	"fmt"
	"reflect"
	"strings"

	"github.com/go-playground/locales"
	"github.com/go-playground/locales/en"
	"github.com/go-playground/locales/zh"
	ut "github.com/go-playground/universal-translator"
	"github.com/go-playground/validator/v10"
	entrans "github.com/go-playground/validator/v10/translations/en"
	zhtrans "github.com/go-playground/validator/v10/translations/zh"
)

// Languages Setup can install. The spelling is the translator language tag
// returned by the universal translator.
const (
	LanguageEnglish = "en"
	LanguageChinese = "zh"
)

// Setup registers language's bundled default translations and the json, form,
// uri, header tag-name resolution on engine, and returns the translator the
// registered messages are recorded in. Callers use it with
// validator.FieldError.Translate.
//
// Setup is not idempotent by contract: registering the translations twice
// returns the translations' own duplicate-registration error, and registering
// the tag-name function twice replaces it, as the underlying library does.
// Configure each engine once, and pass the same engine to the binder that
// produces the errors being translated.
func Setup(engine *validator.Validate, language string) (ut.Translator, error) {
	if engine == nil {
		return nil, errors.New("validatorkit: validator engine is nil")
	}
	translator, register, err := translatorFor(language)
	if err != nil {
		return nil, err
	}
	if err := register(engine, translator); err != nil {
		return nil, fmt.Errorf("validatorkit: register %q translations: %w", language, err)
	}
	engine.RegisterTagNameFunc(TagName)
	return translator, nil
}

// TagName resolves the public name of field: the first non-empty json, form,
// uri, or header tag without its option suffix. A "-" tag is ignored and the
// next tag is tried, and a field with none of those tags keeps its Go name.
func TagName(field reflect.StructField) string {
	for _, key := range [...]string{"json", "form", "uri", "header"} {
		name := strings.SplitN(field.Tag.Get(key), ",", 2)[0]
		if name != "" && name != "-" {
			return name
		}
	}
	return field.Name
}

// translatorFor builds the translator for language and returns the matching
// default-translation registration function. English is always the fallback,
// so a locale miss degrades to an understandable message instead of the raw
// rule name.
func translatorFor(language string) (ut.Translator, func(*validator.Validate, ut.Translator) error, error) {
	english := en.New()
	var (
		target   locales.Translator
		register func(*validator.Validate, ut.Translator) error
	)
	switch language {
	case LanguageEnglish:
		target = english
		register = entrans.RegisterDefaultTranslations
	case LanguageChinese:
		target = zh.New()
		register = zhtrans.RegisterDefaultTranslations
	default:
		return nil, nil, fmt.Errorf(
			"validatorkit: unsupported language %q; supported languages are %q and %q",
			language, LanguageEnglish, LanguageChinese)
	}

	universal := ut.New(english, english, target)
	translator, found := universal.GetTranslator(language)
	if !found {
		return nil, nil, fmt.Errorf("validatorkit: no translator for language %q", language)
	}
	return translator, register, nil
}
