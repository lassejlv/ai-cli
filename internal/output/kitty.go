package output

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	"image/png"
	"io"
	"os"
	"strconv"
	"strings"

	_ "golang.org/x/image/bmp"
	_ "golang.org/x/image/tiff"
	_ "golang.org/x/image/webp"

	"github.com/lassejlv/ai-cli/internal/media"
)

const kittyChunk = 4096

// SupportsKitty reports whether the terminal understands the Kitty graphics
// protocol. AI_CLI_PREVIEW=1 forces it on and AI_CLI_PREVIEW=0 off.
func SupportsKitty() bool {
	switch os.Getenv("AI_CLI_PREVIEW") {
	case "0":
		return false
	case "1":
		return true
	}
	if os.Getenv("TERM") == "xterm-kitty" || os.Getenv("KITTY_WINDOW_ID") != "" || os.Getenv("GHOSTTY_RESOURCES_DIR") != "" {
		return true
	}
	switch strings.ToLower(os.Getenv("TERM_PROGRAM")) {
	case "kitty", "ghostty", "wezterm", "warpterminal", "konsole":
		return true
	}
	return strings.EqualFold(os.Getenv("LC_TERMINAL"), "iterm2")
}

// ToPNG converts a raster image to PNG for previews. PNG input is returned
// unchanged; SVG and undecodable formats return an error.
func ToPNG(data []byte) ([]byte, image.Config, error) {
	if media.Detect(data) == "image/svg+xml" {
		return nil, image.Config{}, fmt.Errorf("SVG previews are not supported")
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, image.Config{}, err
	}
	if media.Detect(data) == "image/png" {
		return data, cfg, nil
	}
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, cfg, err
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, cfg, err
	}
	return buf.Bytes(), cfg, nil
}

// KittyOptions control how an image is placed.
type KittyOptions struct {
	// Columns limits the displayed width in terminal cells (0 = auto-fit).
	Columns int
}

// EncodeKitty returns the escape sequence that displays data inline.
func EncodeKitty(data []byte, opts KittyOptions) ([]byte, error) {
	pngData, cfg, err := ToPNG(data)
	if err != nil {
		return nil, err
	}
	cols := opts.Columns
	if cols == 0 {
		cols = fitColumns(cfg.Width)
	}
	encoded := base64.StdEncoding.EncodeToString(pngData)
	inTmux := os.Getenv("TMUX") != ""
	var out bytes.Buffer
	for i := 0; i < len(encoded); i += kittyChunk {
		end := min(i+kittyChunk, len(encoded))
		more := 0
		if end < len(encoded) {
			more = 1
		}
		control := "m=" + strconv.Itoa(more)
		if i == 0 {
			control = "a=T,f=100,q=2,m=" + strconv.Itoa(more)
			if cols > 0 {
				control += ",c=" + strconv.Itoa(cols)
			}
		}
		seq := "\x1b_G" + control + ";" + encoded[i:end] + "\x1b\\"
		if inTmux {
			seq = "\x1bPtmux;" + strings.ReplaceAll(seq, "\x1b", "\x1b\x1b") + "\x1b\\"
		}
		out.WriteString(seq)
	}
	out.WriteString("\n")
	return out.Bytes(), nil
}

// DisplayKitty writes an inline preview. Failures are silent: previews are
// best effort.
func DisplayKitty(w io.Writer, data []byte, opts KittyOptions) {
	seq, err := EncodeKitty(data, opts)
	if err != nil {
		return
	}
	_, _ = w.Write(seq)
}

// fitColumns limits wide images to the terminal width.
func fitColumns(imageWidth int) int {
	if v, err := strconv.Atoi(os.Getenv("AI_CLI_PREVIEW_WIDTH")); err == nil && v > 0 {
		return v
	}
	cols, xpixel := terminalSize()
	if cols <= 0 {
		return 0
	}
	if xpixel > 0 {
		if imageWidth <= xpixel {
			return 0
		}
		return cols
	}
	// Unknown pixel size: assume ~10px cells and cap at the terminal width.
	if imageWidth/10 > cols {
		return cols
	}
	return 0
}

// Dimensions returns an image's pixel size, or zeros when unknown.
func Dimensions(data []byte) (int, int) {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return 0, 0
	}
	return cfg.Width, cfg.Height
}
