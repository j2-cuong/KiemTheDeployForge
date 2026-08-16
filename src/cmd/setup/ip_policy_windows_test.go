package main

import (
	"os"
	"runtime"
	"strings"
	"testing"
)

// The address field was read-only while detection was the only source. It is
// editable now, because detection cannot see the address of a hosted server
// behind provider NAT. These checks pin the parts that keep the change safe:
// the field is still pre-filled from detection, and nothing reaches the
// installer without being validated first.
func TestSetupValidatesTheEditableAddressField(t *testing.T) {
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate setup source")
	}
	mainPath := strings.TrimSuffix(filename, "ip_policy_windows_test.go") + "main.go"
	sourceBytes, err := os.ReadFile(mainPath)
	if err != nil {
		t.Fatal(err)
	}
	source := string(sourceBytes)

	if strings.Contains(source, "AssignTo: &lanEdit, ReadOnly: true") {
		t.Fatal("the address field is read-only again; a NAT'd server cannot be installed")
	}
	if !strings.Contains(source, "lanEdit.SetText(plan.LAN.Address)") {
		t.Fatal("the address field is not pre-filled from detection")
	}
	// Typed input must go through network.Manual before it is used, otherwise a
	// typo becomes a server nobody can reach.
	if !strings.Contains(source, "network.Manual(text)") {
		t.Fatal("typed input is not validated while the operator edits it")
	}
	if !strings.Contains(source, "network.Manual(lanAddress)") {
		t.Fatal("typed input is not validated before the install starts")
	}
	if !strings.Contains(source, "LANAddress: lanCandidate.Address") {
		t.Fatal("the installer is not given the validated address")
	}
}
