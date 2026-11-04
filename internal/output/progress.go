package output

import (
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// ColorEnabled follows NO_COLOR and FORCE_COLOR.
func ColorEnabled() bool {
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	if v := os.Getenv("FORCE_COLOR"); v != "" && v != "0" {
		return true
	}
	return IsTTY(os.Stderr)
}

// FormatElapsed renders 12s or 1m 5s.
func FormatElapsed(d time.Duration) string {
	secs := int(d.Seconds())
	if secs < 60 {
		return fmt.Sprintf("%ds", secs)
	}
	return fmt.Sprintf("%dm %ds", secs/60, secs%60)
}

type lineState int

const (
	queued lineState = iota
	active
	done
)

type line struct {
	text  string
	final string
	state lineState
	start time.Time
	pos   int
}

// Progress renders shimmering status lines on stderr.
type Progress struct {
	mu       sync.Mutex
	w        io.Writer
	quiet    bool
	tty      bool
	color    bool
	lines    []*line
	rendered int
	stop     chan struct{}
	stopped  bool
}

// NewProgress creates a progress display. Nothing is drawn when quiet.
func NewProgress(quiet bool) *Progress {
	return &Progress{w: os.Stderr, quiet: quiet, tty: IsTTY(os.Stderr), color: ColorEnabled()}
}

// Add registers a queued line and returns its index.
func (p *Progress) Add(text string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.lines = append(p.lines, &line{text: text, pos: -shimmerPad})
	if !p.quiet && p.tty && p.stop == nil && !p.stopped {
		p.stop = make(chan struct{})
		fmt.Fprint(p.w, "\x1b[?25l")
		go p.loop()
	}
	return len(p.lines) - 1
}

// Start marks a line active.
func (p *Progress) Start(i int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if l := p.lines[i]; l.state == queued {
		l.state = active
		l.start = time.Now()
	}
	if !p.quiet && !p.tty {
		fmt.Fprintln(p.w, p.lines[i].text)
	}
}

// Done completes a line with its final text.
func (p *Progress) Done(i int, final string) {
	p.mu.Lock()
	l := p.lines[i]
	l.state = done
	l.final = final
	if !p.quiet && !p.tty {
		fmt.Fprintln(p.w, final)
	}
	p.mu.Unlock()
}

// Elapsed returns time since a line started.
func (p *Progress) Elapsed(i int) time.Duration {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.lines[i].start.IsZero() {
		return 0
	}
	return time.Since(p.lines[i].start)
}

// Close stops rendering and leaves final lines on screen.
func (p *Progress) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stopped {
		return
	}
	p.stopped = true
	if p.stop != nil {
		close(p.stop)
		// The live region only holds single-row lines; print the full final
		// text once, now that nothing redraws over it.
		p.erase()
		var b strings.Builder
		for _, l := range p.lines {
			text := l.final
			if l.state != done {
				text = l.text
			}
			b.WriteString(text + "\x1b[K\n")
		}
		b.WriteString("\x1b[J\x1b[?25h")
		fmt.Fprint(p.w, b.String())
		p.rendered = 0
	}
}

// Println prints a message above the progress lines.
func (p *Progress) Println(msg string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stop != nil && !p.stopped {
		p.erase()
		fmt.Fprintln(p.w, msg)
		p.rendered = 0
		p.render()
		return
	}
	fmt.Fprintln(p.w, msg)
}

func (p *Progress) loop() {
	t := time.NewTicker(60 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-p.stop:
			return
		case <-t.C:
			p.mu.Lock()
			if !p.stopped {
				p.render()
			}
			p.mu.Unlock()
		}
	}
}

func (p *Progress) erase() {
	if p.rendered > 0 {
		fmt.Fprintf(p.w, "\x1b[%dA\r", p.rendered)
	}
}

func (p *Progress) render() {
	p.erase()
	cols, _ := terminalSize()
	if cols <= 0 {
		cols = 80
	}
	var b strings.Builder
	for _, l := range p.lines {
		switch l.state {
		case done:
			// Keep every live row to one screen line so erase() stays exact.
			b.WriteString(truncateEnd(oneLine(l.final), cols-1) + "\x1b[K\n")
		case queued:
			text := truncate(l.text+" (queued)", cols-1)
			if p.color {
				b.WriteString("\x1b[2m\x1b[90m" + text + "\x1b[0m\x1b[K\n")
			} else {
				b.WriteString(text + "\x1b[K\n")
			}
		case active:
			text := truncate(fmt.Sprintf("%s (%s)", l.text, FormatElapsed(time.Since(l.start))), cols-1)
			if p.color {
				b.WriteString(shimmer(text, l.pos) + "\x1b[K\n")
			} else {
				b.WriteString(text + "\x1b[K\n")
			}
			l.pos++
			if l.pos > utf8.RuneCountInString(text)+shimmerPad {
				l.pos = -shimmerPad
			}
		}
	}
	fmt.Fprint(p.w, b.String())
	p.rendered = len(p.lines)
}

func truncate(s string, width int) string {
	if width <= 0 || utf8.RuneCountInString(s) <= width {
		return s
	}
	r := []rune(s)
	return "…" + string(r[len(r)-width+1:])
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

var ansiRE = regexp.MustCompile(`\x1b\[[0-9;?]*[A-Za-z]`)

// truncateEnd cuts text to width visible columns, keeping ANSI color codes.
func truncateEnd(s string, width int) string {
	if width <= 0 || utf8.RuneCountInString(ansiRE.ReplaceAllString(s, "")) <= width {
		return s
	}
	var b strings.Builder
	visible := 0
	for i := 0; i < len(s); {
		if loc := ansiRE.FindStringIndex(s[i:]); loc != nil && loc[0] == 0 {
			b.WriteString(s[i : i+loc[1]])
			i += loc[1]
			continue
		}
		if visible == width-1 {
			b.WriteString("…\x1b[0m")
			break
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		b.WriteRune(r)
		visible++
		i += size
	}
	return b.String()
}

const shimmerPad = 8

// shimmer highlights a moving window of characters.
func shimmer(text string, pos int) string {
	var b strings.Builder
	for i, r := range []rune(text) {
		dist := i - pos
		if dist < 0 {
			dist = -dist
		}
		var color int
		switch {
		case dist <= 1:
			color = 255
		case dist <= 3:
			color = 250
		case dist <= 5:
			color = 246
		default:
			color = 242
		}
		fmt.Fprintf(&b, "\x1b[38;5;%dm%c", color, r)
	}
	b.WriteString("\x1b[0m")
	return b.String()
}
