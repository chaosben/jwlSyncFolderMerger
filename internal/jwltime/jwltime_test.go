package jwltime

import "testing"

func TestParseFormats(t *testing.T) {
	want := "2021-10-23T04:39:48Z"
	for _, in := range []string{
		"2021-10-23T04:39:48Z",
		"2021-10-23T04:39:48+00:00",
		"2021-10-23T06:39:48+02:00",
		"2021-10-23T00:39:48-0400",
		"2021-10-23T04:39:48.000Z",
		"2021-10-23 04:39:48",
		"2021-10-23T04:39:48",
	} {
		got, err := Parse(in)
		if err != nil {
			t.Fatalf("Parse(%q): %v", in, err)
		}
		if FormatDB(got) != want {
			t.Errorf("Parse(%q) = %s, want %s", in, FormatDB(got), want)
		}
	}
	if _, err := Parse("garbage"); err == nil {
		t.Error("expected error for garbage")
	}
	if Canonical("garbage") != "garbage" {
		t.Error("Canonical must pass through unparseable input")
	}
}
