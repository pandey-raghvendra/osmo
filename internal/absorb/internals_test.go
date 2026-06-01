package absorb

// Tests for unexported absorb helpers that have real correctness risk.
// Kept in a separate file to avoid cluttering absorb_test.go.

import (
	"errors"
	"strings"
	"testing"

	"github.com/pandey-raghvendra/osmo/internal/tfplan"
)

// ---- renderTFValue ---------------------------------------------------------

func TestRenderTFValue_String(t *testing.T) {
	got, err := renderTFValue(tfplan.TFStr("hello"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `"hello"` {
		t.Errorf("got %q", got)
	}
}

func TestRenderTFValue_Number_Integer(t *testing.T) {
	got, err := renderTFValue(tfplan.TFNum(42))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "42" {
		t.Errorf("got %q", got)
	}
}

func TestRenderTFValue_Number_Float(t *testing.T) {
	got, err := renderTFValue(tfplan.TFNum(3.14))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "3.14" {
		t.Errorf("got %q", got)
	}
}

func TestRenderTFValue_BoolTrue(t *testing.T) {
	got, err := renderTFValue(tfplan.TFBoolVal(true))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "true" {
		t.Errorf("got %q", got)
	}
}

func TestRenderTFValue_BoolFalse(t *testing.T) {
	got, err := renderTFValue(tfplan.TFBoolVal(false))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "false" {
		t.Errorf("got %q", got)
	}
}

func TestRenderTFValue_Null_Error(t *testing.T) {
	_, err := renderTFValue(tfplan.TFValue{})
	if err == nil {
		t.Fatal("want error for null TFValue")
	}
}

func TestRenderTFValue_List_Error(t *testing.T) {
	_, err := renderTFValue(tfplan.TFListVal(nil))
	if err == nil {
		t.Fatal("want error for list TFValue (not scalar)")
	}
}

// ---- hclEscapeString -------------------------------------------------------

func TestHclEscapeString_NoSpecial(t *testing.T) {
	got := hclEscapeString("hello world")
	if got != "hello world" {
		t.Errorf("got %q", got)
	}
}

func TestHclEscapeString_DoubleQuote(t *testing.T) {
	got := hclEscapeString(`say "hi"`)
	if !strings.Contains(got, `\"`) {
		t.Errorf("double quote not escaped: %q", got)
	}
}

func TestHclEscapeString_Backslash(t *testing.T) {
	got := hclEscapeString(`C:\path`)
	if !strings.Contains(got, `\\`) {
		t.Errorf("backslash not escaped: %q", got)
	}
}

func TestHclEscapeString_Newline(t *testing.T) {
	got := hclEscapeString("line1\nline2")
	if !strings.Contains(got, `\n`) {
		t.Errorf("newline not escaped: %q", got)
	}
}

func TestHclEscapeString_CarriageReturn(t *testing.T) {
	got := hclEscapeString("line1\rline2")
	if !strings.Contains(got, `\r`) {
		t.Errorf("carriage return not escaped: %q", got)
	}
}

func TestHclEscapeString_Tab(t *testing.T) {
	got := hclEscapeString("col1\tcol2")
	if !strings.Contains(got, `\t`) {
		t.Errorf("tab not escaped: %q", got)
	}
}

func TestHclEscapeString_Combined(t *testing.T) {
	// A Windows path in a tag value with a newline — realistic drift scenario.
	input := `C:\Program Files\app` + "\n" + `"quoted"`
	got := hclEscapeString(input)
	if !strings.Contains(got, `\\`) {
		t.Errorf("backslash missing: %q", got)
	}
	if !strings.Contains(got, `\n`) {
		t.Errorf("newline missing: %q", got)
	}
	if !strings.Contains(got, `\"`) {
		t.Errorf("double-quote missing: %q", got)
	}
	// Must not contain raw newline or raw backslash.
	if strings.Contains(got, "\n") {
		t.Errorf("raw newline survived: %q", got)
	}
}

// ---- isAmbiguousNestedMatch ------------------------------------------------

func TestIsAmbiguousNestedMatch_True(t *testing.T) {
	err := errors.New("ambiguous nested block: two blocks matched equally")
	if !isAmbiguousNestedMatch(err) {
		t.Error("expected true for ambiguous match error")
	}
}

func TestIsAmbiguousNestedMatch_False_OtherError(t *testing.T) {
	err := errors.New("block not found")
	if isAmbiguousNestedMatch(err) {
		t.Error("expected false for unrelated error")
	}
}

func TestIsAmbiguousNestedMatch_Nil(t *testing.T) {
	if isAmbiguousNestedMatch(nil) {
		t.Error("expected false for nil error")
	}
}

// ---- renderTFValue in HCL context: string with special chars ends up valid --

func TestRenderTFValue_StringWithSpecialChars_ValidHCL(t *testing.T) {
	// Ensure the rendered output, when embedded in HCL, produces valid syntax.
	// If hclEscapeString is broken, a backslash or quote would break HCL parsing.
	v := tfplan.TFStr(`C:\Users\admin`)
	got, err := renderTFValue(v)
	if err != nil {
		t.Fatal(err)
	}
	// Should be exactly: "C:\\Users\\admin"
	want := `"C:\\Users\\admin"`
	if string(got) != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
