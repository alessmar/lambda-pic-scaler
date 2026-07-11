package scaler

import (
	"fmt"
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	ok, err := Parse(`[{"w":800,"h":600,"postfix":"-md"},{"w":100,"postfix":"-thumb"}]`)
	if err != nil || len(ok) != 2 || ok[1].H != 0 {
		t.Fatalf("got %v, %v", ok, err)
	}
	for _, bad := range []string{
		``, `{}`, `[]`, `[{"w":1,"h":1}]`, `[{"w":0,"h":0,"postfix":"-a"}]`,
		`[{"w":-1,"postfix":"-a"}]`, `[{"w":99999,"postfix":"-a"}]`,
		`[{"w":1,"postfix":"../x"}]`, `[{"w":1,"postfix":"-a"},{"h":2,"postfix":"-a"}]`,
	} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("expected error for %s", bad)
		}
	}
}

// A valid spec must never yield the input key, or the service would overwrite its own input.
func TestOutputKeyNeverEqualsInput(t *testing.T) {
	keys := []string{"a.jpg", "a", ".jpg", "a/b.c.png", "x/", "-md.jpg", "a-md.jpg", ""}
	for _, p := range []string{"-md", ".", "_", "x", "-md-md", " "} {
		for _, k := range keys {
			if got := (Spec{W: 1, Postfix: p}).OutputKey(k); got == k {
				t.Errorf("key %q postfix %q maps to itself", k, p)
			}
		}
	}
}

func TestParseCapsFanOut(t *testing.T) {
	raw := "["
	for i := range MaxSpecs + 1 {
		if i > 0 {
			raw += ","
		}
		raw += fmt.Sprintf(`{"w":1,"postfix":"-%d"}`, i)
	}
	if _, err := Parse(raw + "]"); err == nil {
		t.Error("expected error above MaxSpecs")
	}
}

func TestOutputKey(t *testing.T) {
	for in, want := range map[string]string{
		"a/b.jpg": "a/b-md.jpg", "b.tar.png": "b.tar-md.png", "noext": "noext-md",
	} {
		if got := (Spec{Postfix: "-md"}).OutputKey(in); got != want {
			t.Errorf("%s: got %s want %s", in, got, want)
		}
	}
}

// The tags cannot use the MaxDim constant, so check that the limit they declare is MaxDim.
func TestMaxDimMatchesTags(t *testing.T) {
	for _, field := range []string{"W", "H"} {
		ok := fmt.Sprintf(`[{"%s":%d,"postfix":"-a"}]`, strings.ToLower(field), MaxDim)
		if _, err := Parse(ok); err != nil {
			t.Errorf("%s=%d should be valid: %v", field, MaxDim, err)
		}
		bad := fmt.Sprintf(`[{"%s":%d,"postfix":"-a"}]`, strings.ToLower(field), MaxDim+1)
		if _, err := Parse(bad); err == nil {
			t.Errorf("%s=%d should be invalid", field, MaxDim+1)
		}
	}
}

func TestParseRules(t *testing.T) {
	valid := []string{
		`[{"w":10,"postfix":"-a"}]`,                       // height derived
		`[{"h":10,"postfix":"-a"}]`,                       // width derived
		`[{"w":10,"h":10,"postfix":"-a"}]`,                // fit in a box
		`[{"w":0,"h":5,"postfix":"-a"}]`,                  // explicit zero width
		`[{"w":5,"h":0,"postfix":"-a"}]`,                  // explicit zero height
		`[{"w":1,"postfix":"-a"},{"w":1,"postfix":"-b"}]`, // distinct postfixes
	}
	for _, raw := range valid {
		if _, err := Parse(raw); err != nil {
			t.Errorf("%s: %v", raw, err)
		}
	}
	invalid := []string{
		`[{"w":0,"h":0,"postfix":"-a"}]`,                  // nothing to resize to
		`[{"postfix":"-a"}]`,                              // missing sizes
		`[{"w":1,"postfix":"a/b"}]`,                       // slash
		`[{"w":1,"postfix":"a\\b"}]`,                      // backslash
		`[{"w":1,"postfix":""}]`,                          // would overwrite the original
		`[{"w":1}]`,                                       // postfix missing
		`[{"w":1,"postfix":"-a"},{"h":2,"postfix":"-a"}]`, // duplicate
		`[{"w":-1,"postfix":"-a"}]`,
		`[{"w":1,"h":-1,"postfix":"-a"}]`,
		`[{"w":"x","postfix":"-a"}]`, // wrong type
	}
	for _, raw := range invalid {
		if _, err := Parse(raw); err == nil {
			t.Errorf("expected an error for %s", raw)
		}
	}
}

func TestParseAcceptsExactlyMaxSpecs(t *testing.T) {
	parts := make([]string, MaxSpecs)
	for i := range parts {
		parts[i] = fmt.Sprintf(`{"w":1,"postfix":"-%d"}`, i)
	}
	if _, err := Parse("[" + strings.Join(parts, ",") + "]"); err != nil {
		t.Fatalf("%d specs should be valid: %v", MaxSpecs, err)
	}
}

// The rule that failed and where, in the log line an operator reads.
func TestParseErrorNamesTheRule(t *testing.T) {
	for raw, want := range map[string]string{
		`[{"w":9999,"postfix":"-a"}]`:                     "[0].W max=8192",
		`[{"w":0,"h":0,"postfix":"-a"}]`:                  "[0].H required_without=W",
		`[{"w":1,"postfix":"a/b"}]`:                       "[0].Postfix excludesall=/\\",
		`[{"w":1,"postfix":"-a"},{"w":2,"postfix":"-a"}]`: "unique=Postfix",
		`[]`: "min=1",
	} {
		_, err := Parse(raw)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: got %v, want it to mention %q", raw, err, want)
		}
	}
}
