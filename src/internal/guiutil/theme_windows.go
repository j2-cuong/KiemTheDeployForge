package guiutil

import "github.com/lxn/walk"

// The palette is intentionally small so the Builder and the Setup GUI read as
// one product. Colours are picked for contrast against the two surfaces the
// windows actually use: the light page and the dark console.
var (
	ColorPage        = walk.RGB(0xEF, 0xF1, 0xF4) // window background behind the cards
	ColorCard        = walk.RGB(0xFF, 0xFF, 0xFF) // raised content surface
	ColorHeader      = walk.RGB(0x0E, 0x2A, 0x33) // deep teal header band
	ColorHeaderSub   = walk.RGB(0xB6, 0xCB, 0xD1) // secondary text on the header
	ColorAccent      = walk.RGB(0x11, 0x7B, 0x88) // primary accent, progress fill
	ColorAccentText  = walk.RGB(0xE8, 0xB4, 0x4A) // gold used for the product name
	ColorText        = walk.RGB(0x1B, 0x24, 0x28) // primary body text
	ColorTextMuted   = walk.RGB(0x64, 0x72, 0x78) // labels and hints
	ColorTextFaint   = walk.RGB(0x8A, 0x97, 0x9D) // section captions
	ColorTrack       = walk.RGB(0xDD, 0xE3, 0xE7) // progress bar track
	ColorConsoleBack = walk.RGB(0x0B, 0x12, 0x18) // console surface
	ColorConsoleText = walk.RGB(0xC8, 0xD6, 0xDC) // console text
	ColorOk          = walk.RGB(0x1E, 0x7A, 0x5A)
	ColorWarn        = walk.RGB(0xB0, 0x3A, 0x20)
)

// ConsoleFontFamily is the monospace family used for the scrolling log so the
// detail lines line up like a terminal instead of jittering with proportional
// glyph widths.
const ConsoleFontFamily = "Consolas"

// UIFontFamily is the proportional family used for everything else.
const UIFontFamily = "Segoe UI"
