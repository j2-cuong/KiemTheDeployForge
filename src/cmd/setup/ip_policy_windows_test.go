package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

func TestSetupHasNoLANOverrideFlagAndDisplaysReadOnlyDetection(t *testing.T) {
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate setup source")
	}
	mainPath := strings.TrimSuffix(filename, "ip_policy_windows_test.go") + "main.go"
	raw, err := parser.ParseFile(token.NewFileSet(), mainPath, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	forbiddenFlags := map[string]bool{
		"lan": true, "lanip": true, "ip": true, "ipv4": true,
		"ipaddress": true, "lanaddress": true, "serverip": true,
	}
	ast.Inspect(raw, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		argument := 0
		if strings.HasSuffix(selector.Sel.Name, "Var") {
			argument = 1
		}
		if argument >= len(call.Args) {
			return true
		}
		literal, ok := call.Args[argument].(*ast.BasicLit)
		if !ok || literal.Kind != token.STRING {
			return true
		}
		name, err := strconv.Unquote(literal.Value)
		if err != nil {
			return true
		}
		normalized := strings.NewReplacer("-", "", "_", "", " ", "").Replace(strings.ToLower(name))
		if forbiddenFlags[normalized] {
			t.Errorf("setup exposes forbidden LAN/IP override flag %q", name)
		}
		return true
	})

	sourceBytes, err := os.ReadFile(mainPath)
	if err != nil {
		t.Fatal(err)
	}
	source := string(sourceBytes)
	if !strings.Contains(source, "LineEdit{AssignTo: &lanEdit, ReadOnly: true") {
		t.Fatal("detected LAN IPv4 is not displayed in a read-only field")
	}
	if !strings.Contains(source, "lanEdit.SetText(plan.LAN.Address)") {
		t.Fatal("LAN display is not populated from the auto-detected install plan")
	}
}
