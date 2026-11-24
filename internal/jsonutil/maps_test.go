package jsonutil

import "testing"

func TestMapAccessors(t *testing.T) {
	obj := map[string]interface{}{"text": "hello", "count": float64(2)}
	if got := AsObj(obj); got == nil || got["text"] != "hello" {
		t.Fatalf("AsObj(map) = %#v", got)
	}
	if got := AsObj("not a map"); got != nil {
		t.Fatalf("AsObj(non-map) = %#v, want nil", got)
	}

	arr := []interface{}{"a", "b"}
	if got := AsArr(arr); len(got) != 2 {
		t.Fatalf("AsArr(slice) = %#v", got)
	}
	if got := AsArr(obj); got != nil {
		t.Fatalf("AsArr(non-slice) = %#v, want nil", got)
	}

	if got := AsStr("value"); got != "value" {
		t.Fatalf("AsStr(string) = %q", got)
	}
	if got := AsStr(42); got != "" {
		t.Fatalf("AsStr(non-string) = %q, want empty", got)
	}

	if got := ObjStr(obj, "text"); got != "hello" {
		t.Fatalf("ObjStr(text) = %q", got)
	}
	if got := ObjStr(obj, "count"); got != "" {
		t.Fatalf("ObjStr(non-string) = %q, want empty", got)
	}
	if got := ObjStr(obj, "missing"); got != "" {
		t.Fatalf("ObjStr(missing) = %q, want empty", got)
	}
}
