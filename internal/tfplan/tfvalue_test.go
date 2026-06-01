package tfplan

import (
	"encoding/json"
	"testing"
)

func TestTFValueUnmarshalJSON(t *testing.T) {
	cases := []struct {
		name string
		json string
		want TFValue
	}{
		{"null", `null`, TFValue{}},
		{"string", `"hello"`, TFStr("hello")},
		{"number", `42.5`, TFNum(42.5)},
		{"bool true", `true`, TFBoolVal(true)},
		{"bool false", `false`, TFBoolVal(false)},
		{"list", `["a","b"]`, TFListVal([]TFValue{TFStr("a"), TFStr("b")})},
		{"object", `{"k":"v"}`, TFObjectVal(TFState{"k": TFStr("v")})},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var got TFValue
			if err := json.Unmarshal([]byte(c.json), &got); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if !got.Equal(c.want) {
				t.Errorf("got %+v, want %+v", got, c.want)
			}
		})
	}
}

func TestTFValueEqual(t *testing.T) {
	if !TFStr("x").Equal(TFStr("x")) {
		t.Error("same strings not equal")
	}
	if TFStr("x").Equal(TFStr("y")) {
		t.Error("different strings should not be equal")
	}
	if TFStr("x").Equal(TFNum(1)) {
		t.Error("different kinds should not be equal")
	}
	nested := TFObjectVal(TFState{"a": TFListVal([]TFValue{TFNum(1), TFNum(2)})})
	if !nested.Equal(nested) {
		t.Error("deep equal should hold for same value")
	}
	if nested.Equal(TFObjectVal(TFState{"a": TFListVal([]TFValue{TFNum(1), TFNum(3)})})) {
		t.Error("different nested values should not be equal")
	}
}

func TestTFValueGoValueRoundtrip(t *testing.T) {
	v := TFObjectVal(TFState{
		"name": TFStr("web"),
		"port": TFNum(443),
		"tags": TFListVal([]TFValue{TFStr("a"), TFStr("b")}),
	})
	gv := v.GoValue()
	back := FromGoValue(gv)
	if !back.Equal(v) {
		t.Errorf("roundtrip via GoValue failed:\noriginal: %+v\ngot:      %+v", v, back)
	}
}

func TestFromGoValue(t *testing.T) {
	cases := []struct {
		name string
		in   interface{}
		want TFValue
	}{
		{"nil", nil, TFValue{}},
		{"string", "hello", TFStr("hello")},
		{"float64", float64(3), TFNum(3)},
		{"bool", true, TFBoolVal(true)},
		{"slice", []interface{}{"a"}, TFListVal([]TFValue{TFStr("a")})},
		{"map", map[string]interface{}{"k": "v"}, TFObjectVal(TFState{"k": TFStr("v")})},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := FromGoValue(c.in)
			if !got.Equal(c.want) {
				t.Errorf("got %+v, want %+v", got, c.want)
			}
		})
	}
}

func TestTFStateFrom(t *testing.T) {
	m := map[string]interface{}{
		"size": float64(20),
		"type": "gp2",
	}
	state := TFStateFrom(m)
	if n, ok := state["size"].AsNumber(); !ok || n != 20 {
		t.Errorf("size: got %v", state["size"])
	}
	if s, ok := state["type"].AsString(); !ok || s != "gp2" {
		t.Errorf("type: got %v", state["type"])
	}
}
