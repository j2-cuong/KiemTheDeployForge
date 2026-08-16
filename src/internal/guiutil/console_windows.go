package guiutil

import (
	"strings"
	"time"

	"github.com/lxn/walk"
	"github.com/lxn/walk/declarative"
)

// consoleMaxLines is the scrollback kept in the view. Trimming rewrites the
// whole control, so the limit is high enough that a normal build never hits it.
const consoleMaxLines = 4000

// consoleKeepLines is what survives a trim.
const consoleKeepLines = 2000

// Console is an append-only log rendered like a terminal window.
type Console struct {
	view  *walk.TextEdit
	lines []string
}

// NewConsole returns the declarative widget plus the handle used to write to
// it. The returned Console is only safe to touch from the UI thread.
func NewConsole(minHeight int) (*Console, declarative.TextEdit) {
	console := &Console{}
	return console, declarative.TextEdit{
		AssignTo:   &console.view,
		ReadOnly:   true,
		VScroll:    true,
		MinSize:    declarative.Size{Height: minHeight},
		Background: declarative.SolidColorBrush{Color: ColorConsoleBack},
		TextColor:  ColorConsoleText,
		Font:       declarative.Font{Family: ConsoleFontFamily, PointSize: 9},
	}
}

// Reset clears the scrollback.
func (c *Console) Reset() {
	c.lines = c.lines[:0]
	if c.view != nil {
		_ = c.view.SetText("")
	}
}

// Append adds one line and keeps the newest output visible.
func (c *Console) Append(line string) {
	if c.view == nil {
		return
	}
	c.lines = append(c.lines, line)
	if len(c.lines) > consoleMaxLines {
		c.lines = append(c.lines[:0], c.lines[len(c.lines)-consoleKeepLines:]...)
		_ = c.view.SetText(strings.Join(c.lines, "\r\n") + "\r\n")
	} else {
		c.view.AppendText(line + "\r\n")
	}
	c.view.ScrollToCaret()
}

// Relay decides which progress reports actually reach the window.
//
// The packaging pipeline reports once per I/O block, which is thousands of
// times per second on a large payload. Forwarding all of them made the status
// text and the percentage repaint continuously, which is what produced the
// strobing. Relay collapses that stream into at most one update per interval,
// while never dropping a stage change or the final report.
type Relay struct {
	interval    time.Duration
	lastForward time.Time
	lastPercent int
	lastStage   string
	lastDetail  string
	started     bool
}

// Update is a progress report that survived throttling.
type Update struct {
	Percent int
	Stage   string
	Detail  string
	// StageChanged marks the first report of a new stage, which the caller
	// logs as a heading rather than as another detail line.
	StageChanged bool
	// DetailChanged is false when only the percentage moved, so the caller can
	// leave the file name label untouched instead of repainting it.
	DetailChanged bool
}

// NewRelay returns a relay that forwards at most one update per interval.
func NewRelay(interval time.Duration) *Relay {
	return &Relay{interval: interval}
}

// Next reports whether this progress callback should reach the UI, and what
// changed since the last one that did. It must be called from a single
// goroutine, which is how the build and install pipelines report.
func (r *Relay) Next(percent int, stage, detail string) (Update, bool) {
	stageChanged := !r.started || stage != r.lastStage
	detailChanged := !r.started || detail != r.lastDetail
	now := time.Now()
	// A stage change and the final report are structural, so they are never
	// dropped no matter how recently the window was updated.
	forced := stageChanged || percent >= 100
	if !forced {
		if percent == r.lastPercent && !detailChanged {
			return Update{}, false
		}
		if now.Sub(r.lastForward) < r.interval {
			return Update{}, false
		}
	}
	update := Update{
		Percent: percent, Stage: stage, Detail: detail,
		StageChanged: stageChanged, DetailChanged: detailChanged,
	}
	r.started = true
	r.lastForward = now
	r.lastPercent = percent
	r.lastStage = stage
	r.lastDetail = detail
	return update, true
}
