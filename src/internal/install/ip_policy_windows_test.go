package install

import (
	"reflect"
	"testing"
)

// The LAN address used to be detection-only, and a test forbade any override
// field. That rule could not survive a hosted server behind provider NAT, where
// the address clients connect to is held by the provider and is bound to no
// adapter on the machine. The rule is now the opposite: the override exists,
// and it must be a single explicit field so it can never be set by accident.
func TestOptionsExposeExactlyOneLANOverride(t *testing.T) {
	typeOfOptions := reflect.TypeOf(Options{})
	field, ok := typeOfOptions.FieldByName("LANAddress")
	if !ok {
		t.Fatal("Options has no LANAddress override")
	}
	if field.Type.Kind() != reflect.String {
		t.Fatalf("LANAddress is %s, want a string", field.Type.Kind())
	}
	overrides := 0
	for index := range typeOfOptions.NumField() {
		switch typeOfOptions.Field(index).Name {
		case "LANAddress":
			overrides++
		case "LAN", "IP", "IPAddress", "Address", "LANIP":
			t.Fatalf("Options exposes a second LAN override field %q", typeOfOptions.Field(index).Name)
		}
	}
	if overrides != 1 {
		t.Fatalf("expected exactly one override field, found %d", overrides)
	}
}
