package guiutil

import (
	"reflect"

	"github.com/lxn/walk/declarative"
)

func Margins(left, top, right, lower int) declarative.Margins {
	var result declarative.Margins
	target := reflect.ValueOf(&result).Elem()
	values := [...]int{left, top, right, lower}
	if target.NumField() != len(values) {
		panic("unexpected Walk margins layout")
	}
	for index, value := range values {
		target.Field(index).SetInt(int64(value))
	}
	return result
}
