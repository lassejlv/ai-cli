package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/lassejlv/ai-cli/internal/media"
)

// UserAgent identifies this CLI.
var UserAgent = "ai-cli-go"

// APIError is a non-2xx provider response.
type APIError struct {
	Provider string
	Status   int
	Message  string
	Body     string
	// RetryAfter is the server's requested delay, or -1 when absent.
	RetryAfter time.Duration
}

func (e *APIError) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("%s: %s (HTTP %d)", e.Provider, e.Message, e.Status)
	}
	return fmt.Sprintf("%s: HTTP %d", e.Provider, e.Status)
}

// Retryable reports whether the error is worth retrying.
func (e *APIError) Retryable() bool {
	return e.Status == http.StatusTooManyRequests || e.Status == http.StatusRequestTimeout || e.Status == 409 || e.Status >= 500
}

// call describes one HTTP request.
type call struct {
	Provider string
	Method   string
	URL      string
	Headers  map[string]string
	JSON     any
	Body     []byte
	// ContentType is used with Body.
	ContentType string
	MaxRetries  int
	Client      *http.Client
	// Idempotent allows retrying a POST after it may have reached the server.
	Idempotent bool
}

// response is a successful HTTP response.
type response struct {
	Status  int
	Headers http.Header
	Body    []byte
}

// JSON decodes the response body.
func (r *response) JSONInto(v any) error {
	if err := json.Unmarshal(r.Body, v); err != nil {
		return fmt.Errorf("invalid JSON response: %w", err)
	}
	return nil
}

// do sends a request, retrying transient failures with exponential backoff.
func do(ctx context.Context, c call) (*response, error) {
	body := c.Body
	contentType := c.ContentType
	if c.JSON != nil {
		var err error
		body, err = json.Marshal(c.JSON)
		if err != nil {
			return nil, err
		}
		contentType = "application/json"
	}
	method := c.Method
	if method == "" {
		method = http.MethodPost
		if body == nil {
			method = http.MethodGet
		}
	}
	client := c.Client
	if client == nil {
		client = http.DefaultClient
	}
	// Generation submits are paid and not idempotent: only retry them when the
	// provider cannot have started work (connection refused, 429, 503).
	idempotent := c.Idempotent || method == http.MethodGet || method == http.MethodHead || method == http.MethodPut
	var lastErr error
	for attempt := 0; attempt <= c.MaxRetries; attempt++ {
		if attempt > 0 {
			delay := backoff(attempt, lastErr)
			if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) < delay {
				return nil, lastErr
			}
			select {
			case <-ctx.Done():
				return nil, timeoutError(ctx, ctx.Err())
			case <-time.After(delay):
			}
		}
		var reader io.Reader
		if body != nil {
			reader = bytes.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, c.URL, reader)
		if err != nil {
			return nil, err
		}
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
		req.Header.Set("User-Agent", UserAgent)
		for k, v := range c.Headers {
			req.Header.Set(k, v)
		}
		res, err := client.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return nil, timeoutError(ctx, err)
			}
			lastErr = fmt.Errorf("%s: %w", c.Provider, err)
			if idempotent || isDialError(err) {
				continue
			}
			return nil, lastErr
		}
		data, err := io.ReadAll(io.LimitReader(res.Body, media.MaxDownloadBytes))
		res.Body.Close()
		if err != nil {
			if ctx.Err() != nil {
				return nil, timeoutError(ctx, err)
			}
			lastErr = fmt.Errorf("%s: reading response: %w", c.Provider, err)
			if idempotent {
				continue
			}
			return nil, lastErr
		}
		if res.StatusCode >= 200 && res.StatusCode <= 299 {
			return &response{Status: res.StatusCode, Headers: res.Header, Body: data}, nil
		}
		apiErr := &APIError{Provider: c.Provider, Status: res.StatusCode, Message: errorMessage(data), Body: string(data),
			RetryAfter: parseRetryAfter(res.Header.Get("Retry-After"))}
		lastErr = apiErr
		retry := apiErr.Retryable()
		if !idempotent {
			retry = apiErr.Status == http.StatusTooManyRequests || apiErr.Status == http.StatusServiceUnavailable
		}
		if !retry {
			return nil, apiErr
		}
	}
	return nil, lastErr
}

func isDialError(err error) bool {
	var opErr *net.OpError
	return errors.As(err, &opErr) && opErr.Op == "dial"
}

func timeoutError(ctx context.Context, err error) error {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return errors.New("request timed out (use --timeout to allow longer)")
	}
	return err
}

// parseRetryAfter reads delta-seconds or an HTTP date.
func parseRetryAfter(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return -1
	}
	if secs, err := strconv.Atoi(v); err == nil && secs >= 0 {
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		return max(time.Until(t), 0)
	}
	return -1
}

func backoff(attempt int, lastErr error) time.Duration {
	var apiErr *APIError
	if errors.As(lastErr, &apiErr) && apiErr.RetryAfter >= 0 {
		return apiErr.RetryAfter
	}
	base := time.Duration(1<<(attempt-1)) * 2 * time.Second
	return base + time.Duration(rand.Int64N(int64(500*time.Millisecond)))
}

// errorMessage extracts a human-readable message from common error bodies.
func errorMessage(body []byte) string {
	var v any
	if json.Unmarshal(body, &v) != nil {
		s := strings.TrimSpace(string(body))
		if len(s) > 300 {
			s = s[:300] + "…"
		}
		if strings.HasPrefix(s, "<") {
			return ""
		}
		return s
	}
	if msg := findMessage(v, 0); msg != "" {
		return msg
	}
	s := strings.TrimSpace(string(body))
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	return s
}

func findMessage(v any, depth int) string {
	if depth > 4 {
		return ""
	}
	switch t := v.(type) {
	case string:
		return t
	case map[string]any:
		for _, key := range []string{"message", "detail", "error_description", "error", "errors", "title"} {
			if inner, ok := t[key]; ok {
				if msg := findMessage(inner, depth+1); msg != "" {
					return msg
				}
			}
		}
	case []any:
		var parts []string
		for _, item := range t {
			if msg := findMessage(item, depth+1); msg != "" {
				parts = append(parts, msg)
			}
			if len(parts) == 3 {
				break
			}
		}
		return strings.Join(parts, "; ")
	}
	return ""
}

// poll calls fn until it reports done, sleeping interval between attempts.
func poll(ctx context.Context, interval time.Duration, fn func() (bool, error)) error {
	for {
		done, err := fn()
		if err != nil || done {
			return err
		}
		select {
		case <-ctx.Done():
			return timeoutError(ctx, ctx.Err())
		case <-time.After(interval):
		}
	}
}

// merge deep-merges right into a copy of left.
func merge(left, right map[string]any) map[string]any {
	out := make(map[string]any, len(left)+len(right))
	for k, v := range left {
		out[k] = v
	}
	for k, v := range right {
		if rv, ok := v.(map[string]any); ok {
			if lv, ok := out[k].(map[string]any); ok {
				out[k] = merge(lv, rv)
				continue
			}
		}
		out[k] = v
	}
	return out
}

// Merge is the exported deep merge used by the CLI.
func Merge(left, right map[string]any) map[string]any { return merge(left, right) }

// downloadImages fetches result URLs into Images.
func downloadImages(ctx context.Context, client *http.Client, urls []string, headers map[string]string) ([]Image, error) {
	images := make([]Image, 0, len(urls))
	for _, u := range urls {
		if strings.HasPrefix(u, "data:") {
			img, err := media.ParseDataURL(u)
			if err != nil {
				return nil, err
			}
			images = append(images, Image{Data: img.Data, MediaType: img.MediaType})
			continue
		}
		data, mediaType, err := media.Download(ctx, client, u, headers)
		if err != nil {
			return nil, err
		}
		images = append(images, Image{Data: data, MediaType: mediaType, URL: u})
	}
	return images, nil
}

// decodeB64Image turns a base64 payload into an Image.
func decodeB64Image(b64 string, fallbackType string) (Image, error) {
	data, err := media.DecodeBase64(b64)
	if err != nil {
		return Image{}, fmt.Errorf("invalid base64 image: %w", err)
	}
	mt := media.Detect(data)
	if mt == "application/octet-stream" && fallbackType != "" {
		mt = fallbackType
	}
	return Image{Data: data, MediaType: mt}, nil
}

// aspectToOpenAISize maps an aspect ratio to the nearest GPT Image size.
func aspectToOpenAISize(aspect string) string {
	w, h, ok := parseRatio(aspect)
	if !ok {
		return ""
	}
	switch {
	case w/h > 1.1:
		return "1536x1024"
	case h/w > 1.1:
		return "1024x1536"
	default:
		return "1024x1024"
	}
}

func parseRatio(aspect string) (float64, float64, bool) {
	parts := strings.SplitN(aspect, ":", 2)
	if len(parts) != 2 {
		return 0, 0, false
	}
	w, err1 := strconv.ParseFloat(parts[0], 64)
	h, err2 := strconv.ParseFloat(parts[1], 64)
	if err1 != nil || err2 != nil || w <= 0 || h <= 0 {
		return 0, 0, false
	}
	return w, h, true
}

func parseSize(size string) (int, int, bool) {
	parts := strings.SplitN(size, "x", 2)
	if len(parts) != 2 {
		return 0, 0, false
	}
	w, err1 := strconv.Atoi(parts[0])
	h, err2 := strconv.Atoi(parts[1])
	return w, h, err1 == nil && err2 == nil && w > 0 && h > 0
}

func headerID(h http.Header) string {
	for _, key := range []string{"x-ai-gateway-response-id", "x-ai-gateway-request-id", "x-request-id", "request-id", "x-vercel-id"} {
		if v := strings.TrimSpace(h.Get(key)); v != "" {
			return v
		}
	}
	return ""
}

func withHeaders(base map[string]string, extra map[string]string) map[string]string {
	out := make(map[string]string, len(base)+len(extra))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range extra {
		out[k] = v
	}
	return out
}

func authCheck(provider, source string, err error) error {
	var apiErr *APIError
	if errors.As(err, &apiErr) && (apiErr.Status == http.StatusUnauthorized || apiErr.Status == http.StatusForbidden) {
		e := NewAuthError(provider, source)
		if apiErr.Message != "" && apiErr.Status == http.StatusForbidden {
			e.Message += " (" + apiErr.Message + ")"
		}
		return e
	}
	return err
}
