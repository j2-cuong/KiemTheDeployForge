package install

import (
	"kiemthedeployforge/internal/configpatch"
	"kiemthedeployforge/internal/release"
)

// AuthorCredit is the single source for the credit line shown in the Builder
// GUI, the Setup GUI and the ISO README.
const AuthorCredit = "Developed by CuongNH - a gift to the Hoi Quan Vo Lam brotherhood"

const (
	// ClientTargetRoot, ServerTargetRoot and BotTargetRoot are the three
	// directories Setup creates inside the installation directory.
	ClientTargetRoot = "Client"
	ServerTargetRoot = configpatch.ServerTargetRoot
	BotTargetRoot    = release.BotTargetRoot

	// BotExecutableName and BotEnvName must exist at the root of the selected
	// bot directory; Setup rewrites the env file to point at the installed
	// server tree and the local MySQL account.
	BotExecutableName = "loginprobe.exe"
	BotEnvName        = "loginprobe.env"

	// ClientGameShortcutTarget and ClientBotShortcutTarget are the two
	// executables published to the desktop, relative to the Client tree.
	ClientGameShortcutTarget = "Game.exe"
	ClientBotShortcutTarget  = `AutoPk\wjxtdAutoPro.exe`
)

// BotSystemDriveFreeBytes is the free space the bot needs on the Windows
// system drive, independent of where the user installs the release.
const BotSystemDriveFreeBytes = uint64(20 * 1024 * 1024 * 1024)
