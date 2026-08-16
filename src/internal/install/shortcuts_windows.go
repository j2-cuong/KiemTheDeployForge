package install

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"

	"kiemthedeployforge/internal/winprocess"
)

// Shortcut is one desktop link Setup publishes after the payload is committed.
type Shortcut struct {
	// LinkName is the .lnk file name shown on the desktop.
	LinkName string
	// RelativeTarget is the executable path relative to the install root.
	RelativeTarget string
	Description    string
}

// DesktopShortcuts are the two launchers requested for the destination
// machine: the game client and the AutoPk bot client.
func DesktopShortcuts() []Shortcut {
	return []Shortcut{
		{
			LinkName:       "Kiem The.lnk",
			RelativeTarget: filepath.Join(ClientTargetRoot, ClientGameShortcutTarget),
			Description:    "Kiem The game client",
		},
		{
			LinkName:       "Kiem The AutoPk.lnk",
			RelativeTarget: filepath.Join(ClientTargetRoot, ClientBotShortcutTarget),
			Description:    "Kiem The AutoPk bot client",
		},
	}
}

// publicDesktopPath resolves the all-users desktop so the shortcuts are
// visible to the signed-in user and not only to the elevated administrator
// account that ran Setup.
func publicDesktopPath() (string, error) {
	path, err := windows.KnownFolderPath(windows.FOLDERID_PublicDesktop, windows.KF_FLAG_DEFAULT)
	if err != nil {
		return "", fmt.Errorf("resolve the all-users desktop directory: %w", err)
	}
	if strings.TrimSpace(path) == "" {
		return "", errors.New("the all-users desktop directory is empty")
	}
	return path, nil
}

// CreateDesktopShortcuts publishes every shortcut whose target exists under
// installRoot. It overwrites links it previously created and returns the
// number written.
func CreateDesktopShortcuts(ctx context.Context, installRoot string, shortcuts []Shortcut, logf func(string, ...any)) (int, error) {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	desktop, err := publicDesktopPath()
	if err != nil {
		return 0, err
	}
	if info, err := os.Stat(desktop); err != nil || !info.IsDir() {
		return 0, fmt.Errorf("the all-users desktop directory is unavailable: %s", desktop)
	}
	created := 0
	for _, shortcut := range shortcuts {
		select {
		case <-ctx.Done():
			return created, ctx.Err()
		default:
		}
		target := filepath.Join(installRoot, shortcut.RelativeTarget)
		info, err := os.Stat(target)
		if err != nil || !info.Mode().IsRegular() {
			return created, fmt.Errorf("shortcut target is missing: %s", target)
		}
		linkPath := filepath.Join(desktop, shortcut.LinkName)
		if err := writeShortcut(ctx, linkPath, target, filepath.Dir(target), shortcut.Description); err != nil {
			return created, fmt.Errorf("create desktop shortcut %s: %w", shortcut.LinkName, err)
		}
		if _, err := os.Stat(linkPath); err != nil {
			return created, fmt.Errorf("desktop shortcut was not written: %s", linkPath)
		}
		logf("Desktop shortcut created: %s -> %s", linkPath, target)
		created++
	}
	return created, nil
}

// shortcutScript reads every path from the environment so no install path can
// be misread as PowerShell syntax.
const shortcutScript = `$ErrorActionPreference = 'Stop'
$shell = New-Object -ComObject WScript.Shell
$link = $shell.CreateShortcut($env:KTF_LINK_PATH)
$link.TargetPath = $env:KTF_TARGET_PATH
$link.WorkingDirectory = $env:KTF_WORKING_DIR
$link.Description = $env:KTF_DESCRIPTION
$link.Save()`

func writeShortcut(ctx context.Context, linkPath, targetPath, workingDir, description string) error {
	command, err := winprocess.PowerShellCommandContext(ctx,
		"-NoLogo", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass",
		"-Command", shortcutScript)
	if err != nil {
		return err
	}
	command.Env = append(os.Environ(),
		"KTF_LINK_PATH="+linkPath,
		"KTF_TARGET_PATH="+targetPath,
		"KTF_WORKING_DIR="+workingDir,
		"KTF_DESCRIPTION="+description,
	)
	winprocess.Hide(command)
	output, err := command.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w (%s)", err, strings.TrimSpace(string(output)))
	}
	return nil
}
