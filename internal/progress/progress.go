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
	minBar   = 10
	minLabel = 12
	// noBar and compact are barW values: the line without its bar, and the icon
	// and label alone, for a terminal too narrow for anything more.
	noBar, compact = 0, -1
	minShortLabel  = 4
	redraw         = 100 * time.Millisecond
	// speedWindow is how far back the download speed looks, so that it follows
	// the connection rather than the average of the whole download.
	speedWindow = 3 * time.Second
)

var spinner = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

type sample struct {
	at   time.Time
	done int64
}

type item struct {
	label       string
	done, total int64
	samples     []sample
	finished    bool
}

// Display shows downloads as they happen. On a terminal it redraws one line per
// download in place; elsewhere it only says when each one has finished.
// It is safe for concurrent use.
type Display struct {
	// Now is time.Now unless a test sets it.
	Now func() time.Time
	// Width says how many columns the terminal has, so lines can be cut to fit.
	// Nil means no limit.
	Width func() int

	w   io.Writer
	tty bool

	mu     sync.Mutex
	items  []*item
	drawn  int // lines on screen
	last   time.Time
	frames int
	stale  bool // updates not yet on screen
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
		it = &item{label: label}
		d.items = append(d.items, it)
	}
	it.done, it.total = done, total
	d.stale = true
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

// Close draws the final state, unless it is already on screen: by now other
// output may have followed it, and drawing again would repeat the lines.
func (d *Display) Close() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.tty && d.stale {
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
	d.stale = false
	d.frames++
	for _, it := range d.items {
		if !it.finished {
			it.sample(now)
		}
	}
	var b strings.Builder
	if d.drawn > 0 {
		fmt.Fprintf(&b, "\x1b[%dA", d.drawn)
	}
	for _, line := range d.lines(now) {
		b.WriteString("\r\x1b[2K")
		b.WriteString(line)
		b.WriteByte('\n')
	}
	d.drawn = len(d.items)
	_, _ = io.WriteString(d.w, b.String())
}

// sample notes how far the download is, and forgets what is older than the
// window the speed is measured over.
func (it *item) sample(now time.Time) {
	it.samples = append(it.samples, sample{now, it.done})
	for len(it.samples) > 1 && now.Sub(it.samples[0].at) > speedWindow {
		it.samples = it.samples[1:]
	}
}

// speed is bytes a second over the window, or false before there is one.
func (it *item) speed(now time.Time) (int64, bool) {
	if len(it.samples) == 0 {
		return 0, false
	}
	first := it.samples[0]
	secs := now.Sub(first.at).Seconds()
	if secs <= 0 {
		return 0, false
	}
	return int64(float64(it.done-first.done) / secs), true
}

// lines renders every item with the same column widths. They shrink to fit the
// terminal, as a line that wraps would break redrawing in place: first the bar,
// then the label, then the speed goes.
func (d *Display) lines(now time.Time) []string {
	labelW := 0
	for _, it := range d.items {
		labelW = max(labelW, len([]rune(it.label)))
	}
	render := func(labelW, barW int, speed bool) (lines []string, widest int) {
		for _, it := range d.items {
			l := d.line(it, labelW, barW, speed, now)
			lines = append(lines, l)
			widest = max(widest, len([]rune(l)))
		}
		return lines, widest
	}
	avail := 0
	if d.Width != nil {
		avail = d.Width() - 1 // the last column would wrap
	}
	if avail <= 0 {
		lines, _ := render(labelW, barWidth, true)
		return lines
	}
	try := func(labelW, barW int, speed bool) []string {
		if lines, widest := render(labelW, barW, speed); widest <= avail {
			return lines
		}
		return nil
	}
	for _, speed := range []bool{true, false} {
		for bar := barWidth; bar >= minBar; bar-- {
			if lines := try(labelW, bar, speed); lines != nil {
				return lines
			}
		}
		for w := labelW - 1; w >= min(labelW, minLabel); w-- {
			if lines := try(w, minBar, speed); lines != nil {
				return lines
			}
		}
	}
	for w := min(labelW, minLabel); w >= minShortLabel; w-- {
		if lines := try(w, noBar, false); lines != nil {
			return lines
		}
	}
	lines, _ := render(max(avail-2, 1), compact, false)
	return lines
}

func (d *Display) line(it *item, labelW, barW int, withSpeed bool, now time.Time) string {
	icon, state := spinner[d.frames%len(spinner)], "Downloading"
	if it.finished {
		icon, state = "✔", "Downloaded"
	}
	if barW == compact {
		return icon + " " + fit(it.label, labelW)
	}
	bar := strings.Repeat(" ", barW)
	size := fmt.Sprintf("%6s", cellar.ReadableSize(it.done))
	if it.total >= 0 {
		filled := barW
		if it.total > 0 {
			filled = int(int64(barW) * min(it.done, it.total) / it.total)
		}
		bar = strings.Repeat("#", filled) + strings.Repeat(" ", barW-filled)
		size += fmt.Sprintf("/%6s", cellar.ReadableSize(it.total))
	}
	speed := ""
	if withSpeed && !it.finished {
		if v, ok := it.speed(now); ok {
			speed = fmt.Sprintf("  %6s/s", cellar.ReadableSize(v))
		}
	}
	if barW != noBar {
		bar += "  "
	}
	return fmt.Sprintf("%s Bottle %s  %s%-11s %s%s", icon, fit(it.label, labelW), bar, state, size, speed)
}

// fit pads or cuts the label to w columns, ending a cut one in "…".
func fit(label string, w int) string {
	r := []rune(label)
	if len(r) > w {
		return string(r[:max(w-1, 0)]) + "…"
	}
	return label + strings.Repeat(" ", w-len(r))
}
