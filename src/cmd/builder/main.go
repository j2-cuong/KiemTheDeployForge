package main

import (
	"context"
	"embed"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/lxn/walk"
	. "github.com/lxn/walk/declarative"

	"kiemthedeployforge/internal/builder"
	"kiemthedeployforge/internal/configpatch"
	"kiemthedeployforge/internal/database"
	"kiemthedeployforge/internal/guiutil"
	"kiemthedeployforge/internal/install"
	"kiemthedeployforge/internal/release"
)

// mysqlDisplayVersion matches the pinned package the Builder embeds.
const mysqlDisplayVersion = "5.5.15 Win32"

// uiRefreshInterval caps how often the packaging pipeline is allowed to repaint
// the window. The pipeline reports once per I/O block, so without this the
// status text and percentage redraw continuously and visibly strobe.
const uiRefreshInterval = 90 * time.Millisecond

//go:embed assets/*
var assets embed.FS

func main() {
	if len(os.Args) > 1 && os.Args[1] == "--cli" {
		os.Exit(runCLI(os.Args[2:]))
	}
	runGUI()
}

func runCLI(args []string) int {
	set := flag.NewFlagSet("builder", flag.ContinueOnError)
	client := set.String("client", "", "Client directory")
	server := set.String("server", "", "Server directory")
	bot := set.String("bot", "", "Bot directory (optional)")
	sql := set.String("sql", "", "jxaccount SQL file")
	output := set.String("output", "", "output directory")
	skipISO := set.Bool("skip-iso", false, "skip ISO creation")
	rootPassword := set.String("root-password", "", "MySQL root password (default "+release.DefaultRootPassword+")")
	botUser := set.String("bot-user", "", "MySQL bot user (default "+release.DefaultBotUser+")")
	botPassword := set.String("bot-password", "", "MySQL bot password (default "+release.DefaultBotPassword+")")
	if err := set.Parse(args); err != nil {
		return 2
	}
	stub, err := loadStub()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	result, err := builder.Build(context.Background(), builder.Options{
		ClientPath: *client, ServerPath: *server, BotPath: *bot, SQLPath: *sql, OutputPath: *output,
		Accounts: release.Accounts{RootPassword: *rootPassword, BotUser: *botUser, BotPassword: *botPassword},
		SkipISO:  *skipISO, SetupStub: stub,
		Progress: func(percent int, stage, detail string) { fmt.Printf("%d%% %s: %s\n", percent, stage, detail) },
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	raw, _ := json.MarshalIndent(result, "", "  ")
	fmt.Println(string(raw))
	return 0
}

func runGUI() {
	var mw *walk.MainWindow
	var clientEdit, serverEdit, botEdit, sqlEdit, outputEdit *walk.LineEdit
	var stageLabel, fileLabel, accountsLabel *walk.Label
	var buildButton, cancelButton, accountsButton *walk.PushButton
	var clientBrowseButton, serverBrowseButton, botBrowseButton, sqlBrowseButton, outputBrowseButton *walk.PushButton
	var cancel context.CancelFunc
	var running bool
	var closeAfterRun bool
	var stateMu sync.Mutex

	accounts := release.DefaultAccounts()
	editAccounts := func() {
		updated, ok := showAccountsDialog(mw, accounts)
		if !ok {
			return
		}
		accounts = updated
		accountsLabel.SetText(accountsSummary(accounts))
	}

	meter, meterWidget := guiutil.NewMeter()
	console, consoleWidget := guiutil.NewConsole(220)

	browseFolder := func(edit **walk.LineEdit, title string) func() {
		return func() {
			dialog := new(walk.FileDialog)
			dialog.Title = title
			if *edit != nil {
				dialog.FilePath = (*edit).Text()
			}
			accepted, err := dialog.ShowBrowseFolder(mw)
			if err != nil {
				walk.MsgBox(mw, "Browse error", err.Error(), walk.MsgBoxIconError)
				return
			}
			if accepted {
				(*edit).SetText(dialog.FilePath)
			}
		}
	}
	browseSQL := func() {
		dialog := new(walk.FileDialog)
		dialog.Title = "Select jxaccount SQL"
		dialog.Filter = "SQL files (*.sql)|*.sql"
		dialog.FilePath = sqlEdit.Text()
		accepted, err := dialog.ShowOpen(mw)
		if err != nil {
			walk.MsgBox(mw, "Browse error", err.Error(), walk.MsgBoxIconError)
			return
		}
		if accepted {
			sqlEdit.SetText(dialog.FilePath)
		}
	}
	setRunning := func(value bool) {
		stateMu.Lock()
		running = value
		stateMu.Unlock()
		for _, control := range []walk.Widget{
			clientEdit, serverEdit, botEdit, sqlEdit, outputEdit,
			clientBrowseButton, serverBrowseButton, botBrowseButton, sqlBrowseButton, outputBrowseButton,
			accountsButton, buildButton,
		} {
			if control != nil {
				control.SetEnabled(!value)
			}
		}
		if cancelButton != nil {
			cancelButton.SetEnabled(value)
		}
	}
	takeCloseAfterRun := func() bool {
		stateMu.Lock()
		defer stateMu.Unlock()
		value := closeAfterRun
		closeAfterRun = false
		return value
	}
	startBuild := func() {
		stateMu.Lock()
		if running {
			stateMu.Unlock()
			return
		}
		stateMu.Unlock()
		stub, err := loadStub()
		if err != nil {
			walk.MsgBox(mw, "Builder error", err.Error(), walk.MsgBoxIconError)
			return
		}
		ctx, cancelBuild := context.WithCancel(context.Background())
		cancel = cancelBuild
		clientPath := strings.TrimSpace(clientEdit.Text())
		serverPath := strings.TrimSpace(serverEdit.Text())
		botPath := strings.TrimSpace(botEdit.Text())
		sqlPath := strings.TrimSpace(sqlEdit.Text())
		outputPath := strings.TrimSpace(outputEdit.Text())
		// Snapshot the credentials so a dialog opened later cannot change what
		// the running build is packaging.
		selectedAccounts := accounts
		setRunning(true)
		meter.SetProgress(0, "")
		stageLabel.SetText("Bắt đầu đóng gói")
		fileLabel.SetText("")
		console.Reset()
		console.Append("> Bắt đầu đóng gói lúc " + time.Now().Format("15:04:05"))
		relay := guiutil.NewRelay(uiRefreshInterval)
		go func() {
			result, buildErr := builder.Build(ctx, builder.Options{
				ClientPath: clientPath, ServerPath: serverPath, BotPath: botPath,
				SQLPath: sqlPath, OutputPath: outputPath,
				Accounts: selectedAccounts, SetupStub: stub,
				Progress: func(percent int, stage, detail string) {
					update, ok := relay.Next(percent, stage, detail)
					if !ok {
						return
					}
					mw.Synchronize(func() {
						stageName := builderStageVietnamese(update.Stage)
						// The percentage lives inside the meter and the file
						// name has its own full width row, so neither can
						// resize the other and force a relayout.
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
				},
			})
			mw.Synchronize(func() {
				setRunning(false)
				if takeCloseAfterRun() {
					mw.Close()
					return
				}
				if buildErr != nil {
					stageLabel.SetText("Đóng gói thất bại")
					fileLabel.SetText("")
					console.Append("")
					console.Append("!! LỖI: " + buildErr.Error())
					walk.MsgBox(mw, "Build failed", buildErr.Error(), walk.MsgBoxIconError)
					return
				}
				meter.SetProgress(100, "Hoàn tất")
				stageLabel.SetText("Hoàn tất")
				fileLabel.SetText("")
				console.Append("")
				console.Append("== Hoàn tất ==")
				console.Append("   Setup.exe : " + result.SetupPath)
				console.Append("   ISO       : " + result.ISOPath)
				message := "Setup.exe:\r\n" + result.SetupPath + "\r\n\r\nISO:\r\n" + result.ISOPath
				walk.MsgBox(mw, "Build completed", message, walk.MsgBoxIconInformation)
			})
		}()
	}

	caption := func(text string) Label {
		return Label{Text: text, TextColor: guiutil.ColorTextFaint, Font: Font{Family: guiutil.UIFontFamily, PointSize: 8, Bold: true}}
	}
	field := func(label string, edit **walk.LineEdit, button **walk.PushButton, hint string, onBrowse func()) []Widget {
		return []Widget{
			Label{Text: label, TextColor: guiutil.ColorText, MinSize: Size{Width: 132}},
			LineEdit{AssignTo: edit, CueBanner: hint, MinSize: Size{Height: 24}},
			PushButton{AssignTo: button, Text: "Duyệt…", MinSize: Size{Width: 90, Height: 26}, OnClicked: onBrowse},
		}
	}

	err := (MainWindow{
		AssignTo:   &mw,
		Title:      "Kiem The Deploy Forge x64",
		MinSize:    Size{Width: 900, Height: 740},
		Size:       Size{Width: 980, Height: 820},
		Font:       Font{Family: guiutil.UIFontFamily, PointSize: 9},
		Background: SolidColorBrush{Color: guiutil.ColorPage},
		Layout:     VBox{MarginsZero: true, SpacingZero: true},
		Children: []Widget{
			// Header band.
			Composite{
				Background: SolidColorBrush{Color: guiutil.ColorHeader},
				Layout:     VBox{Margins: guiutil.Margins(30, 20, 30, 20), Spacing: 4},
				Children: []Widget{
					Label{Text: "KIEM THE DEPLOY FORGE", TextColor: guiutil.ColorAccentText, Font: Font{Family: guiutil.UIFontFamily, PointSize: 20, Bold: true}},
					Label{Text: "Đóng gói Client + Server + Bot + MySQL 5.5.15 + jxaccount thành Setup.exe và ISO cài đặt ngoại tuyến.", TextColor: guiutil.ColorHeaderSub},
					Label{Text: install.AuthorCredit, TextColor: guiutil.ColorAccent, Font: Font{Family: guiutil.UIFontFamily, PointSize: 8, Bold: true}},
				},
			},
			// Scrollable body so the window stays usable on small screens.
			ScrollView{
				Background: SolidColorBrush{Color: guiutil.ColorPage},
				Layout:     VBox{Margins: guiutil.Margins(18, 16, 18, 16), Spacing: 12},
				Children: []Widget{
					// Card: input paths.
					Composite{
						Background: SolidColorBrush{Color: guiutil.ColorCard},
						Layout:     VBox{Margins: guiutil.Margins(20, 16, 20, 18), Spacing: 10},
						Children: []Widget{
							caption("NGUỒN ĐÓNG GÓI"),
							Composite{
								Layout: Grid{Columns: 3, Spacing: 8, MarginsZero: true},
								Children: slices.Concat(
									field("Thư mục Client", &clientEdit, &clientBrowseButton, `Thư mục chứa Game.exe và AutoPk\wjxtdAutoPro.exe`, browseFolder(&clientEdit, "Chọn thư mục Client")),
									field("Thư mục Server", &serverEdit, &serverBrowseButton, `Thư mục chứa Gameserver\GS1..GS9servercfg.ini`, browseFolder(&serverEdit, "Chọn thư mục Server")),
									field("Thư mục Bot", &botEdit, &botBrowseButton, "Tuỳ chọn — để trống nếu không đóng gói bot", browseFolder(&botEdit, "Chọn thư mục Bot")),
									field("File jxaccount.sql", &sqlEdit, &sqlBrowseButton, "File .sql phải có CREATE TABLE cho bảng account", browseSQL),
									field("Thư mục xuất bản", &outputEdit, &outputBrowseButton, "Ổ NTFS cần khoảng 2,2 lần payload + 2 GiB", browseFolder(&outputEdit, "Chọn thư mục xuất bản")),
								),
							},
						},
					},
					// Card: optional overrides, kept behind a button so the
					// common case stays a short form.
					Composite{
						Background: SolidColorBrush{Color: guiutil.ColorCard},
						Layout:     VBox{Margins: guiutil.Margins(20, 16, 20, 18), Spacing: 8},
						Children: []Widget{
							caption("CẤU HÌNH MỞ RỘNG"),
							Composite{
								Layout: HBox{MarginsZero: true, Spacing: 12},
								Children: []Widget{
									Label{AssignTo: &accountsLabel, Text: accountsSummary(accounts), TextColor: guiutil.ColorTextMuted, StretchFactor: 1},
									PushButton{AssignTo: &accountsButton, Text: "Tài khoản MySQL…", MinSize: Size{Width: 165, Height: 28}, OnClicked: editAccounts},
								},
							},
							Label{Text: fmt.Sprintf("MySQL %s  •  dịch vụ %s  •  127.0.0.1:3306  •  database %s", mysqlDisplayVersion, database.ServiceName, database.DatabaseName), TextColor: guiutil.ColorTextFaint},
							Label{Text: fmt.Sprintf("Setup tự nhận IP LAN và ghi %d khóa cấu hình; tạo shortcut Game và AutoPk ngoài Desktop.", len(configpatch.LANRules("127.0.0.1"))), TextColor: guiutil.ColorTextFaint},
						},
					},
					// Card: progress.
					Composite{
						Background: SolidColorBrush{Color: guiutil.ColorCard},
						Layout:     VBox{Margins: guiutil.Margins(20, 16, 20, 18), Spacing: 8},
						Children: []Widget{
							caption("TIẾN ĐỘ"),
							// Stage and file each own a full width row. Their
							// text changes cannot resize a neighbour, which is
							// what used to make the whole strip jitter.
							Label{AssignTo: &stageLabel, Text: "Sẵn sàng đóng gói", TextColor: guiutil.ColorText, Font: Font{Family: guiutil.UIFontFamily, PointSize: 11, Bold: true}, AlwaysConsumeSpace: true},
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
									PushButton{AssignTo: &buildButton, Text: "TẠO SETUP + ISO", OnClicked: startBuild, MinSize: Size{Width: 190, Height: 34}, Font: Font{Family: guiutil.UIFontFamily, PointSize: 10, Bold: true}},
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
	mw.Run()
}

func builderStageVietnamese(stage string) string {
	translations := map[string]string{
		"Scan and hash":          "Quét và kiểm tra dữ liệu",
		"MySQL package":          "Gói MySQL",
		"Disk preflight":         "Kiểm tra dung lượng ổ đĩa",
		"Build payload package":  "Tạo gói dữ liệu ISO",
		"Build Setup bootstrap":  "Tạo Setup.exe nhỏ",
		"Verify payload package": "Kiểm tra gói dữ liệu",
		"Build ISO UDF":          "Tạo ISO UDF",
		"Complete":               "Hoàn tất",
	}
	if translated, exists := translations[stage]; exists {
		return translated
	}
	return stage
}

func loadStub() ([]byte, error) {
	stub, err := assets.ReadFile("assets/SetupStub.exe")
	if err != nil {
		return nil, fmt.Errorf("Setup GUI stub is missing; run scripts/Build-Tools.ps1: %w", err)
	}
	if len(stub) < 1024*1024 || string(stub[:2]) != "MZ" {
		return nil, fmt.Errorf("Setup GUI stub is invalid")
	}
	return stub, nil
}
