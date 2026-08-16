package guiutil

import (
	"fmt"

	"github.com/lxn/walk"
	"github.com/lxn/walk/declarative"
)

// MeterHeight is the drawn height of the progress meter in 1/96" units.
const MeterHeight = 26

// Meter is a progress bar that draws its own percentage inside the track.
//
// Two problems with the stock ProgressBar plus a neighbouring percentage Label
// made the old window flicker. The label's width changed with every value, so
// the surrounding layout was recomputed on each update, and the repaint was
// single buffered. Owning the drawing solves both: the widget never changes
// size, and PaintBuffered composes off-screen before blitting.
type Meter struct {
	widget  *walk.CustomWidget
	percent int
	caption string
	font    *walk.Font
}

// NewMeter returns the declarative widget plus the handle used to update it.
// The returned Meter is only safe to touch from the UI thread.
func NewMeter() (*Meter, declarative.CustomWidget) {
	meter := &Meter{}
	return meter, declarative.CustomWidget{
		AssignTo:            &meter.widget,
		MinSize:             declarative.Size{Height: MeterHeight},
		MaxSize:             declarative.Size{Height: MeterHeight},
		PaintMode:           declarative.PaintBuffered,
		InvalidatesOnResize: true,
		// PaintPixels rather than Paint: the 1/96" callback rounds the update
		// bounds and can leave a thin unpainted edge on scaled displays.
		PaintPixels: meter.paint,
	}
}

// SetProgress updates the meter and repaints only when something changed, so a
// build that reports thousands of times per file does not force thousands of
// redraws.
func (m *Meter) SetProgress(percent int, caption string) {
	if percent < 0 {
		percent = 0
	}
	if percent > 100 {
		percent = 100
	}
	if m.percent == percent && m.caption == caption {
		return
	}
	m.percent = percent
	m.caption = caption
	if m.widget != nil {
		m.widget.Invalidate()
	}
}

// Percent reports the value currently drawn.
func (m *Meter) Percent() int {
	return m.percent
}

func (m *Meter) paint(canvas *walk.Canvas, _ walk.Rectangle) error {
	bounds := m.widget.ClientBoundsPixels()
	if bounds.Width <= 0 || bounds.Height <= 0 {
		return nil
	}
	radius := walk.Size{Width: bounds.Height, Height: bounds.Height}

	track, err := walk.NewSolidColorBrush(ColorTrack)
	if err != nil {
		return err
	}
	defer track.Dispose()
	if err := canvas.FillRoundedRectanglePixels(track, bounds, radius); err != nil {
		return err
	}

	fillWidth := bounds.Width * m.percent / 100
	// A rounded rectangle narrower than its own corner diameter degenerates
	// into a sliver, so hold the fill at the minimum pill width instead.
	if m.percent > 0 && fillWidth < bounds.Height {
		fillWidth = bounds.Height
	}
	if fillWidth > 0 {
		fill, err := walk.NewSolidColorBrush(ColorAccent)
		if err != nil {
			return err
		}
		defer fill.Dispose()
		filled := walk.Rectangle{X: bounds.X, Y: bounds.Y, Width: fillWidth, Height: bounds.Height}
		if err := canvas.FillRoundedRectanglePixels(fill, filled, radius); err != nil {
			return err
		}
	}

	if m.font == nil {
		font, err := walk.NewFont(UIFontFamily, 9, walk.FontBold)
		if err != nil {
			return err
		}
		m.font = font
	}
	label := fmt.Sprintf("%d%%", m.percent)
	if m.caption != "" {
		label = fmt.Sprintf("%d%%  ·  %s", m.percent, m.caption)
	}
	// The caption sits on the fill once the bar passes the middle, so the text
	// colour follows the surface underneath it rather than fighting for
	// contrast with a fixed colour.
	textColor := ColorText
	if fillWidth*2 >= bounds.Width {
		textColor = walk.RGB(0xFF, 0xFF, 0xFF)
	}
	return canvas.DrawTextPixels(label, m.font, textColor, bounds,
		walk.TextCenter|walk.TextVCenter|walk.TextSingleLine|walk.TextEndEllipsis)
}
