package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/lxn/walk"
	. "github.com/lxn/walk/declarative"

	"kiemthedeployforge/internal/configpatch"
	"kiemthedeployforge/internal/database"
	"kiemthedeployforge/internal/guiutil"
	"kiemthedeployforge/internal/install"
)

// uiRefreshInterval caps how often the install pipeline is allowed to repaint
// the window. Extraction reports once per file, so without this the status text
// and percentage redraw continuously and visibly strobe.
const uiRefreshInterval = 90 * time.Millisecond

func gib(value uint64) float64 {
	return float64(value) / (1024 * 1024 * 1024)
}

// releaseLayoutSummary describes what this particular release will create. The
// components and the MySQL accounts both come from the manifest, because the
// bot is optional and the credentials are chosen when the release is built.
func releaseLayoutSummary(plan *install.Plan) string {
	components := install.ClientTargetRoot + ", " + install.ServerTargetRoot
	if plan.IncludesBot {
		components += ", " + install.BotTargetRoot
	}
	accounts := plan.Accounts.WithDefaults()
	return fmt.Sprintf("Sẽ tạo %s + shortcut Game và AutoPk ngoài Desktop  •  MySQL %s/%s và %s/%s trên %s",
		components, database.RootUser, accounts.RootPassword, accounts.BotUser, accounts.BotPassword, database.DatabaseName)
}

// diskSummary reports free space for the directory the operator picked and for
// the system drive the bot depends on, whichever volumes those are.
func diskSummary(plan *install.Plan, root string) string {
	targetFree, err := install.AvailableBytes(root)
	if err != nil {
		return "Không đọc được dung lượng trống: " + err.Error()
	}
	summary := fmt.Sprintf("Cần %.2f GiB • %s còn trống %.2f GiB", gib(uint64(plan.RequiredBytes)), root, gib(targetFree))
	if targetFree < uint64(plan.RequiredBytes) {
		summary += " — KHÔNG ĐỦ"
	}
	systemDrive, systemFree, systemErr := install.SystemDriveFree()
	if systemErr != nil {
		return summary + " • không đọc được ổ hệ thống: " + systemErr.Error()
	}
	summary += fmt.Sprintf(" • Bot cần %.0f GiB trên %s, còn trống %.2f GiB", gib(install.BotSystemDriveFreeBytes), systemDrive, gib(systemFree))
	if systemFree < install.BotSystemDriveFreeBytes {
		summary += " — CẢNH BÁO"
	}
	return summary
}

var buildVersion = "dev"

func main() {
	if len(os.Args) > 1 && os.Args[1] == "--cli-plan" {
		os.Exit(runCLIPlan(os.Args[2:]))
	}
	if len(os.Args) > 1 && os.Args[1] == "--cli-install" {
		os.Exit(runCLIInstall(os.Args[2:]))
	}
	if !install.IsAdministrator() {
		executable, err := os.Executable()
		if err != nil || install.Elevate(executable, os.Args[1:]) != nil {
			walk.MsgBox(nil, "Administrator required", "Setup must run as Administrator.", walk.MsgBoxIconError)
		}
		return
	}
	runGUI()
}

func runCLIPlan(args []string) int {
	set := flag.NewFlagSet("plan", flag.ContinueOnError)
	setupPath := set.String("setup", executablePath(), "Setup.exe bootstrap next to the offline ISO")
	root := set.String("install-root", `C:\KiemTheServer`, "installation directory")
	verify := set.Bool("verify", false, "hash all files in the offline payload package")
	if err := set.Parse(args); err != nil {
		return 2
	}
	plan, err := install.BuildPlan(context.Background(), install.Options{SetupPath: *setupPath, InstallRoot: *root, VerifyPackage: *verify})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	raw, _ := json.MarshalIndent(plan, "", "  ")
	fmt.Println(string(raw))
	return 0
}

func runCLIInstall(args []string) int {
	set := flag.NewFlagSet("install", flag.ContinueOnError)
	setupPath := set.String("setup", executablePath(), "Setup.exe bootstrap next to the offline ISO")
	root := set.String("install-root", `C:\KiemTheServer`, "installation directory")
	if err := set.Parse(args); err != nil {
		return 2
	}
	if !install.IsAdministrator() {
		fmt.Fprintln(os.Stderr, "Administrator permission is required")
		return 1
	}
	state, err := install.Run(context.Background(), install.Options{SetupPath: *setupPath, InstallRoot: *root, Logf: func(format string, args ...any) { fmt.Printf(format+"\n", args...) }}, func(percent int, stage, detail string) {
		fmt.Printf("%d%% %s: %s\n", percent, stage, detail)
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	raw, _ := json.MarshalIndent(state, "", "  ")
	fmt.Println(string(raw))
	return 0
}

func runGUI() {
	var mw *walk.MainWindow
	var rootEdit, lanEdit *walk.LineEdit
	var stageLabel, fileLabel, packageLabel, diskLabel, layoutLabel *walk.Label
	var installButton, cancelButton *walk.PushButton
	var rootBrowseButton *walk.PushButton
	var cancel context.CancelFunc
	var running bool
	var closeAfterRun bool
	var stateMu sync.Mutex
	var latestPlan *install.Plan
	setupPath := executablePath()

	meter, meterWidget := guiutil.NewMeter()
	console, consoleWidget := guiutil.NewConsole(200)

	// refreshDiskSummary re-reads free space for whatever directory is typed
	// now. The preflight plan was measured against the default directory, so
	// the numbers must follow the operator's choice.
	refreshDiskSummary := func() {
		if latestPlan == nil || diskLabel == nil {
			return
		}
		root := strings.TrimSpace(rootEdit.Text())
		if err := install.ValidateInstallTarget(root, setupPath); err != nil {
			diskLabel.SetTextColor(guiutil.ColorWarn)
			diskLabel.SetText(err.Error())
			return
		}
		diskLabel.SetTextColor(guiutil.ColorTextMuted)
		diskLabel.SetText(diskSummary(latestPlan, root))
	}

	browseRoot := func() {
		dialog := new(walk.FileDialog)
		dialog.Title = "Select installation directory"
		dialog.FilePath = rootEdit.Text()
		accepted, err := dialog.ShowBrowseFolder(mw)
		if err != nil {
			walk.MsgBox(mw, "Browse error", err.Error(), walk.MsgBoxIconError)
			return
		}
		if accepted {
			rootEdit.SetText(dialog.FilePath)
			refreshDiskSummary()
		}
	}
	setRunning := func(value bool) {
		stateMu.Lock()
		running = value
		stateMu.Unlock()
		rootEdit.SetEnabled(!value)
		rootBrowseButton.SetEnabled(!value)
		installButton.SetEnabled(!value && lanEdit.Text() != "")
		cancelButton.SetEnabled(value)
	}
	takeCloseAfterRun := func() bool {
		stateMu.Lock()
		defer stateMu.Unlock()
		value := closeAfterRun
		closeAfterRun = false
		return value
	}
	startInstall := func() {
		stateMu.Lock()
		if running {
			stateMu.Unlock()
			return
		}
		stateMu.Unlock()
		root := strings.TrimSpace(rootEdit.Text())
		if root == "" {
			walk.MsgBox(mw, "Install path required", "Choose an installation directory.", walk.MsgBoxIconWarning)
			return
		}
		// Catch the folder that holds Setup.exe and the ISO before any work
		// starts, so the operator is not told about it half way through.
		if err := install.ValidateInstallTarget(root, setupPath); err != nil {
			walk.MsgBox(mw, "Choose another installation directory", err.Error(), walk.MsgBoxIconError)
			return
		}
		detectedLAN := strings.TrimSpace(lanEdit.Text())
		targetFree, freeErr := install.AvailableBytes(root)
		if freeErr != nil {
			walk.MsgBox(mw, "Disk check failed", freeErr.Error(), walk.MsgBoxIconError)
			return
		}
		if latestPlan != nil && targetFree < uint64(latestPlan.RequiredBytes) {
			walk.MsgBox(mw, "Not enough disk space", fmt.Sprintf(
				"%s has only %.2f GiB free but the installation needs %.2f GiB.\r\n\r\nChoose another directory or free up space.",
				root, gib(targetFree), gib(uint64(latestPlan.RequiredBytes))), walk.MsgBoxIconError)
			return
		}
		systemDrive, systemFree, systemErr := install.SystemDriveFree()
		if systemErr != nil {
			walk.MsgBox(mw, "Disk check failed", systemErr.Error(), walk.MsgBoxIconError)
			return
		}
		message := fmt.Sprintf(
			"Install %s, %s, %s, the local MySQL service and jxaccount to:\r\n\r\n%s\r\n\r\n"+
				"Free on the target: %.2f GiB\r\nFree on %s (used by the bot): %.2f GiB\r\n\r\n"+
				"LAN IPv4 detected during preflight: %s\r\nSetup will detect LAN again inside the background installation flow.",
			install.ClientTargetRoot, install.ServerTargetRoot, install.BotTargetRoot, root,
			gib(targetFree), systemDrive, gib(systemFree), detectedLAN)
		icon := walk.MsgBoxIconQuestion
		if systemFree < install.BotSystemDriveFreeBytes {
			message += fmt.Sprintf("\r\n\r\nWARNING: the bot needs at least %.0f GiB free on %s. "+
				"You can still install now, but free up space on %s before starting the bot.",
				gib(install.BotSystemDriveFreeBytes), systemDrive, systemDrive)
			icon = walk.MsgBoxIconWarning
		}
		answer := walk.MsgBox(mw, "Start installation", message, walk.MsgBoxYesNo|icon)
		if answer != walk.DlgCmdYes {
			return
		}
		ctx, cancelInstall := context.WithCancel(context.Background())
		cancel = cancelInstall
		setRunning(true)
		meter.SetProgress(0, "")
		stageLabel.SetText("Bắt đầu cài đặt")
		fileLabel.SetText("")
		console.Reset()
		console.Append("> Bắt đầu cài đặt lúc " + time.Now().Format("15:04:05"))
		relay := guiutil.NewRelay(uiRefreshInterval)
		go func() {
			state, installErr := install.Run(ctx, install.Options{
				SetupPath: setupPath, InstallRoot: root,
				Logf: func(format string, args ...any) {
					message := fmt.Sprintf(format, args...)
					mw.Synchronize(func() { console.Append("   " + message) })
				},
			}, func(percent int, stage, detail string) {
				update, ok := relay.Next(percent, stage, detail)
				if !ok {
					return
				}
				mw.Synchronize(func() {
					stageName := setupStageVietnamese(update.Stage)
					meter.SetProgress(update.Percent, stageName)
					if update.StageChanged {
						stageLabel.SetText(stageName)
						console.Append("")
						console.Append("== " + stageName + " ==")
					}
					if update.DetailChanged {
						fileLabel.SetText(update.Detail)
						console.Append("   " + update.Detail)
					}
				})
			})
			mw.Synchronize(func() {
				setRunning(false)
				if takeCloseAfterRun() {
					mw.Close()
					return
				}
				if installErr != nil {
					stageLabel.SetText("Cài đặt thất bại")
					fileLabel.SetText("")
					console.Append("")
					console.Append("!! LỖI: " + installErr.Error())
					walk.MsgBox(mw, "Installation failed", installErr.Error(), walk.MsgBoxIconError)
					return
				}
				meter.SetProgress(100, "Hoàn tất")
				stageLabel.SetText("Hoàn tất")
				fileLabel.SetText("")
				mysqlOrigin := "installed as service " + state.MySQL.Service
				if state.MySQL.Adopted {
					mysqlOrigin = "reused the MySQL already running on 127.0.0.1:3306"
				}
				components := install.ClientTargetRoot + ", " + install.ServerTargetRoot
				if state.PatchedBotKeys > 0 {
					components += ", " + install.BotTargetRoot
				}
				accounts := latestPlan.Accounts.WithDefaults()
				done := fmt.Sprintf(
					"Installation completed.\r\n\r\nPath: %s\r\n  %s\r\n\r\nLAN IPv4: %s (%d config keys updated)\r\n"+
						"Desktop shortcuts: %d\r\nMySQL %s: %s\r\nAccounts: %s/%s and %s/%s on %s\r\njxaccount: ready",
					state.InstallRoot, components,
					state.LANIP, state.PatchedKeys, state.Shortcuts, state.MySQL.Version, mysqlOrigin,
					database.RootUser, accounts.RootPassword, accounts.BotUser, accounts.BotPassword, database.DatabaseName)
				icon := walk.MsgBoxIconInformation
				if state.BotDiskWarning != "" {
					done += "\r\n\r\nWARNING: " + state.BotDiskWarning
					icon = walk.MsgBoxIconWarning
				}
				walk.MsgBox(mw, "Kiem The Server is ready", done, icon)
			})
		}()
	}

	caption := func(text string) Label {
		return Label{Text: text, TextColor: guiutil.ColorTextFaint, Font: Font{Family: guiutil.UIFontFamily, PointSize: 8, Bold: true}}
	}

	err := (MainWindow{
		AssignTo:   &mw,
		Title:      "Kiem The Server Setup x64",
		MinSize:    Size{Width: 860, Height: 700},
		Size:       Size{Width: 920, Height: 780},
		Font:       Font{Family: guiutil.UIFontFamily, PointSize: 9},
		Background: SolidColorBrush{Color: guiutil.ColorPage},
		Layout:     VBox{MarginsZero: true, SpacingZero: true},
		Children: []Widget{
			Composite{
				Background: SolidColorBrush{Color: guiutil.ColorHeader},
				Layout:     VBox{Margins: guiutil.Margins(30, 20, 30, 20), Spacing: 4},
				Children: []Widget{
					Label{Text: "KIEM THE SERVER SETUP", TextColor: guiutil.ColorAccentText, Font: Font{Family: guiutil.UIFontFamily, PointSize: 20, Bold: true}},
					Label{Text: "Cài Client + Server + Bot + MySQL 5.5.15 + jxaccount hoàn toàn ngoại tuyến.", TextColor: guiutil.ColorHeaderSub},
					Label{Text: fmt.Sprintf("IP LAN tự nhận từ card mạng vật lý; không có gateway vẫn tự fallback và ghi %d khóa cấu hình.", len(configpatch.LANRules("127.0.0.1"))), TextColor: guiutil.ColorHeaderSub},
					Label{Text: install.AuthorCredit, TextColor: guiutil.ColorAccent, Font: Font{Family: guiutil.UIFontFamily, PointSize: 8, Bold: true}},
				},
			},
			ScrollView{
				Background: SolidColorBrush{Color: guiutil.ColorPage},
				Layout:     VBox{Margins: guiutil.Margins(18, 16, 18, 16), Spacing: 12},
				Children: []Widget{
					Composite{
						Background: SolidColorBrush{Color: guiutil.ColorCard},
						Layout:     VBox{Margins: guiutil.Margins(20, 16, 20, 18), Spacing: 10},
						Children: []Widget{
							caption("ĐÍCH CÀI ĐẶT"),
							Composite{
								Layout: Grid{Columns: 3, Spacing: 8, MarginsZero: true},
								Children: []Widget{
									Label{Text: "Thư mục cài đặt", TextColor: guiutil.ColorText, MinSize: Size{Width: 132}},
									LineEdit{AssignTo: &rootEdit, Text: `C:\KiemTheServer`, MinSize: Size{Height: 24}},
									PushButton{AssignTo: &rootBrowseButton, Text: "Duyệt…", MinSize: Size{Width: 90, Height: 26}, OnClicked: browseRoot},

									Label{Text: "IPv4 LAN tự nhận", TextColor: guiutil.ColorText},
									LineEdit{AssignTo: &lanEdit, ReadOnly: true, CueBanner: "Đang tự dò card mạng vật lý…", MinSize: Size{Height: 24}},
									Label{Text: "Tự động", TextColor: guiutil.ColorOk},
								},
							},
						},
					},
					Composite{
						Background: SolidColorBrush{Color: guiutil.ColorCard},
						Layout:     VBox{Margins: guiutil.Margins(20, 16, 20, 18), Spacing: 5},
						Children: []Widget{
							caption("GÓI CÀI ĐẶT VÀ DUNG LƯỢNG"),
							Label{AssignTo: &packageLabel, Text: "Đang tự tìm ISO và đọc gói cài đặt…", TextColor: guiutil.ColorTextMuted},
							Label{AssignTo: &diskLabel, Text: "Đang kiểm tra dung lượng ổ đĩa…", TextColor: guiutil.ColorTextMuted},
							Label{AssignTo: &layoutLabel, Text: "Đang đọc thành phần của bản phát hành…", TextColor: guiutil.ColorTextMuted},
						},
					},
					Composite{
						Background: SolidColorBrush{Color: guiutil.ColorCard},
						Layout:     VBox{Margins: guiutil.Margins(20, 16, 20, 18), Spacing: 8},
						Children: []Widget{
							caption("TIẾN ĐỘ"),
							Label{AssignTo: &stageLabel, Text: "Kiểm tra trước khi cài", TextColor: guiutil.ColorText, Font: Font{Family: guiutil.UIFontFamily, PointSize: 11, Bold: true}, AlwaysConsumeSpace: true},
							meterWidget,
							Label{AssignTo: &fileLabel, Text: "", TextColor: guiutil.ColorTextMuted, EllipsisMode: EllipsisPath, AlwaysConsumeSpace: true, MinSize: Size{Height: 17}},
							consoleWidget,
							Composite{
								Layout: HBox{MarginsZero: true, Spacing: 8},
								Children: []Widget{
									HSpacer{},
									PushButton{AssignTo: &cancelButton, Text: "Hủy", Enabled: false, MinSize: Size{Width: 110, Height: 34}, OnClicked: func() {
										if cancel != nil {
											cancel()
										}
									}},
									PushButton{AssignTo: &installButton, Text: "CÀI ĐẶT", Enabled: false, MinSize: Size{Width: 190, Height: 34}, Font: Font{Family: guiutil.UIFontFamily, PointSize: 10, Bold: true}, OnClicked: startInstall},
								},
							},
						},
					},
				},
			},
		},
	}).Create()
	if err != nil {
		panic(err)
	}
	mw.Closing().Attach(func(canceled *bool, _ walk.CloseReason) {
		stateMu.Lock()
		active := running
		if active {
			closeAfterRun = true
		}
		stateMu.Unlock()
		if !active {
			return
		}
		*canceled = true
		if cancel != nil {
			cancel()
		}
		stageLabel.SetText("Đang hủy an toàn trước khi đóng...")
		cancelButton.SetEnabled(false)
	})

	preflightRoot := rootEdit.Text()
	preflightCtx, cancelPreflight := context.WithCancel(context.Background())
	cancel = cancelPreflight
	stateMu.Lock()
	running = true
	stateMu.Unlock()
	rootEdit.SetEnabled(false)
	rootBrowseButton.SetEnabled(false)
	installButton.SetEnabled(false)
	cancelButton.SetEnabled(false)
	go func() {
		plan, planErr := install.BuildPlan(preflightCtx, install.Options{SetupPath: setupPath, InstallRoot: preflightRoot})
		mw.Synchronize(func() {
			if planErr == nil {
				lanEdit.SetText(plan.LAN.Address)
			}
			setRunning(false)
			if takeCloseAfterRun() {
				mw.Close()
				return
			}
			if planErr != nil {
				packageLabel.SetText("Kiểm tra gói cài đặt thất bại")
				diskLabel.SetText("")
				stageLabel.SetText(planErr.Error())
				walk.MsgBox(mw, "Setup preflight failed", planErr.Error(), walk.MsgBoxIconError)
				return
			}
			latestPlan = plan
			packageLabel.SetText(fmt.Sprintf("Release %s • %d files • %.2f GiB • MySQL %s", plan.ReleaseID, plan.PayloadFiles, float64(plan.PayloadBytes)/(1024*1024*1024), displayMySQLVersion(plan.MySQLVersion)))
			layoutLabel.SetText(releaseLayoutSummary(plan))
			refreshDiskSummary()
			if plan.BotDiskWarning != "" {
				diskLabel.SetTextColor(guiutil.ColorWarn)
				walk.MsgBox(mw, "Not enough space for the bot", plan.BotDiskWarning, walk.MsgBoxIconWarning)
			}
			stageLabel.SetText("Sẵn sàng — card LAN: " + plan.LAN.Interface)
		})
	}()
	mw.Run()
}

func displayMySQLVersion(version string) string {
	version = strings.TrimSpace(version)
	if strings.EqualFold(version, "5.5.15-win32") {
		return "5.5.15 (dịch vụ MySQL legacy Win32 riêng)"
	}
	return version
}

func setupStageVietnamese(stage string) string {
	translations := map[string]string{
		"Extract payload":     "Giải nén dữ liệu",
		"Repair payload":      "Kiểm tra và phục hồi dữ liệu",
		"Patch configuration": "Cập nhật cấu hình IP",
		"Install MySQL":       "Cài đặt MySQL",
		"Create shortcuts":    "Tạo shortcut ngoài Desktop",
		"Complete":            "Hoàn tất",
	}
	if translated, exists := translations[stage]; exists {
		return translated
	}
	return stage
}

func executablePath() string {
	path, err := os.Executable()
	if err != nil {
		return filepath.Clean("Setup.exe")
	}
	return path
}
