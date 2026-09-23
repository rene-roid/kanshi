package vitals

import "testing"

func TestParsing(t *testing.T) {
	f := splitFields([]byte("  cpu0 10\t20  30 "), nil)
	if len(f) != 4 || string(f[0]) != "cpu0" || atou(f[3]) != 30 {
		t.Errorf("fields = %q", f)
	}
	if v := atof([]byte("12.34")); v < 12.339 || v > 12.341 {
		t.Errorf("atof = %v", v)
	}
	if v := atou([]byte("1234 kB")); v != 1234 {
		t.Errorf("atou = %v", v)
	}
}
