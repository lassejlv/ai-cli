package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"github.com/lassejlv/ai-cli/internal/media"
)

func newMCPCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "mcp",
		Short: "Start an MCP server over stdio with image generation tools",
		Long: `Start a Model Context Protocol server over stdio. Tools: generate_image,
list_providers, list_models and list_accounts. Images are saved to
AI_CLI_OUTPUT_DIR (or the working directory) and returned as file paths;
set return_images to also receive them inline.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return serveMCP(cmd.Context(), os.Stdin, os.Stdout)
		},
	}
}

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func serveMCP(ctx context.Context, in io.Reader, out io.Writer) error {
	var mu sync.Mutex
	send := func(v any) {
		mu.Lock()
		defer mu.Unlock()
		b, _ := json.Marshal(v)
		_, _ = out.Write(append(b, '\n'))
	}
	reply := func(id json.RawMessage, result any, rerr *rpcError) {
		msg := map[string]any{"jsonrpc": "2.0", "id": id}
		if rerr != nil {
			msg["error"] = rerr
		} else {
			msg["result"] = result
		}
		send(msg)
	}
	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 1<<20), 64<<20)
	var wg sync.WaitGroup
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var req rpcRequest
		if err := json.Unmarshal(line, &req); err != nil {
			reply(nil, nil, &rpcError{Code: -32700, Message: "parse error"})
			continue
		}
		if len(req.ID) == 0 {
			continue // notification
		}
		switch req.Method {
		case "initialize":
			var p struct {
				ProtocolVersion string `json:"protocolVersion"`
			}
			_ = json.Unmarshal(req.Params, &p)
			version := p.ProtocolVersion
			if version == "" {
				version = "2025-06-18"
			}
			reply(req.ID, map[string]any{
				"protocolVersion": version,
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]any{"name": "ai-cli", "version": Version},
				"instructions":    "Generate images with generate_image. Models are provider[@account]:model; call list_providers to see which providers are signed in and list_models for model IDs.",
			}, nil)
		case "ping":
			reply(req.ID, map[string]any{}, nil)
		case "tools/list":
			reply(req.ID, map[string]any{"tools": mcpTools()}, nil)
		case "tools/call":
			wg.Add(1)
			go func(req rpcRequest) {
				defer wg.Done()
				result := callTool(ctx, req.Params)
				reply(req.ID, result, nil)
			}(req)
		default:
			reply(req.ID, nil, &rpcError{Code: -32601, Message: "method not found: " + req.Method})
		}
	}
	wg.Wait()
	return scanner.Err()
}

func mcpTools() []map[string]any {
	str := func(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }
	return []map[string]any{
		{
			"name":        "generate_image",
			"description": "Generate or edit images. Returns saved file paths and metadata as JSON.",
			"inputSchema": map[string]any{
				"type":     "object",
				"required": []string{"prompt"},
				"properties": map[string]any{
					"prompt":          str("What to generate"),
					"model":           str("provider[@account]:model, comma-separated for several (default: the default provider's model)"),
					"provider":        str("Provider for unprefixed model IDs"),
					"account":         str("Saved account name"),
					"n":               map[string]any{"type": "integer", "minimum": 1, "maximum": 16, "description": "Images per model"},
					"size":            str("WxH, e.g. 1024x1024"),
					"aspect_ratio":    str("W:H, e.g. 16:9"),
					"seed":            map[string]any{"type": "integer"},
					"images":          map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Reference image paths, URLs or data URLs"},
					"mask":            str("Mask image for edits"),
					"params":          map[string]any{"type": "object", "description": "Provider-specific options, e.g. {\"quality\":\"high\"}"},
					"output_dir":      str("Directory to save images in"),
					"return_images":   map[string]any{"type": "boolean", "description": "Also return the images inline (default false)"},
					"timeout_seconds": map[string]any{"type": "integer", "description": "Per-request timeout (default 300)"},
				},
			},
		},
		{"name": "list_providers", "description": "List image providers, whether they are signed in, and default models.", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{}}},
		{"name": "list_models", "description": "List known image models.", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{"provider": str("Only this provider")}}},
		{"name": "list_accounts", "description": "List saved accounts per provider.", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{}}},
	}
}

func toolText(v any, isError bool) map[string]any {
	b, _ := json.MarshalIndent(v, "", "  ")
	return map[string]any{"content": []any{map[string]any{"type": "text", "text": string(b)}}, "isError": isError}
}

func callTool(ctx context.Context, raw json.RawMessage) map[string]any {
	var call struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(raw, &call); err != nil {
		return toolText(map[string]string{"error": "invalid tool call"}, true)
	}
	switch call.Name {
	case "list_providers":
		return toolText(listProviders(), false)
	case "list_accounts":
		rows, err := listAccounts()
		if err != nil {
			return toolText(map[string]string{"error": err.Error()}, true)
		}
		return toolText(rows, false)
	case "list_models":
		var a struct {
			Provider string `json:"provider"`
		}
		_ = json.Unmarshal(call.Arguments, &a)
		return toolText(listModels(ctx, strings.ToLower(a.Provider), true), false)
	case "generate_image":
		return mcpGenerate(ctx, call.Arguments)
	}
	return toolText(map[string]string{"error": "unknown tool " + call.Name}, true)
}

func mcpGenerate(ctx context.Context, raw json.RawMessage) map[string]any {
	var a struct {
		Prompt       string         `json:"prompt"`
		Model        string         `json:"model"`
		Provider     string         `json:"provider"`
		Account      string         `json:"account"`
		N            int            `json:"n"`
		Size         string         `json:"size"`
		AspectRatio  string         `json:"aspect_ratio"`
		Seed         *int64         `json:"seed"`
		Images       []string       `json:"images"`
		Mask         string         `json:"mask"`
		Params       map[string]any `json:"params"`
		OutputDir    string         `json:"output_dir"`
		ReturnImages bool           `json:"return_images"`
		Timeout      int            `json:"timeout_seconds"`
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return toolText(map[string]string{"error": "invalid arguments: " + err.Error()}, true)
	}
	f := &imageFlags{
		model: a.Model, providerName: a.Provider, account: a.Account, n: max(a.N, 1), size: a.Size, aspectRatio: a.AspectRatio,
		images: a.Images, mask: a.Mask, maxRetries: 2, concurrency: defaultImageConcurrency, timeout: defaultImageTimeout, json: true,
	}
	if a.Timeout > 0 {
		f.timeout = a.Timeout
	}
	if a.Seed != nil {
		f.seed = fmt.Sprint(*a.Seed)
	}
	f.outputPath = a.OutputDir
	if f.outputPath == "" {
		f.outputPath = os.Getenv("AI_CLI_OUTPUT_DIR")
	}
	if f.outputPath == "" {
		f.outputPath, _ = os.Getwd()
	}
	if !strings.HasSuffix(f.outputPath, "/") {
		f.outputPath += "/"
	}
	opts, specs, err := buildRun(ctx, f, strings.TrimSpace(a.Prompt), nil)
	if err != nil {
		return toolText(map[string]any{"success": false, "error": ErrorDetail{Name: "UsageError", Message: err.Error()}}, true)
	}
	opts.Params = a.Params
	opts.Quiet, opts.Preview, opts.Stdout = true, false, io.Discard
	ctx, cancel := context.WithTimeout(ctx, time.Duration(f.timeout+30)*time.Second)
	defer cancel()
	result := Run(ctx, specs, opts)
	resp := toolText(result, !result.Success)
	if a.ReturnImages {
		content := resp["content"].([]any)
		for _, r := range result.Results {
			for _, img := range r.Images {
				if img.File == nil {
					continue
				}
				data, err := os.ReadFile(*img.File)
				if err != nil || media.Detect(data) == "image/svg+xml" {
					continue
				}
				content = append(content, map[string]any{"type": "image", "data": base64.StdEncoding.EncodeToString(data), "mimeType": img.MediaType})
			}
		}
		resp["content"] = content
	}
	return resp
}
