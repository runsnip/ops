package expand

import "testing"

func TestString(t *testing.T) {
	env := Map(map[string]string{"A": "a", "EMPTY": "", "P": "preview.example"})
	for in, want := range map[string]string{
		"$A/${A}":                    "a/a",
		"${MISSING:-x}":              "x",
		"${EMPTY:-x}":                "x",
		"${EMPTY-x}":                 "",
		"${MISSING-x}":               "x",
		"${SHARE_DOMAIN:-${P}}":      "preview.example",
		"${A:+on}${MISSING:+on}":     "on",
		"$$A":                        "$A",
		"wildcard-test.${P}":         "wildcard-test.preview.example",
		"${DATA_DIR:-/srv/x}/models": "/srv/x/models",
		"cost $5":                    "cost $5",
	} {
		if got := String(in, env); got != want {
			t.Errorf("%q → %q, want %q", in, got, want)
		}
	}
}

func TestNames(t *testing.T) {
	got := Names("${SHARE_DOMAIN:-${PREVIEW_DOMAIN}} $A ${A}")
	if len(got) != 3 || got[0] != "SHARE_DOMAIN" || got[1] != "PREVIEW_DOMAIN" || got[2] != "A" {
		t.Fatalf("%v", got)
	}
}
