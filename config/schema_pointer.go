package config

import "reflect"

func prepareInlinePointers(root reflect.Value, rootType reflect.Type, input any) []reflect.Value {
	inputValue, ok := stringMapValue(reflect.ValueOf(input))
	if !ok {
		return nil
	}
	var temporary []reflect.Value
	prepareStructPointers(root, rootType, inputValue, &temporary)
	return temporary
}

func prepareStructPointers(root reflect.Value, rootType reflect.Type, input reflect.Value, temporary *[]reflect.Value) {
	for i := 0; i < rootType.NumField(); i++ {
		field := rootType.Field(i)
		if field.PkgPath != "" {
			continue
		}
		name, inline, skip := yamlField(field)
		fieldType := dereference(field.Type)
		if skip || !isStructSchema(fieldType) {
			continue
		}

		fieldValue := root.Field(i)
		childInput := input
		matched := true
		if inline {
			matched = mapContainsStructField(input, fieldType)
		} else {
			var found bool
			childInput, found = mapField(input, name)
			if !found {
				continue
			}
			if _, ok := stringMapValue(childInput); !ok {
				continue
			}
		}

		if fieldValue.Kind() == reflect.Pointer {
			if fieldValue.IsNil() {
				fieldValue.Set(reflect.New(fieldValue.Type().Elem()))
				if inline && !matched {
					*temporary = append(*temporary, fieldValue)
				}
			}
			fieldValue = fieldValue.Elem()
		}
		childInput, ok := stringMapValue(childInput)
		if !ok {
			continue
		}
		prepareStructPointers(fieldValue, fieldType, childInput, temporary)
	}
}

func stringMapValue(value reflect.Value) (reflect.Value, bool) {
	for value.IsValid() && (value.Kind() == reflect.Interface || value.Kind() == reflect.Pointer) {
		if value.IsNil() {
			return reflect.Value{}, false
		}
		value = value.Elem()
	}
	return value, value.IsValid() && value.Kind() == reflect.Map && value.Type().Key().Kind() == reflect.String
}

func mapField(input reflect.Value, name string) (reflect.Value, bool) {
	for _, key := range input.MapKeys() {
		if key.String() == name {
			return input.MapIndex(key), true
		}
	}
	return reflect.Value{}, false
}

func mapContainsStructField(input reflect.Value, structType reflect.Type) bool {
	for i := 0; i < structType.NumField(); i++ {
		field := structType.Field(i)
		if field.PkgPath != "" {
			continue
		}
		name, inline, skip := yamlField(field)
		if skip {
			continue
		}
		if inline {
			if mapContainsStructField(input, dereference(field.Type)) {
				return true
			}
			continue
		}
		for _, key := range input.MapKeys() {
			if key.String() == name {
				return true
			}
		}
	}
	return false
}
