package cli

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/lassejlv/ai-cli/internal/media"
)

func historyCLI(t *testing.T, args ...string) ([]byte, error) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = old }()
	read := make(chan []byte, 1)
	go func() { data, _ := io.ReadAll(r); read <- data }()
	root := NewRoot()
	root.SetArgs(args)
	err = root.Execute()
	w.Close()
	data := <-read
	r.Close()
	return data, err
}

func TestHistoryGenerateRerunEditWorkflow(t *testing.T) {
	isolate(t)
	t.Setenv("AI_CLI_NO_HISTORY", "")
	t.Setenv("OPENAI_API_KEY", "test-key")
	firstImage, generatedImage := pngBytes(t, 8, 8), pngBytes(t, 9, 9)
	type request struct {
		model, prompt, quality, size string
		n                            int
		images                       [][]byte
	}
	requests := make(chan request, 10)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := request{n: 1}
		if strings.Contains(r.Header.Get("Content-Type"), "multipart/form-data") {
			if err := r.ParseMultipartForm(1 << 20); err != nil {
				t.Error(err)
				w.WriteHeader(400)
				return
			}
			defer r.MultipartForm.RemoveAll()
			got.model, got.prompt, got.quality, got.size = r.FormValue("model"), r.FormValue("prompt"), r.FormValue("quality"), r.FormValue("size")
			if n := r.FormValue("n"); n != "" {
				got.n, _ = strconv.Atoi(n)
			}
			for _, f := range r.MultipartForm.File["image[]"] {
				file, err := f.Open()
				if err != nil {
					t.Error(err)
					return
				}
				data, _ := io.ReadAll(file)
				file.Close()
				got.images = append(got.images, data)
			}
		} else {
			var body struct {
				Model, Prompt, Quality, Size string
				N                            int
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
				return
			}
			got.model, got.prompt, got.quality, got.size = body.Model, body.Prompt, body.Quality, body.Size
			if body.N > 0 {
				got.n = body.N
			}
		}
		requests <- got
		items := make([]map[string]string, got.n)
		for i := range items {
			items[i] = map[string]string{"b64_json": base64.StdEncoding.EncodeToString(generatedImage)}
		}
		// Repeated provider IDs must not cause replay to replace old output.
		w.Header().Set("x-request-id", "repeated-id")
		json.NewEncoder(w).Encode(map[string]any{"data": items})
	}))
	defer srv.Close()
	run := func(args ...string) RunResult {
		t.Helper()
		data, err := historyCLI(t, args...)
		if err != nil {
			t.Fatalf("%v: %v: %s", args, err, data)
		}
		var result RunResult
		if err := json.Unmarshal(data, &result); err != nil {
			t.Fatal(err)
		}
		if !result.Success || result.HistoryID == "" {
			t.Fatalf("result = %s", data)
		}
		return result
	}
	initial := run("image", "a pencil fox", "--model", "openai:gpt-image-1", "--n", "2", "--size", "1024x1024", "--param", "quality=high", "--base-url", srv.URL, "--output", filepath.Join(t.TempDir(), "fox.png"), "--json", "--no-stdin")
	<-requests
	entry, err := readHistory(initial.HistoryID)
	if err != nil {
		t.Fatal(err)
	}
	if entry.Prompt != "a pencil fox" || len(entry.Outputs) != 2 {
		t.Fatalf("entry = %+v", entry)
	}
	for _, img := range entry.Outputs {
		if err := os.WriteFile(img.File, firstImage, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("AI_CLI_IMAGE_PROVIDER", "google")
	t.Setenv("AI_CLI_IMAGE_MODEL", "google:different-model")

	// Dry runs restore settings without spending or creating a history entry.
	data, err := historyCLI(t, "rerun", "--last", "--dry-run")
	if err != nil || !bytes.Contains(data, []byte("a pencil fox")) {
		t.Fatalf("dry run: %s, %v", data, err)
	}
	if len(requests) != 0 {
		t.Fatal("dry run called the provider")
	}
	entries, _ := listHistory(0)
	if len(entries) != 1 {
		t.Fatal("dry run recorded history")
	}

	repeated := run("rerun", "--last", "--n", "1", "--param", "quality=low", "--json")
	got := <-requests
	if got.model != "gpt-image-1" || got.prompt != entry.Prompt || got.n != 1 || got.quality != "low" || got.size != "1024x1024" {
		t.Fatalf("rerun request = %+v", got)
	}
	if *repeated.Results[0].Images[0].File == entry.Outputs[0].File {
		t.Fatal("rerun reused original filename")
	}
	for _, img := range entry.Outputs {
		data, _ := os.ReadFile(img.File)
		if !bytes.Equal(data, firstImage) {
			t.Fatal("rerun overwrote an original image")
		}
	}

	run("edit", "--from", initial.HistoryID, "--image-index", "2", "make the fox younger", "--json")
	got = <-requests
	if got.n != 1 || got.prompt != "make the fox younger" || got.quality != "high" || len(got.images) != 1 || !bytes.Equal(got.images[0], firstImage) {
		t.Fatalf("edit request = %+v", got)
	}
	// A replayed edit retains the reference bytes, even after the source is removed.
	if err := os.Remove(entry.Outputs[1].File); err != nil {
		t.Fatal(err)
	}
	run("rerun", "--last", "--json")
	got = <-requests
	if len(got.images) != 1 || !bytes.Equal(got.images[0], firstImage) {
		t.Fatal("rerun lost its cached edit reference")
	}

	data, err = historyCLI(t, "history", "--json", "--limit", "2")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &entries); err != nil || len(entries) != 2 {
		t.Fatalf("history: %s, %v", data, err)
	}
	if !entries[0].CreatedAt.After(entries[1].CreatedAt) {
		t.Fatal("history is not newest first")
	}
	if data, err := historyCLI(t, "edit", "--from", initial.HistoryID, "--image-index", "2", "snow", "--json"); err == nil || !bytes.Contains(data, []byte("missing")) {
		t.Fatalf("missing image: %s, %v", data, err)
	}
}

func TestHistoryOptOutAndSaveFailures(t *testing.T) {
	isolate(t)
	t.Setenv("AI_CLI_NO_HISTORY", "")
	srv := mockOpenAI(t, nil)
	defer srv.Close()
	t.Setenv("OPENAI_API_KEY", "test-key")
	specs := []ModelSpec{{Provider: "openai", Model: "gpt-image-1"}}
	opts := RunOptions{Prompt: "fox", N: 1, Concurrency: 1, Timeout: time.Second, JSON: true, Output: t.TempDir() + "/", Overrides: Overrides{BaseURL: srv.URL}}
	opts.NoHistory = true
	if r := Run(context.Background(), specs, opts); r.HistoryID != "" || !r.Success {
		t.Fatal("NoHistory failed")
	}
	opts.NoHistory = false
	t.Setenv("AI_CLI_NO_HISTORY", "1")
	if r := Run(context.Background(), specs, opts); r.HistoryID != "" || !r.Success {
		t.Fatal("environment opt-out failed")
	}
	t.Setenv("AI_CLI_NO_HISTORY", "")
	opts.NoSave = true
	if r := Run(context.Background(), specs, opts); r.HistoryID != "" {
		t.Fatal("NoSave recorded history")
	}
	opts.NoSave = false
	if entries, _ := listHistory(0); len(entries) != 0 {
		t.Fatal("opt-out left entries")
	}
	if err := os.MkdirAll(filepath.Dir(historyDir()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(historyDir(), []byte("blocked"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := Run(context.Background(), specs, opts)
	if !r.Success || len(r.Warnings) != 1 || r.Results[0].Images[0].File == nil {
		t.Fatal("history failure should preserve generated outputs and expose a warning")
	}
}

func TestHistoryConcurrentWritesAndCredentials(t *testing.T) {
	isolate(t)
	file := filepath.Join(t.TempDir(), "image.png")
	models := []preparedModel{{spec: ModelSpec{Provider: "openai", Model: "gpt-image-1"}, account: ResolvedAccount{Source: "login", Account: "work"}}}
	opts := RunOptions{Prompt: "fox", N: 1, Concurrency: 1, Timeout: time.Second, Images: []media.Image{media.FromBytes(pngBytes(t, 2, 2))}, Overrides: Overrides{BaseURL: "https://user:url-secret@example.com/v1?token=query-secret", Headers: map[string]string{"Authorization": "header-secret"}}}
	result := &RunResult{Success: true, Results: []*ModelResult{{Images: []*ArtifactInfo{{File: &file, MediaType: "image/png"}}}}}
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := saveHistory(models, opts, result); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	entries, err := listHistory(0)
	if err != nil || len(entries) != 12 {
		t.Fatalf("concurrent history: %d entries, %v", len(entries), err)
	}
	for _, e := range entries {
		path := filepath.Join(historyDir(), e.ID+".json")
		data, _ := os.ReadFile(path)
		if bytes.Contains(data, []byte("secret")) {
			t.Fatal("history included connection credentials")
		}
		if e.Models[0].Account != "work" || !e.Settings.NeedsHeaders || !e.Settings.NeedsBaseURL {
			t.Fatalf("settings = %+v", e)
		}
		if runtime.GOOS != "windows" {
			st, _ := os.Stat(path)
			if st.Mode().Perm() != 0o600 {
				t.Fatal("history is not private")
			}
		}
	}
	data, err := historyCLI(t, "rerun", "--last", "--dry-run", "--json")
	if err == nil || !bytes.Contains(data, []byte("pass --headers again")) {
		t.Fatalf("missing headers: %s, %v", data, err)
	}
}

func TestHistoryEmptyAndInvalidSelections(t *testing.T) {
	isolate(t)
	data, err := historyCLI(t, "history", "--json")
	if err != nil || strings.TrimSpace(string(data)) != "[]" {
		t.Fatalf("empty history: %s, %v", data, err)
	}
	for _, args := range [][]string{{"rerun", "--last"}, {"rerun"}, {"rerun", "--last", "another-id"}, {"edit", "a fox"}, {"edit", "--last", "--from", "another-id", "fox"}, {"rerun", "../auth"}} {
		data, err := historyCLI(t, append(args, "--json")...)
		if err == nil || !json.Valid(data) {
			t.Fatalf("%v: expected JSON error, got %s, %v", args, data, err)
		}
	}
}

func TestReplayConnectionDefaultsStayWithProvider(t *testing.T) {
	isolate(t)
	e := &historyEntry{Prompt: "fox", Models: []ModelSpec{{Provider: "openai", Model: "gpt-image-1"}},
		Settings: historySettings{N: 1, Concurrency: 1, Timeout: 30, BaseURL: "https://original.example/v1", Scope: "original-team"},
		Outputs:  []historyOutput{{File: filepath.Join(t.TempDir(), "fox.png"), MediaType: "image/png"}}}
	for _, model := range []string{"openai:gpt-image-2", "google:gemini-image"} {
		f := &imageFlags{}
		cmd := &cobra.Command{Use: "rerun"}
		addImageFlags(cmd, f)
		if err := cmd.ParseFlags([]string{"--model", model}); err != nil {
			t.Fatal(err)
		}
		if _, err := restoreHistory(cmd, f, e, false, 0, nil); err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(model, "openai:") {
			if f.baseURL != e.Settings.BaseURL || f.scope != e.Settings.Scope {
				t.Fatal("same-provider rerun lost its endpoint")
			}
		} else if f.baseURL != "" || f.scope != "" {
			t.Fatal("provider switch inherited another provider's connection settings")
		}
	}
}
