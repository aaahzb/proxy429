package jsonutil

import (
	"encoding/json"
	"testing"
)

// TestSetTopLevelJSONValue locks text-level top-level key editing: set/delete/append touch only the target key;
// every other field's content, order, and formatting (indentation, newline style) is preserved byte for byte; the input is not modified in place.
func TestSetTopLevelJSONValue(t *testing.T) {
	cases := []struct {
		name string
		src  string
		key  string
		raw  []byte
		want string
	}{
		{"改值-多行", "{\n  \"a\": 1,\n  \"ui_lang\": \"zh\",\n  \"b\": 2\n}\n", "ui_lang", []byte(`"en"`), "{\n  \"a\": 1,\n  \"ui_lang\": \"en\",\n  \"b\": 2\n}\n"},
		{"改值-紧凑", `{"a":1,"ui_lang":"zh"}`, "ui_lang", []byte(`"en"`), `{"a":1,"ui_lang":"en"}`},
		{"删除-中间键", "{\n  \"a\": 1,\n  \"ui_lang\": \"zh\",\n  \"b\": 2\n}\n", "ui_lang", nil, "{\n  \"a\": 1,\n  \"b\": 2\n}\n"},
		{"删除-首键", "{\n  \"ui_lang\": \"zh\",\n  \"a\": 1\n}\n", "ui_lang", nil, "{\n  \"a\": 1\n}\n"},
		{"删除-尾键紧凑", `{"a":1,"ui_lang":"zh"}`, "ui_lang", nil, `{"a":1}`},
		{"删除-唯一键", `{"ui_lang":"zh"}`, "ui_lang", nil, `{}`},
		{"删除-不存在的键", `{"a":1}`, "ui_lang", nil, `{"a":1}`},
		{"追加-多行", "{\n  \"a\": 1\n}\n", "ui_lang", []byte(`"en"`), "{\n  \"a\": 1,\n  \"ui_lang\": \"en\"\n}\n"},
		{"追加-空对象", `{}`, "ui_lang", []byte(`"en"`), "{\n  \"ui_lang\": \"en\"\n}"},
		{"追加-CRLF", "{\r\n  \"a\": 1\r\n}\r\n", "ui_lang", []byte(`"en"`), "{\r\n  \"a\": 1,\r\n  \"ui_lang\": \"en\"\r\n}\r\n"},
		{"值可以是对象", `{"a":1,"thinking":{"type":"enabled"}}`, "thinking", []byte(`{"type":"disabled"}`), `{"a":1,"thinking":{"type":"disabled"}}`},
		{"不误伤嵌套同名键", `{"ui_lang":"zh","nested":{"ui_lang":"fr"}}`, "ui_lang", []byte(`"en"`), `{"ui_lang":"en","nested":{"ui_lang":"fr"}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := []byte(tc.src)
			before := string(src)
			got, ok := SetTopLevelJSONValue(src, tc.key, tc.raw)
			if !ok {
				t.Fatalf("ok=false")
			}
			if string(got) != tc.want {
				t.Errorf("got  %q\nwant %q", got, tc.want)
			}
			var probe any
			if err := json.Unmarshal(got, &probe); err != nil {
				t.Errorf("输出不是合法 JSON: %v", err)
			}
			if string(src) != before {
				t.Errorf("入参被原地修改")
			}
		})
	}
	if _, ok := SetTopLevelJSONValue([]byte(`[1,2]`), "ui_lang", []byte(`"en"`)); ok {
		t.Errorf("顶层数组应 ok=false")
	}
}
