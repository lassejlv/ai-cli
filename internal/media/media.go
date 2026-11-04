// Package media loads reference images and detects image media types.
package media

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
)

// Image is either inline bytes or a remote URL. Remote images are only
// downloaded when a provider needs their bytes.
type Image struct {
	Data      []byte
	MediaType string
	URL       string
}

// IsRemote reports whether the image is an HTTP(S) URL without loaded bytes.
func (img Image) IsRemote() bool { return len(img.Data) == 0 && img.URL != "" }

var (
	schemeRE     = regexp.MustCompile(`^[a-zA-Z][a-zA-Z\d+.-]*://`)
	windowsDrive = regexp.MustCompile(`^[a-zA-Z]:[\\/]`)
)

// Load resolves a path, file:// URL, data URL or HTTP(S) URL into an Image.
func Load(ref, optionName string) (Image, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return Image{}, fmt.Errorf("--%s cannot be empty", optionName)
	}
	lower := strings.ToLower(ref)
	if strings.HasPrefix(lower, "data:") {
		return ParseDataURL(ref)
	}
	if !windowsDrive.MatchString(ref) && schemeRE.MatchString(ref) {
		u, err := url.Parse(ref)
		if err != nil {
			return Image{}, fmt.Errorf("invalid reference image URL %q: %w", ref, err)
		}
		switch u.Scheme {
		case "http", "https":
			return Image{URL: u.String()}, nil
		case "file":
			return readFile(u.Path)
		default:
			return Image{}, fmt.Errorf("unsupported reference image URL scheme %q; use a file path, file:// URL, http(s) URL, or data URL", u.Scheme+":")
		}
	}
	return readFile(ref)
}

// LoadAll loads every reference.
func LoadAll(refs []string, optionName string) ([]Image, error) {
	out := make([]Image, 0, len(refs))
	for _, ref := range refs {
		img, err := Load(ref, optionName)
		if err != nil {
			return nil, err
		}
		out = append(out, img)
	}
	return out, nil
}

func readFile(path string) (Image, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Image{}, fmt.Errorf("could not read reference image %q: %w", path, err)
	}
	return FromBytes(data), nil
}

// FromBytes wraps raw bytes and sniffs their media type.
func FromBytes(data []byte) Image {
	return Image{Data: data, MediaType: Detect(data)}
}

// ParseDataURL decodes a base64 data URL.
func ParseDataURL(raw string) (Image, error) {
	comma := strings.IndexByte(raw, ',')
	if comma < 0 {
		return Image{}, fmt.Errorf("invalid data URL")
	}
	meta := raw[len("data:"):comma]
	payload := raw[comma+1:]
	parts := strings.Split(meta, ";")
	mediaType := parts[0]
	isBase64 := false
	for _, p := range parts[1:] {
		if strings.EqualFold(p, "base64") {
			isBase64 = true
		}
	}
	var data []byte
	if isBase64 {
		decoded, err := DecodeBase64(payload)
		if err != nil {
			return Image{}, fmt.Errorf("invalid base64 in data URL: %w", err)
		}
		data = decoded
	} else {
		unescaped, err := url.PathUnescape(payload)
		if err != nil {
			return Image{}, fmt.Errorf("invalid data URL: %w", err)
		}
		data = []byte(unescaped)
	}
	if mediaType == "" {
		mediaType = Detect(data)
	}
	return Image{Data: data, MediaType: mediaType}, nil
}

// DecodeBase64 accepts standard or URL-safe base64, padded or not.
func DecodeBase64(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	if i := strings.Index(s, ","); strings.HasPrefix(s, "data:") && i >= 0 {
		s = s[i+1:]
	}
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if b, err := enc.DecodeString(s); err == nil {
			return b, nil
		}
	}
	return nil, fmt.Errorf("not valid base64")
}

// Bytes returns the image bytes, downloading remote images when needed.
func Bytes(ctx context.Context, client *http.Client, img Image) (Image, error) {
	if !img.IsRemote() {
		if img.MediaType == "" {
			img.MediaType = Detect(img.Data)
		}
		return img, nil
	}
	data, mediaType, err := Download(ctx, client, img.URL, nil)
	if err != nil {
		return Image{}, err
	}
	return Image{Data: data, MediaType: mediaType, URL: img.URL}, nil
}

// DataURL encodes the image as a data URL, downloading it first when remote.
func DataURL(ctx context.Context, client *http.Client, img Image) (string, error) {
	loaded, err := Bytes(ctx, client, img)
	if err != nil {
		return "", err
	}
	return "data:" + loaded.MediaType + ";base64," + base64.StdEncoding.EncodeToString(loaded.Data), nil
}

// MaxDownloadBytes bounds generated image downloads.
const MaxDownloadBytes = 256 << 20

// Download fetches a URL and returns its bytes and detected media type.
func Download(ctx context.Context, client *http.Client, rawURL string, headers map[string]string) ([]byte, string, error) {
	if client == nil {
		client = http.DefaultClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, "", err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	res, err := client.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("download %s: %w", redactURL(rawURL), err)
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode > 299 {
		return nil, "", fmt.Errorf("download %s: HTTP %d", redactURL(rawURL), res.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(res.Body, MaxDownloadBytes+1))
	if err != nil {
		return nil, "", fmt.Errorf("download %s: %w", redactURL(rawURL), err)
	}
	if len(data) > MaxDownloadBytes {
		return nil, "", fmt.Errorf("download %s: larger than %d bytes", redactURL(rawURL), MaxDownloadBytes)
	}
	mediaType := Detect(data)
	if mediaType == "application/octet-stream" {
		if ct := res.Header.Get("Content-Type"); strings.HasPrefix(ct, "image/") {
			mediaType = strings.TrimSpace(strings.SplitN(ct, ";", 2)[0])
		}
	}
	return data, mediaType, nil
}

func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "URL"
	}
	u.RawQuery = ""
	u.Fragment = ""
	return u.String()
}

// Detect sniffs an image media type from its first bytes.
func Detect(data []byte) string {
	switch {
	case bytes.HasPrefix(data, []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}):
		return "image/png"
	case bytes.HasPrefix(data, []byte{0xff, 0xd8, 0xff}):
		return "image/jpeg"
	case bytes.HasPrefix(data, []byte("GIF87a")), bytes.HasPrefix(data, []byte("GIF89a")):
		return "image/gif"
	case len(data) >= 12 && string(data[0:4]) == "RIFF" && string(data[8:12]) == "WEBP":
		return "image/webp"
	case bytes.HasPrefix(data, []byte("BM")):
		return "image/bmp"
	case bytes.HasPrefix(data, []byte{0x49, 0x49, 0x2a, 0x00}), bytes.HasPrefix(data, []byte{0x4d, 0x4d, 0x00, 0x2a}):
		return "image/tiff"
	}
	if len(data) >= 12 && string(data[4:8]) == "ftyp" {
		limit := min(len(data), 64)
		for off := 8; off+4 <= limit; off += 4 {
			switch string(data[off : off+4]) {
			case "avif", "avis":
				return "image/avif"
			case "heic", "heix", "hevc", "hevx", "mif1", "msf1":
				return "image/heic"
			}
		}
	}
	if looksLikeSVG(data) {
		return "image/svg+xml"
	}
	return "application/octet-stream"
}

// IsImage reports whether bytes look like a supported image.
func IsImage(data []byte) bool {
	return Detect(data) != "application/octet-stream"
}

func looksLikeSVG(data []byte) bool {
	prefix := strings.ToLower(strings.TrimSpace(string(data[:min(len(data), 512)])))
	return strings.HasPrefix(prefix, "<svg") || (strings.HasPrefix(prefix, "<?xml") && strings.Contains(prefix, "<svg"))
}

var extensions = map[string]string{
	"image/avif":    ".avif",
	"image/bmp":     ".bmp",
	"image/gif":     ".gif",
	"image/heic":    ".heic",
	"image/jpg":     ".jpg",
	"image/jpeg":    ".jpg",
	"image/png":     ".png",
	"image/svg+xml": ".svg",
	"image/tiff":    ".tiff",
	"image/webp":    ".webp",
}

// Extension returns a file extension for a media type, defaulting to .png.
func Extension(mediaType string) string {
	mt := strings.ToLower(strings.TrimSpace(strings.SplitN(mediaType, ";", 2)[0]))
	if ext, ok := extensions[mt]; ok {
		return ext
	}
	return ".png"
}
