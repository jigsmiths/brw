// Package progress draws the progress of bottle downloads, as brew does: a line
// for each with a bar, its state, the bytes so far and the speed.
package progress

import (
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/jigsmiths/brw/internal/cellar"
)

const (
	barWidth = 30
	redraw   = 100 * time.Millisecond
)

var spinner = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

type item struct {
	label       string
	done, total int64
	start       time.Time
	finished    bool
}

// Display shows downloads as they happen. On a terminal it redraws one line per
// download in place; elsewhere it only says when each one has finished.
// It is safe for concurrent use.
type Display struct {
	// Now is time.Now unless a test sets it.
	Now func() time.Time

	w   io.Writer
	tty bool

	mu     sync.Mutex
	items  []*item
	drawn  int // lines on screen
	last   time.Time
	frames int
}

func New(w io.Writer, tty bool) *Display {
	return &Display{Now: time.Now, w: w, tty: tty}
}

// Update records that done of total bytes (-1 if unknown) of a download are in.
func (d *Display) Update(label string, done, total int64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	now := d.Now()
	it := d.find(label)
	if it == nil {
		it = &item{label: label, start: now}
		d.items = append(d.items, it)
	}
	it.done, it.total = done, total
	justFinished := total >= 0 && done >= total && !it.finished
	if justFinished {
		it.finished = true
	}
	if !d.tty {
		if justFinished {
			_, _ = fmt.Fprintf(d.w, "Downloaded Bottle %s (%s)\n", label, cellar.ReadableSize(done))
		}
		return
	}
	if justFinished || now.Sub(d.last) >= redraw {
		d.draw(now)
	}
}

// Close draws the final state.
func (d *Display) Close() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.tty && len(d.items) > 0 {
		d.draw(d.Now())
	}
}

func (d *Display) find(label string) *item {
	for _, it := range d.items {
		if it.label == label {
			return it
		}
	}
	return nil
}

func (d *Display) draw(now time.Time) {
	d.last = now
	d.frames++
	var b strings.Builder
	if d.drawn > 0 {
		fmt.Fprintf(&b, "\x1b[%dA", d.drawn)
	}
	width := 0
	for _, it := range d.items {
		width = max(width, len(it.label))
	}
	for _, it := range d.items {
		b.WriteString("\r\x1b[2K")
		b.WriteString(d.line(it, width, now))
		b.WriteByte('\n')
	}
	d.drawn = len(d.items)
	_, _ = io.WriteString(d.w, b.String())
}

func (d *Display) line(it *item, width int, now time.Time) string {
	icon, state := spinner[d.frames%len(spinner)], "Downloading"
	if it.finished {
		icon, state = "✔", "Downloaded"
	}
	bar := strings.Repeat(" ", barWidth)
	size := fmt.Sprintf("%6s", cellar.ReadableSize(it.done))
	if it.total >= 0 {
		filled := barWidth
		if it.total > 0 {
			filled = int(int64(barWidth) * min(it.done, it.total) / it.total)
		}
		bar = strings.Repeat("#", filled) + strings.Repeat(" ", barWidth-filled)
		size += fmt.Sprintf("/%6s", cellar.ReadableSize(it.total))
	}
	speed := ""
	if secs := now.Sub(it.start).Seconds(); !it.finished && secs > 0 {
		speed = "  " + cellar.ReadableSize(int64(float64(it.done)/secs)) + "/s"
	}
	return fmt.Sprintf("%s Bottle %-*s  %s  %-11s %s%s", icon, width, it.label, bar, state, size, speed)
}
