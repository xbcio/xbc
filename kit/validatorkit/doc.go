// Package validatorkit installs localized go-playground/validator
// translations and a stable public field-name convention on a caller-owned
// validator engine.
//
// The package is framework-free: it imports only the go-playground validator
// libraries, keeps no package-level state, and never selects an engine itself.
// The caller owns the engine and the returned translator, so two components
// that validate different structs can configure independent engines without
// sharing a global translator:
//
//	engine := validator.New()
//	trans, err := validatorkit.Setup(engine, validatorkit.LanguageChinese)
//	if err != nil {
//		return err
//	}
//	if err := engine.Struct(input); err != nil {
//		if validationErrors, ok := err.(validator.ValidationErrors); ok {
//			for _, fieldError := range validationErrors {
//				log.Print(fieldError.Translate(trans))
//			}
//		}
//	}
//
// Setup is also the adapter seam for a transport that binds request bodies
// through its own engine: a Gin host passes
// binding.Validator.Engine().(*validator.Validate), for example, without this
// package importing Gin.
//
// # Field names
//
// Setup registers one tag-name function. A field's public name is the first
// non-empty json, form, uri, or header tag, with any option suffix after a
// comma removed; a "-" value is ignored and the next tag is tried. A field
// that carries none of those tags keeps its Go name. This is the same
// resolution the Web transport uses for binding errors, so a translated
// message and a Problem Details field name agree.
package validatorkit
