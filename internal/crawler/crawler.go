package crawler

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/romanthekat/gemini-tools/internal/gemini"
)

// Options controls crawler behavior
// Defaults: set by New if zero values provided
//  - DBDir: "data"
//  - QueuePath: "queue.txt"
//  - ErrorLogPath: "error_queue.log"
//  - Throttle: 2s
//  - RecrawlWindow: 72h
//  - MaxResponseMB: 10 (soft cap)
//
// Single-pass queue processing as per docs/crawler_plan.md

type Options struct {
	DBDir         string
	QueuePath     string
	ErrorLogPath  string
	Throttle      time.Duration
	RecrawlWindow time.Duration
	MaxResponseMB int
}

type Crawler struct {
	opts    Options
	seen    map[string]struct{}
	lastReq map[string]time.Time
	mu      sync.Mutex
}

func New(opts Options) *Crawler {
	if opts.DBDir == "" {
		opts.DBDir = "data"
	}
	if opts.QueuePath == "" {
		opts.QueuePath = "queue.txt"
	}
	if opts.ErrorLogPath == "" {
		opts.ErrorLogPath = "error_queue.log"
	}
	if opts.Throttle == 0 {
		opts.Throttle = 2 * time.Second
	}
	if opts.RecrawlWindow == 0 {
		opts.RecrawlWindow = 72 * time.Hour
	}
	if opts.MaxResponseMB == 0 {
		opts.MaxResponseMB = 10
	}
	return &Crawler{
		opts:    opts,
		seen:    make(map[string]struct{}, 1024),
		lastReq: make(map[string]time.Time),
	}
}

type pageMeta struct {
	URL         string    `json:"url"`
	LastCrawled time.Time `json:"last_crawled"`
	Status      string    `json:"status"`
	MIME        string    `json:"mime"`
	SizeBytes   int       `json:"size_bytes"`
	Version     int       `json:"version"`
}

// Run processes the queue file once (single pass)
func (c *Crawler) Run(ctx context.Context) error {
	qf, err := os.Open(c.opts.QueuePath)
	if err != nil {
		return fmt.Errorf("open queue: %w", err)
	}
	defer qf.Close()

	if err := os.MkdirAll(c.opts.DBDir, 0o755); err != nil {
		return fmt.Errorf("mkdir db: %w", err)
	}

	scanner := bufio.NewScanner(qf)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		u, canon, err := c.normalizeURL(line)
		if err != nil {
			// not a valid gemini URL, skip silently
			continue
		}
		if _, ok := c.seen[canon]; ok {
			continue
		}
		c.seen[canon] = struct{}{}

		host, id := pageID(u)
		should, err := c.shouldFetch(host, id)
		if err != nil {
			c.logError(canon, err)
			continue
		}
		if !should {
			// Skip recent page
			continue
		}

		if err := c.throttle(host); err != nil {
			return err
		}

		// Ensure URL for request contains default port
		reqURL, err := gemini.GetFullGeminiLink(u.String())
		if err != nil {
			c.logError(canon, err)
			_ = c.writeErrorMeta(host, id, u, "canon-error", 0)
			continue
		}

		resp, err := gemini.DoRequest(reqURL)
		if err != nil {
			c.logError(canon, err)
			_ = c.writeErrorMeta(host, id, u, "request-error", 0)
			continue
		}

		if resp.Status != gemini.StatusSuccess {
			c.logError(canon, fmt.Errorf("status %d: %s", resp.Status, resp.Meta))
			_ = c.writeErrorMeta(host, id, u, fmt.Sprintf("status-%d", resp.Status), len(resp.Body))
			continue
		}

		mime := resp.Meta
		if max := c.opts.MaxResponseMB; max > 0 && len(resp.Body) > max*1024*1024 {
			err := fmt.Errorf("response too large: %d bytes", len(resp.Body))
			c.logError(canon, err)
			_ = c.writeErrorMeta(host, id, u, "too-large", len(resp.Body))
			continue
		}

		if err := c.savePage(host, id, u, mime, resp.Body); err != nil {
			c.logError(canon, err)
			_ = c.writeErrorMeta(host, id, u, "save-error", len(resp.Body))
			continue
		}

		// Extract and append links for gemtext only
		if strings.HasPrefix(strings.ToLower(mime), gemini.GeminiMediaType) {
			links := extractLinks(u, resp.Body)
			if len(links) > 0 {
				c.appendToQueueDedup(links)
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("scan queue: %w", err)
	}
	return nil
}

// normalizeURL ensures gemini scheme, lowercased host, no fragment, non-empty path
func (c *Crawler) normalizeURL(raw string) (*url.URL, string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, "", err
	}
	if u.Scheme == "" {
		u.Scheme = "gemini"
	}
	if u.Scheme != "gemini" {
		return nil, "", fmt.Errorf("unsupported scheme: %s", u.Scheme)
	}
	u.Fragment = ""
	u.Host = strings.ToLower(u.Host)
	if u.Path == "" {
		u.Path = "/"
	}
	// Build canonical without default port
	canon := canonicalString(u)
	return u, canon, nil
}

func canonicalString(u *url.URL) string {
	// Strip default port 1965
	host := u.Host
	if strings.Contains(host, ":") {
		// If port is 1965, drop it
		if h, p, ok := strings.Cut(host, ":"); ok && (p == gemini.Port) {
			host = h
		}
	}
	// Rebuild without fragment (already cleared)
	var b strings.Builder
	b.WriteString("gemini://")
	b.WriteString(host)
	if u.Path == "" {
		b.WriteString("/")
	} else {
		b.WriteString(u.Path)
	}
	if u.RawQuery != "" {
		b.WriteString("?")
		b.WriteString(u.RawQuery)
	}
	return b.String()
}

func pageID(u *url.URL) (host, id string) {
	host = strings.ToLower(u.Host)
	// Drop default port in host dir name
	if h, p, ok := strings.Cut(host, ":"); ok && (p == gemini.Port) {
		host = h
	}
	canon := canonicalString(u)
	h := sha256.Sum256([]byte(canon))
	hash := hex.EncodeToString(h[:])
	slug := slugFromPath(u.Path)
	id = fmt.Sprintf("%s__%s", slug, hash)
	return host, id
}

var slugRe = regexp.MustCompile(`[^a-zA-Z0-9._-]+`)

func slugFromPath(p string) string {
	if p == "" || p == "/" {
		return "root"
	}
	parts := strings.Split(strings.TrimSuffix(p, "/"), "/")
	last := parts[len(parts)-1]
	last = slugRe.ReplaceAllString(last, "-")
	if len(last) > 80 {
		last = last[:80]
	}
	if last == "" || last == "-" {
		return "page"
	}
	return last
}

func (c *Crawler) hostDir(host string) string {
	return filepath.Join(c.opts.DBDir, host)
}

func (c *Crawler) pagesDir(host string) string {
	return filepath.Join(c.hostDir(host), "pages")
}

func (c *Crawler) metaPath(host, id string) string {
	return filepath.Join(c.pagesDir(host), id+".meta.json")
}

func (c *Crawler) contentPath(host, id, mime string) string {
	ext := ".bin"
	lm := strings.ToLower(mime)
	if strings.HasPrefix(lm, gemini.GeminiMediaType) {
		ext = ".gmi"
	} else if strings.HasPrefix(lm, "text/") {
		ext = ".txt"
	}
	return filepath.Join(c.pagesDir(host), id+ext)
}

func (c *Crawler) shouldFetch(host, id string) (bool, error) {
	mp := c.metaPath(host, id)
	b, err := os.ReadFile(mp)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return true, nil
		}
		return false, err
	}
	var m pageMeta
	if err := json.Unmarshal(b, &m); err != nil {
		return true, nil // malformed meta, try fetching anew
	}
	if time.Since(m.LastCrawled) < c.opts.RecrawlWindow {
		return false, nil
	}
	return true, nil
}

func (c *Crawler) throttle(host string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	if t, ok := c.lastReq[host]; ok {
		elapsed := now.Sub(t)
		if wait := c.opts.Throttle - elapsed; wait > 0 {
			// Unlock during sleep to avoid blocking other hosts in future concurrency
			c.mu.Unlock()
			time.Sleep(wait)
			c.mu.Lock()
		}
	}
	c.lastReq[host] = time.Now()
	return nil
}

func (c *Crawler) savePage(host, id string, u *url.URL, mime string, body []byte) error {
	if err := os.MkdirAll(c.pagesDir(host), 0o755); err != nil {
		return err
	}
	cp := c.contentPath(host, id, mime)
	tp := cp + ".tmp"
	if err := os.WriteFile(tp, body, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tp, cp); err != nil {
		return err
	}
	m := pageMeta{
		URL:         canonicalString(u),
		LastCrawled: time.Now().UTC(),
		Status:      "success",
		MIME:        mime,
		SizeBytes:   len(body),
		Version:     1,
	}
	mb, _ := json.MarshalIndent(&m, "", "  ")
	mp := c.metaPath(host, id)
	mtp := mp + ".tmp"
	if err := os.WriteFile(mtp, mb, 0o644); err != nil {
		return err
	}
	return os.Rename(mtp, mp)
}

func (c *Crawler) writeErrorMeta(host, id string, u *url.URL, status string, size int) error {
	if err := os.MkdirAll(c.pagesDir(host), 0o755); err != nil {
		return err
	}
	m := pageMeta{
		URL:         canonicalString(u),
		LastCrawled: time.Now().UTC(),
		Status:      status,
		MIME:        "",
		SizeBytes:   size,
		Version:     1,
	}
	mb, _ := json.MarshalIndent(&m, "", "  ")
	mp := c.metaPath(host, id)
	mtp := mp + ".tmp"
	if err := os.WriteFile(mtp, mb, 0o644); err != nil {
		return err
	}
	return os.Rename(mtp, mp)
}

func extractLinks(base *url.URL, body []byte) []string {
	text := string(body)
	lines := strings.Split(text, "\n")
	out := make([]string, 0, 16)
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "=>") {
			continue
		}
		line = strings.TrimSpace(strings.TrimPrefix(line, "=>"))
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		refRaw := fields[0]
		ref, err := url.Parse(refRaw)
		if err != nil {
			continue
		}
		abs := base.ResolveReference(ref)
		if abs.Scheme == "" {
			abs.Scheme = "gemini"
		}
		if abs.Scheme != "gemini" {
			continue
		}
		// Normalize host to lowercase to keep canonical form consistent
		abs.Host = strings.ToLower(abs.Host)
		abs.Fragment = ""
		if abs.Path == "" {
			abs.Path = "/"
		}
		out = append(out, canonicalString(abs))
	}
	return out
}

func (c *Crawler) appendToQueueDedup(urls []string) {
	f, err := os.OpenFile(c.opts.QueuePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	for _, u := range urls {
		if _, ok := c.seen[u]; ok {
			continue
		}
		c.seen[u] = struct{}{}
		_, _ = f.WriteString(u + "\n")
	}
}

func (c *Crawler) logError(urlStr string, err error) {
	_ = os.MkdirAll(filepath.Dir(c.opts.ErrorLogPath), 0o755)
	f, ferr := os.OpenFile(c.opts.ErrorLogPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if ferr != nil {
		return
	}
	defer f.Close()
	msg := strings.ReplaceAll(err.Error(), "\n", " ")
	line := fmt.Sprintf("%s\t%s\t%s\n", time.Now().UTC().Format(time.RFC3339), urlStr, msg)
	_, _ = f.WriteString(line)
}
