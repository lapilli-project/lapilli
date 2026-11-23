package oncall

import "testing"

// Each Windows name in the table is a zone this system's zone files hold:
// the table names zones by the names tzdata now gives them. None is an
// IANA name but UTC.
func TestWindowsZones(t *testing.T) {
	if len(windowsZones) < 100 {
		t.Fatalf("%d names", len(windowsZones))
	}
	for windows, iana := range windowsZones {
		if _, err := LoadZone(iana); err != nil {
			t.Errorf("%s: %s: %v", windows, iana, err)
		}
		// An IANA name wins: only UTC is both, and both are UTC.
		if _, err := LoadZone(windows); err == nil && windows != "UTC" {
			t.Errorf("%s is an IANA name too, and would be read as one", windows)
		}
	}
}
