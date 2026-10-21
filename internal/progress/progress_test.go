package progress

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func newDisplay(tty bool) (*Display, *bytes.Buffer, *clock) {
	var buf bytes.Buffer
	c := &clock{t: time.Unix(1000, 0)}
	d := New(&buf, tty)
	d.Now = c.now
	return d, &buf, c
}

func TestTerminalShowsBarStateSizesAndSpeed(t *testing.T) {
	d, buf, c := newDisplay(true)
	d.Update("jq (1.8.2)", 0, 10_000_000)
	c.t = c.t.Add(time.Second)
	d.Update("jq (1.8.2)", 5_000_000, 10_000_000)
	got := buf.String()
	for _, want := range []string{"Bottle jq (1.8.2)", "Downloading", "5MB/", "10MB", "5MB/s", strings.Repeat("#", barWidth/2) + strings.Repeat(" ", barWidth/2)} {
		if !strings.Contains(got, want) {
			t.Errorf("output lacks %q:\n%q", want, got)
		}
	}
	if !strings.Contains(got, "\x1b[1A") {
		t.Errorf("a second draw should move the cursor up over the first:\n%q", got)
	}
}

func TestTerminalMarksFinishedBottlesAndKeepsOthersLive(t *testing.T) {
	d, buf, c := newDisplay(true)
	d.Update("a (1)", 10, 10)
	d.Update("b (2)", 1, 10)
	c.t = c.t.Add(time.Second)
	d.Close()
	got := buf.String()
	if !strings.Contains(got, "✔ Bottle a (1)") || !strings.Contains(got, "Downloaded") {
		t.Errorf("finished bottle not marked:\n%q", got)
	}
	if !strings.Contains(got, "Downloading") {
		t.Errorf("unfinished bottle should still read Downloading:\n%q", got)
	}
}

func TestRedrawsAreThrottled(t *testing.T) {
	d, buf, _ := newDisplay(true)
	d.Update("a (1)", 1, 100)
	n := buf.Len()
	d.Update("a (1)", 2, 100)
	if buf.Len() != n {
		t.Error("a second update at the same instant redrew")
	}
	d.Update("a (1)", 100, 100)
	if buf.Len() == n {
		t.Error("finishing a download must draw at once")
	}
}

func TestWithoutATerminalOnlyFinishedBottlesAreReported(t *testing.T) {
	d, buf, _ := newDisplay(false)
	d.Update("jq (1.8.2)", 1, 10)
	if buf.Len() != 0 {
		t.Errorf("wrote %q before finishing", buf.String())
	}
	d.Update("jq (1.8.2)", 10, 10)
	d.Update("jq (1.8.2)", 10, 10)
	d.Close()
	if got, want := buf.String(), "Downloaded Bottle jq (1.8.2) (10B)\n"; got != want {
		t.Errorf("output = %q, want %q", got, want)
	}
	if strings.Contains(buf.String(), "\x1b") {
		t.Error("escape codes written to something that is not a terminal")
	}
}

func TestUnknownTotalShowsNoBar(t *testing.T) {
	d, buf, _ := newDisplay(true)
	d.Update("a (1)", 5, -1)
	if strings.Contains(buf.String(), "#") {
		t.Errorf("bar drawn without a total: %q", buf.String())
	}
}
