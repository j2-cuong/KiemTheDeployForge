package install

import (
	"reflect"
	"strings"
	"testing"
)

func TestInstallOptionsCannotOverrideDetectedLAN(t *testing.T) {
	typeOfOptions := reflect.TypeOf(Options{})
	for index := 0; index < typeOfOptions.NumField(); index++ {
		name := strings.ToLower(typeOfOptions.Field(index).Name)
		for _, forbidden := range []string{"lan", "lanip", "ip", "ipaddress", "address"} {
			if name == forbidden {
				t.Fatalf("Options exposes forbidden LAN override field %q", typeOfOptions.Field(index).Name)
			}
		}
	}
}
