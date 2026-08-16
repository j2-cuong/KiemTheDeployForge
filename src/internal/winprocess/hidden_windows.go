package winprocess

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/windows"
)

// PowerShellCommandContext resolves Windows PowerShell from the real system
// directory so a same-directory or PATH-injected executable cannot be launched.
func PowerShellCommandContext(ctx context.Context, args ...string) (*exec.Cmd, error) {
	systemDirectory, err := windows.GetSystemDirectory()
	if err != nil {
		return nil, fmt.Errorf("resolve Windows system directory: %w", err)
	}
	path := filepath.Join(systemDirectory, "WindowsPowerShell", "v1.0", "powershell.exe")
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("resolve trusted Windows PowerShell: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("trusted Windows PowerShell is not a regular file: %s", path)
	}
	return exec.CommandContext(ctx, path, args...), nil
}

// Hide prevents console helpers from flashing a window behind the Builder or Setup GUI.
func Hide(command *exec.Cmd) {
	if command.SysProcAttr == nil {
		command.SysProcAttr = &syscall.SysProcAttr{}
	}
	command.SysProcAttr.HideWindow = true
	command.SysProcAttr.CreationFlags |= windows.CREATE_NO_WINDOW
}
