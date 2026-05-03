package slack

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	// Side-effect imports register decoders so image.Decode can detect format.
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"gioui.org/op/paint"
	_ "golang.org/x/image/webp" // webp shows up in Slack uploads occasionally
)

// errLoginRedirect is returned by CheckRedirect when files.slack.com bounces us
// to the workspace web login page. That happens both when a token genuinely
// can't see the file and as a load-shedding response under heavy concurrency.
// Either way, downloading the login HTML is pointless — fail fast so the
// retry/backoff path can decide whether to try again.
var errLoginRedirect = errors.New("redirected to workspace login")

// maxConcurrentFetches caps how many image downloads run at once. files.slack.com
// rate-limits aggressively per token; bursts of ~16 thumbnails on a busy
// channel reliably trigger 429s and login redirects. 4 keeps every fetch
// progressing without tripping the limiter.
const maxConcurrentFetches = 4

// ImageLoader downloads and decodes Slack file URLs (which require the bot
// token in an Authorization header) on demand. Results are kept in memory
// and mirrored to disk so subsequent runs don't re-fetch.
type ImageLoader struct {
	token    string
	cookie   string
	client   *http.Client
	cacheDir string
	sem      chan struct{}

	mu       sync.Mutex
	entries  map[string]*imageEntry
	onChange func()
}

type imageEntry struct {
	img    image.Image
	op     paint.ImageOp // built once on first read; reused across frames
	hasOp  bool
	err    error
	loaded bool
}

func NewImageLoader(token, cookie string, onChange func()) *ImageLoader {
	dir, err := os.UserCacheDir()
	if err != nil {
		dir = os.TempDir()
	}
	hc := &http.Client{
		Timeout: 30 * time.Second,
		// Slack's files.slack.com regularly 302s to a CDN host. Go's default
		// redirect policy strips Authorization when the target host changes,
		// which leaves us fetching an HTML login page. Re-attach the bearer
		// on every hop so the CDN serves the bytes.
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return fmt.Errorf("too many redirects")
			}
			// Slack 302s authed file requests to <workspace>.slack.com/?redir=<path>
			// when the token doesn't have access OR (more often in practice) when
			// the limiter is shedding load. The login page returns 200 + HTML, so
			// without this guard we'd cache HTML as if it were a successful fetch.
			// Aborting here surfaces a typed error the retry path can recognize.
			if req.URL.Path == "/" && req.URL.Query().Get("redir") != "" &&
				strings.HasSuffix(req.URL.Host, ".slack.com") {
				return errLoginRedirect
			}
			if len(via) > 0 {
				if auth := via[0].Header.Get("Authorization"); auth != "" {
					if strings.HasSuffix(req.URL.Host, ".slack.com") || req.URL.Host == "slack.com" {
						req.Header.Set("Authorization", auth)
					}
				}
			}
			return nil
		},
	}
	return &ImageLoader{
		token:    token,
		cookie:   cookie,
		client:   hc,
		cacheDir: filepath.Join(dir, "wlslack", "images"),
		sem:      make(chan struct{}, maxConcurrentFetches),
		entries:  make(map[string]*imageEntry),
		onChange: onChange,
	}
}

// Get returns the decoded image for url, or nil if it isn't loaded yet. The
// first call for a URL kicks off a background fetch; when the fetch finishes,
// the loader fires onChange so the UI can redraw.
func (l *ImageLoader) Get(url string) (image.Image, bool) {
	if url == "" {
		return nil, true // treat as "nothing to load"
	}
	l.mu.Lock()
	if e, ok := l.entries[url]; ok {
		img, done := e.img, e.loaded
		l.mu.Unlock()
		return img, done
	}
	l.entries[url] = &imageEntry{}
	l.mu.Unlock()

	go l.fetch(url)
	return nil, false
}

// GetOp is the rendering-friendly variant of Get: it returns a cached
// paint.ImageOp built once per image so callers don't rebuild the GPU upload
// every frame. The boolean reports whether the fetch has completed (true with
// an empty op means the fetch failed); the missing-decoder case is also
// folded in here, surfacing as done=true, hasOp=false.
func (l *ImageLoader) GetOp(url string) (paint.ImageOp, bool, bool) {
	if url == "" {
		return paint.ImageOp{}, false, true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	e, ok := l.entries[url]
	if !ok {
		l.entries[url] = &imageEntry{}
		go l.fetch(url)
		return paint.ImageOp{}, false, false
	}
	if !e.loaded {
		return paint.ImageOp{}, false, false
	}
	if e.img == nil {
		return paint.ImageOp{}, false, true
	}
	if !e.hasOp {
		e.op = paint.NewImageOp(e.img)
		e.hasOp = true
	}
	return e.op, true, true
}

func (l *ImageLoader) fetch(url string) {
	img, err := l.load(url)
	l.mu.Lock()
	e := l.entries[url]
	e.img = img
	e.err = err
	e.loaded = true
	l.mu.Unlock()

	if err != nil {
		slog.Debug("image fetch failed", "url", url, "error", err)
	}
	if l.onChange != nil {
		l.onChange()
	}
}

func (l *ImageLoader) load(url string) (image.Image, error) {
	path := l.diskPath(url)
	if data, err := os.ReadFile(path); err == nil {
		if img, _, err := image.Decode(bytes.NewReader(data)); err == nil {
			return img, nil
		}
		// Decode failed: fall through and re-fetch, overwriting the bad cache.
	}

	// Cap concurrent network fetches. Slack's files.slack.com starts shedding
	// load at fairly low concurrency, and the symptoms (429s plus login-page
	// redirects) cascade if we let every visible thumbnail race at once.
	l.sem <- struct{}{}
	defer func() { <-l.sem }()

	const maxAttempts = 4
	var lastErr error
	for attempt := range maxAttempts {
		if attempt > 0 {
			delay := backoffDelay(attempt, lastErr)
			slog.Debug("image fetch retry", "url", url, "attempt", attempt, "delay", delay, "prev_err", lastErr)
			time.Sleep(delay)
		}

		data, err := l.fetchOnce(url)
		if err == nil {
			img, _, derr := image.Decode(bytes.NewReader(data))
			if derr != nil {
				return nil, fmt.Errorf("decode (%d bytes, head=%q): %w", len(data), peek(data), derr)
			}
			if mkErr := os.MkdirAll(filepath.Dir(path), 0755); mkErr == nil {
				_ = os.WriteFile(path, data, 0600)
			}
			return img, nil
		}

		lastErr = err
		if !isRetryable(err) {
			return nil, err
		}
	}
	return nil, fmt.Errorf("after %d attempts: %w", maxAttempts, lastErr)
}

// retryableErr wraps a transient failure so the retry loop can recognize it
// without depending on string matching.
type retryableErr struct {
	err        error
	retryAfter time.Duration // 0 means "use exponential backoff"
}

func (r *retryableErr) Error() string { return r.err.Error() }
func (r *retryableErr) Unwrap() error { return r.err }

func isRetryable(err error) bool {
	var r *retryableErr
	return errors.As(err, &r)
}

// fetchOnce performs a single GET and returns the response bytes, classifying
// transient failures (429s, 5xx, login-page redirects, HTML bodies) as
// retryableErr so the caller can back off and try again.
func (l *ImageLoader) fetchOnce(url string) ([]byte, error) {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	if strings.HasSuffix(req.URL.Host, ".slack.com") || req.URL.Host == "slack.com" {
		req.Header.Set("Authorization", "Bearer "+l.token)
		if l.cookie != "" {
			req.AddCookie(&http.Cookie{Name: "d", Value: l.cookie})
		}
	}

	resp, err := l.client.Do(req)
	if err != nil {
		// Login redirects are permanent for a given (token, file) pair: the
		// origin already saw our auth and chose to bounce us to the workspace
		// login page. Retrying just hammers the same wall, so surface as a
		// non-retryable error with a hint about the most common fix.
		if errors.Is(err, errLoginRedirect) {
			hint := "auth rejected by files.slack.com"
			if l.cookie == "" {
				hint += " (set SLACK_COOKIE=<d cookie value> if using an xoxc token)"
			}
			return nil, fmt.Errorf("%s: %w", hint, err)
		}
		return nil, &retryableErr{err: fmt.Errorf("fetch: %w", err)}
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, &retryableErr{
			err:        fmt.Errorf("status 429"),
			retryAfter: parseRetryAfter(resp.Header.Get("Retry-After")),
		}
	}
	if resp.StatusCode >= 500 {
		return nil, &retryableErr{err: fmt.Errorf("status %d", resp.StatusCode)}
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, &retryableErr{err: fmt.Errorf("read body: %w", err)}
	}

	// An HTML body on a 200 means the redirect chain landed on the workspace
	// shell — same auth-rejected story as errLoginRedirect, just laundered
	// through a different response. Don't cache HTML and don't retry.
	ct := resp.Header.Get("Content-Type")
	if strings.HasPrefix(ct, "text/html") || looksLikeHTML(data) {
		final := resp.Request.URL.String()
		hint := "auth rejected by files.slack.com"
		if l.cookie == "" {
			hint += " (set SLACK_COOKIE=<d cookie value> if using an xoxc token)"
		}
		return nil, fmt.Errorf("%s: got HTML (content-type=%q, final-url=%q, head=%q)", hint, ct, final, peek(data))
	}

	return data, nil
}

// backoffDelay picks the wait before the next attempt. If the server told us a
// Retry-After, we honor that (Slack's limiter sets it on 429s); otherwise we
// fall back to exponential backoff with jitter, scaled so a 4-attempt budget
// stays under ~10s total.
func backoffDelay(attempt int, prev error) time.Duration {
	var r *retryableErr
	if errors.As(prev, &r) && r.retryAfter > 0 {
		return r.retryAfter
	}
	base := time.Duration(500*(1<<(attempt-1))) * time.Millisecond
	jitter := time.Duration(rand.Int63n(int64(base / 2)))
	return base + jitter
}

func parseRetryAfter(h string) time.Duration {
	if h == "" {
		return 0
	}
	if secs, err := strconv.Atoi(strings.TrimSpace(h)); err == nil && secs > 0 {
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(h); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

func looksLikeHTML(data []byte) bool {
	head := bytes.TrimLeft(data[:min(len(data), 256)], " \t\r\n")
	return bytes.HasPrefix(bytes.ToLower(head), []byte("<!doctype html")) ||
		bytes.HasPrefix(bytes.ToLower(head), []byte("<html"))
}

func peek(data []byte) string {
	if len(data) > 64 {
		return string(data[:64])
	}
	return string(data)
}

func (l *ImageLoader) diskPath(url string) string {
	h := sha256.Sum256([]byte(url))
	return filepath.Join(l.cacheDir, hex.EncodeToString(h[:]))
}
