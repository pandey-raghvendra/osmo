package tfplan

import (
	"encoding/json"
	"fmt"
)

// TFKind identifies the JSON type of a Terraform attribute value.
type TFKind byte

const (
	TFNull   TFKind = iota // JSON null, or absent key (zero value)
	TFString               // JSON string
	TFNumber               // JSON number (float64)
	TFBool                 // JSON bool
	TFList                 // JSON array — nested blocks or tuples
	TFObject               // JSON object — nested map or object type
)

// TFValue is a typed representation of one attribute value from a Terraform
// plan's before/after state. It replaces map[string]interface{} and eliminates
// runtime type-assertion panics across the absorb engine.
type TFValue struct {
	kind TFKind
	s    string
	n    float64
	b    bool
	list []TFValue
	obj  map[string]TFValue
}

// TFState is a typed map of attribute name → value for one resource snapshot.
// It is a type alias (not a defined type) so map literals work without casting.
type TFState = map[string]TFValue

// ---- Constructors ----------------------------------------------------------

// TFStr returns a string-kind TFValue.
func TFStr(s string) TFValue { return TFValue{kind: TFString, s: s} }

// TFNum returns a number-kind TFValue.
func TFNum(n float64) TFValue { return TFValue{kind: TFNumber, n: n} }

// TFBoolVal returns a bool-kind TFValue.
func TFBoolVal(b bool) TFValue { return TFValue{kind: TFBool, b: b} }

// TFListVal returns a list-kind TFValue.
func TFListVal(list []TFValue) TFValue { return TFValue{kind: TFList, list: list} }

// TFObjectVal returns an object-kind TFValue.
func TFObjectVal(obj map[string]TFValue) TFValue { return TFValue{kind: TFObject, obj: obj} }

// Zero value of TFValue is TFNull — no constructor needed.

// ---- Accessors -------------------------------------------------------------

func (v TFValue) Kind() TFKind { return v.kind }
func (v TFValue) IsNull() bool { return v.kind == TFNull }
func (v TFValue) IsList() bool { return v.kind == TFList }

func (v TFValue) AsString() (string, bool) {
	return v.s, v.kind == TFString
}
func (v TFValue) AsNumber() (float64, bool) {
	return v.n, v.kind == TFNumber
}
func (v TFValue) AsBool() (bool, bool) {
	return v.b, v.kind == TFBool
}
func (v TFValue) AsList() ([]TFValue, bool) {
	if v.kind != TFList {
		return nil, false
	}
	return v.list, true
}
func (v TFValue) AsObject() (map[string]TFValue, bool) {
	if v.kind != TFObject {
		return nil, false
	}
	return v.obj, true
}

// ---- Equality --------------------------------------------------------------

// Equal reports deep structural equality between two TFValues.
func (v TFValue) Equal(other TFValue) bool {
	if v.kind != other.kind {
		return false
	}
	switch v.kind {
	case TFNull:
		return true
	case TFString:
		return v.s == other.s
	case TFNumber:
		return v.n == other.n
	case TFBool:
		return v.b == other.b
	case TFList:
		if len(v.list) != len(other.list) {
			return false
		}
		for i := range v.list {
			if !v.list[i].Equal(other.list[i]) {
				return false
			}
		}
		return true
	case TFObject:
		if len(v.obj) != len(other.obj) {
			return false
		}
		for k, vv := range v.obj {
			ov, ok := other.obj[k]
			if !ok || !vv.Equal(ov) {
				return false
			}
		}
		return true
	}
	return false
}

// ---- Bridge to untyped Go values -------------------------------------------

// GoValue converts TFValue to the equivalent JSON-decoded Go value
// (string, float64, bool, nil, []interface{}, map[string]interface{}).
// Used as a bridge to HCL evaluation and cty conversion.
func (v TFValue) GoValue() interface{} {
	switch v.kind {
	case TFNull:
		return nil
	case TFString:
		return v.s
	case TFNumber:
		return v.n
	case TFBool:
		return v.b
	case TFList:
		out := make([]interface{}, len(v.list))
		for i, el := range v.list {
			out[i] = el.GoValue()
		}
		return out
	case TFObject:
		out := make(map[string]interface{}, len(v.obj))
		for k, vv := range v.obj {
			out[k] = vv.GoValue()
		}
		return out
	}
	return nil
}

// FromGoValue converts an untyped JSON-decoded value (nil, string, float64,
// bool, []interface{}, map[string]interface{}) to TFValue. Useful in tests and
// as a migration bridge.
func FromGoValue(v interface{}) TFValue {
	switch x := v.(type) {
	case nil:
		return TFValue{}
	case string:
		return TFStr(x)
	case float64:
		return TFNum(x)
	case bool:
		return TFBoolVal(x)
	case []interface{}:
		list := make([]TFValue, len(x))
		for i, el := range x {
			list[i] = FromGoValue(el)
		}
		return TFListVal(list)
	case map[string]interface{}:
		obj := make(map[string]TFValue, len(x))
		for k, vv := range x {
			obj[k] = FromGoValue(vv)
		}
		return TFObjectVal(obj)
	default:
		return TFValue{}
	}
}

// TFStateFrom converts a map[string]interface{} to TFState. Useful in tests.
func TFStateFrom(m map[string]interface{}) TFState {
	state := make(TFState, len(m))
	for k, v := range m {
		state[k] = FromGoValue(v)
	}
	return state
}

// ---- JSON marshaling -------------------------------------------------------

// UnmarshalJSON implements json.Unmarshaler.
func (v *TFValue) UnmarshalJSON(data []byte) error {
	if len(data) == 0 {
		return fmt.Errorf("tfvalue: empty JSON token")
	}
	switch data[0] {
	case 'n':
		v.kind = TFNull
		return nil
	case '"':
		v.kind = TFString
		return json.Unmarshal(data, &v.s)
	case 't', 'f':
		v.kind = TFBool
		return json.Unmarshal(data, &v.b)
	case '[':
		v.kind = TFList
		return json.Unmarshal(data, &v.list)
	case '{':
		v.kind = TFObject
		return json.Unmarshal(data, &v.obj)
	default:
		v.kind = TFNumber
		return json.Unmarshal(data, &v.n)
	}
}

// MarshalJSON implements json.Marshaler.
func (v TFValue) MarshalJSON() ([]byte, error) {
	return json.Marshal(v.GoValue())
}
