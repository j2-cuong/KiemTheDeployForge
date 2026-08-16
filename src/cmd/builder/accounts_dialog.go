package main

import (
	"fmt"
	"strings"

	"github.com/lxn/walk"
	. "github.com/lxn/walk/declarative"

	"kiemthedeployforge/internal/database"
	"kiemthedeployforge/internal/guiutil"
	"kiemthedeployforge/internal/release"
)

// accountsSummary is the one line description shown next to the button.
func accountsSummary(accounts release.Accounts) string {
	accounts = accounts.WithDefaults()
	summary := fmt.Sprintf("Tài khoản MySQL: %s / %s  và  %s / %s  (toàn quyền trên %s)",
		database.RootUser, accounts.RootPassword, accounts.BotUser, accounts.BotPassword, database.DatabaseName)
	if accounts == release.DefaultAccounts() {
		return summary + "  — mặc định"
	}
	return summary + "  — đã tuỳ chỉnh"
}

// showAccountsDialog edits the MySQL credentials baked into the release. It
// returns the accepted accounts, or ok=false when the operator cancels.
//
// The values are validated before the dialog closes, because a rejected
// credential discovered during packaging would waste a full build.
func showAccountsDialog(owner walk.Form, current release.Accounts) (release.Accounts, bool) {
	current = current.WithDefaults()
	var dialog *walk.Dialog
	var rootEdit, botUserEdit, botPasswordEdit *walk.LineEdit
	var errorLabel *walk.Label
	var acceptButton, cancelButton, resetButton *walk.PushButton
	result := current
	accepted := false

	read := func() release.Accounts {
		return release.Accounts{
			RootPassword: strings.TrimSpace(rootEdit.Text()),
			BotUser:      strings.TrimSpace(botUserEdit.Text()),
			BotPassword:  strings.TrimSpace(botPasswordEdit.Text()),
		}
	}
	write := func(accounts release.Accounts) {
		rootEdit.SetText(accounts.RootPassword)
		botUserEdit.SetText(accounts.BotUser)
		botPasswordEdit.SetText(accounts.BotPassword)
	}

	err := (Dialog{
		AssignTo:      &dialog,
		Title:         "Tài khoản MySQL của bản phát hành",
		DefaultButton: &acceptButton,
		CancelButton:  &cancelButton,
		MinSize:       Size{Width: 560, Height: 330},
		Font:          Font{Family: guiutil.UIFontFamily, PointSize: 9},
		Background:    SolidColorBrush{Color: guiutil.ColorCard},
		Layout:        VBox{Margins: guiutil.Margins(22, 20, 22, 18), Spacing: 12},
		Children: []Widget{
			Label{Text: "Giá trị này được ghi vào bản phát hành. Setup tạo đúng các tài khoản này trên máy đích, và ghi luôn vào file cấu hình của bot.",
				TextColor: guiutil.ColorTextMuted},
			Composite{
				Layout: Grid{Columns: 2, Spacing: 8, MarginsZero: true},
				Children: []Widget{
					Label{Text: "Mật khẩu " + database.RootUser, MinSize: Size{Width: 150}},
					LineEdit{AssignTo: &rootEdit, Text: current.RootPassword, MinSize: Size{Height: 24}},

					Label{Text: "User cho bot"},
					LineEdit{AssignTo: &botUserEdit, Text: current.BotUser, MinSize: Size{Height: 24}},

					Label{Text: "Mật khẩu bot"},
					LineEdit{AssignTo: &botPasswordEdit, Text: current.BotPassword, MinSize: Size{Height: 24}},
				},
			},
			Label{Text: "Tên user " + database.RootUser + " không đổi được; chỉ đổi mật khẩu. Mật khẩu tối đa 32 ký tự, user bot tối đa 16 ký tự. " +
				"Không dùng dấu nháy, dấu gạch chéo ngược hay khoảng trắng — MySQL 5.5 coi gạch chéo ngược là ký tự escape.",
				TextColor: guiutil.ColorTextFaint},
			Label{AssignTo: &errorLabel, Text: "", TextColor: guiutil.ColorWarn, AlwaysConsumeSpace: true},
			Composite{
				Layout: HBox{MarginsZero: true, Spacing: 8},
				Children: []Widget{
					PushButton{AssignTo: &resetButton, Text: "Về mặc định", MinSize: Size{Width: 120, Height: 30}, OnClicked: func() {
						write(release.DefaultAccounts())
						errorLabel.SetText("")
					}},
					HSpacer{},
					PushButton{AssignTo: &cancelButton, Text: "Huỷ", MinSize: Size{Width: 100, Height: 30}, OnClicked: func() {
						dialog.Cancel()
					}},
					PushButton{AssignTo: &acceptButton, Text: "Lưu", MinSize: Size{Width: 100, Height: 30}, OnClicked: func() {
						candidate := read().WithDefaults()
						if err := candidate.Validate(); err != nil {
							errorLabel.SetText(err.Error())
							return
						}
						result = candidate
						accepted = true
						dialog.Accept()
					}},
				},
			},
		},
	}).Create(owner)
	if err != nil {
		walk.MsgBox(owner, "Dialog error", err.Error(), walk.MsgBoxIconError)
		return current, false
	}
	dialog.Run()
	return result, accepted
}
