package jsonutil

// AsObj returns v as an object, or nil when v holds another JSON type.
func AsObj(v interface{}) map[string]interface{} {
	m, _ := v.(map[string]interface{})
	return m
}

// AsArr returns v as an array, or nil when v holds another JSON type.
func AsArr(v interface{}) []interface{} {
	a, _ := v.([]interface{})
	return a
}

// AsStr returns v as a string, or an empty string when v holds another JSON type.
func AsStr(v interface{}) string {
	s, _ := v.(string)
	return s
}

// ObjStr returns m[k] as a string, or an empty string when the value is absent or not a string.
func ObjStr(m map[string]interface{}, k string) string { return AsStr(m[k]) }
