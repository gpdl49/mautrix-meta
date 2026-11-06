package metaid

import (
	"testing"
)

func TestCopyFromKeepsTimezone(t *testing.T) {
	existing := &UserLoginMetadata{Timezone: "Europe/London", IGID: "1"}
	existing.CopyFrom(&UserLoginMetadata{IGID: "1", LoginUA: "new"})
	if existing.Timezone != "Europe/London" || existing.LoginUA != "new" {
		t.Errorf("re-login lost the timezone or ignored the update: %+v", existing)
	}
}

func TestLoadTimezone(t *testing.T) {
	var nilMeta *UserLoginMetadata
	if loc, err := nilMeta.LoadTimezone(); loc != nil || err != nil {
		t.Errorf("nil metadata: %v, %v", loc, err)
	}
	if loc, err := (&UserLoginMetadata{}).LoadTimezone(); loc != nil || err != nil {
		t.Errorf("unset: %v, %v", loc, err)
	}
	loc, err := (&UserLoginMetadata{Timezone: "Asia/Kolkata"}).LoadTimezone()
	if err != nil || loc.String() != "Asia/Kolkata" {
		t.Errorf("set: %v, %v", loc, err)
	}
	if _, err = (&UserLoginMetadata{Timezone: "Nowhere/Land"}).LoadTimezone(); err == nil {
		t.Error("bad zone accepted")
	}
}
