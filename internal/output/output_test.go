package output

import (
	"bytes"
	"strings"
	"testing"
)

func TestTruncateEnd(t *testing.T) {
	colored := "\x1b[31m✗\x1b[0m " + strings.Repeat("x", 50)
	got := truncateEnd(colored, 20)
	if visible := len([]rune(ansiRE.ReplaceAllString(got, ""))); visible != 20 {
		t.Fatalf("visible width = %d (%q)", visible, got)
	}
	if !strings.HasPrefix(got, "\x1b[31m✗\x1b[0m") {
		t.Fatal("color codes must survive")
	}
	if truncateEnd("short", 20) != "short" {
		t.Fatal("short strings are unchanged")
	}
	if oneLine("a\n  b\tc") != "a b c" {
		t.Fatal("oneLine")
	}
}

func TestProgressCloseRestoresCursor(t *testing.T) {
	var buf bytes.Buffer
	p := &Progress{w: &buf, tty: true}
	i := p.Add("Generating")
	p.Start(i)
	p.Done(i, "failed: "+strings.Repeat("long error ", 30))
	p.Close()
	out := buf.String()
	if strings.LastIndex(out, "\x1b[?25h") < strings.LastIndex(out, "\x1b[?25l") {
		t.Fatal("cursor must be shown again after Close")
	}
	if !strings.Contains(out, strings.Repeat("long error ", 30)) {
		t.Fatal("full error text must be printed on Close")
	}
}

func TestTargetAndSuffix(t *testing.T) {
	if got := Target("", "resp/1", "image/png", ""); got != "resp-1.png" {
		t.Fatalf("got %s", got)
	}
	if got := Target("out.png", "x", "image/png", "1-2"); got != "out-1-2.png" {
		t.Fatalf("got %s", got)
	}
	if got := Target("dir/", "x", "image/webp", "2-1"); got != "dir/x-2-1.webp" {
		t.Fatalf("got %s", got)
	}
}
