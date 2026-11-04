package media

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDetect(t *testing.T) {
	cases := map[string][]byte{
		"image/png":     {0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a, 0},
		"image/jpeg":    {0xff, 0xd8, 0xff, 0xe0},
		"image/gif":     []byte("GIF89a..."),
		"image/webp":    []byte("RIFF\x00\x00\x00\x00WEBPVP8 "),
		"image/svg+xml": []byte("  <?xml version=\"1.0\"?><svg xmlns=\"http://www.w3.org/2000/svg\"/>"),
		"image/avif":    []byte("\x00\x00\x00\x1cftypavif\x00\x00\x00\x00"),
	}
	for want, data := range cases {
		if got := Detect(data); got != want {
			t.Errorf("Detect(%q) = %s, want %s", data[:4], got, want)
		}
	}
	if IsImage([]byte("hello world")) {
		t.Error("text is not an image")
	}
}

func TestLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.png")
	_ = os.WriteFile(path, []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}, 0o644)

	img, err := Load(path, "images")
	if err != nil || img.MediaType != "image/png" {
		t.Fatalf("path: %+v %v", img, err)
	}
	if img, err = Load("file://"+path, "images"); err != nil || len(img.Data) != 8 {
		t.Fatalf("file url: %v", err)
	}
	if img, err = Load("https://example.com/x.png", "images"); err != nil || !img.IsRemote() {
		t.Fatalf("http: %+v %v", img, err)
	}
	if img, err = Load("data:image/png;base64,iVBORw0KGgo=", "images"); err != nil || img.MediaType != "image/png" || len(img.Data) != 8 {
		t.Fatalf("data url: %+v %v", img, err)
	}
	if _, err = Load("ftp://x/y.png", "images"); err == nil {
		t.Fatal("ftp should be rejected")
	}
	if _, err = Load("   ", "images"); err == nil {
		t.Fatal("empty should be rejected")
	}
}

func TestExtension(t *testing.T) {
	if Extension("image/jpeg") != ".jpg" || Extension("image/webp; q=1") != ".webp" || Extension("weird") != ".png" {
		t.Fatal("Extension mapping wrong")
	}
}
