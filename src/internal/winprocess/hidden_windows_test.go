package winprocess

import (
	"os/exec"
	"testing"

	"golang.org/x/sys/windows"
)

func TestHideDisablesConsoleWindows(t *testing.T) {
	command := exec.Command("cmd.exe", "/c", "exit", "0")
	Hide(command)
	if command.SysProcAttr == nil || !command.SysProcAttr.HideWindow {
		t.Fatal("hidden helper did not set HideWindow")
	}
	if command.SysProcAttr.CreationFlags&windows.CREATE_NO_WINDOW == 0 {
		t.Fatal("hidden helper did not set CREATE_NO_WINDOW")
	}
}
