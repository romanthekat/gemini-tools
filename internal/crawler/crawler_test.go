package crawler

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/romanthekat/gemini-tools/internal/gemini"
)

func newTestCrawler(t *testing.T, dir string) *Crawler {
	t.Helper()
	opts := Options{
		DBDir:         filepath.Join(dir, "db"),
		QueuePath:     filepath.Join(dir, "queue.txt"),
		ErrorLogPath:  filepath.Join(dir, "error.log"),
		Throttle:      100 * time.Millisecond,
		RecrawlWindow: 72 * time.Hour,
		MaxResponseKB: 10,
	}
	return New(opts, nil)
}

func TestNormalizeAndCanonicalAndPageID(t *testing.T) {
	c := New(Options{}, nil)

	u, canon, err := c.normalizeURL("gemini://Example.org:1965/foo/bar#frag")
	if err != nil {
		t.Fatalf("normalize error: %v", err)
	}
	if u.Scheme != "gemini" {
		t.Fatalf("scheme: %s", u.Scheme)
	}
	if u.Host != "example.org:1965" {
		t.Fatalf("host: %s", u.Host)
	}
	if u.Fragment != "" {
		t.Fatalf("fragment not removed: %s", u.Fragment)
	}
	if canon != "gemini://example.org/foo/bar" {
		t.Fatalf("canon: %s", canon)
	}

	// Default path
	_, canon2, err := c.normalizeURL("gemini://example.org")
	if err != nil {
		t.Fatalf("normalize2: %v", err)
	}
	if canon2 != "gemini://example.org/" {
		t.Fatalf("canon2: %s", canon2)
	}

	// pageID consistent with/without default port in input
	u1, _, _ := c.normalizeURL("gemini://example.org/path")
	u2, _, _ := c.normalizeURL("gemini://example.org:1965/path")
	h1, id1 := pageID(u1)
	h2, id2 := pageID(u2)
	if h1 != h2 || id1 != id2 {
		t.Fatalf("pageID mismatch: %s/%s vs %s/%s", h1, id1, h2, id2)
	}
}

func TestExtractLinks(t *testing.T) {
	base, _ := url.Parse("gemini://example.org:1965/dir/index.gmi")
	body := []byte(strings.Join([]string{
		"=> /abs",                        // absolute path on same host
		"=> rel",                         // relative path
		"=> ../up#frag Some text",        // parent path, drop fragment
		"=> gemini://Other.org/page?x=1", // absolute gemini
		"=> http://example.com/skip",     // not gemini
		"=> ?query-only",                 // query on current path
		"not a link",
	}, "\n"))

	var crawler *Crawler
	links := crawler.extractLinks(base, body)
	want := map[string]bool{
		"gemini://example.org/abs":                      true,
		"gemini://example.org/dir/rel":                  true,
		"gemini://example.org/up":                       true,
		"gemini://other.org/page?x=1":                   true,
		"gemini://example.org/dir/index.gmi?query-only": true,
	}
	if len(links) != len(want) {
		t.Fatalf("got %d links: %v", len(links), links)
	}
	for _, l := range links {
		if !want[l] {
			t.Errorf("unexpected link: %s", l)
		}
	}
}

func TestShouldFetch_RecrawlWindow(t *testing.T) {
	dir := t.TempDir()
	c := newTestCrawler(t, dir)

	u, canon, _ := c.normalizeURL("gemini://example.org/path")
	host, id := pageID(u)
	// Ensure meta directory exists for writing meta.json
	metaDir := filepath.Dir(c.metaPath(host, id))
	if err := os.MkdirAll(metaDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Write meta with recent crawl
	recent := pageMeta{
		URL:         canonicalString(u),
		LastCrawled: time.Now().UTC(),
		MIME:        gemini.GeminiMediaType,
		Status:      "success",
	}
	mb, _ := jsonMarshalIndent(recent)
	if err := os.WriteFile(c.metaPath(host, id), mb, 0o644); err != nil {
		t.Fatal(err)
	}

	should, err := c.shouldFetch(Job{u, canon, host, id})
	if err != nil {
		t.Fatal(err)
	}
	if should {
		t.Fatalf("expected shouldFetch=false for recent meta")
	}

	// Overwrite with old timestamp
	old := recent
	old.LastCrawled = time.Now().UTC().Add(-73 * time.Hour)
	mb, _ = jsonMarshalIndent(old)
	if err := os.WriteFile(c.metaPath(host, id), mb, 0o644); err != nil {
		t.Fatal(err)
	}

	//refresh seen map, as this link was already seen
	c.seen = make(map[string]struct{})
	should, err = c.shouldFetch(Job{u, canon, host, id})
	if err != nil {
		t.Fatal(err)
	}
	if !should {
		t.Fatalf("expected shouldFetch=true for old meta")
	}

	//now it was seen, shouldn't be fetched
	should, err = c.shouldFetch(Job{u, canon, host, id})
	if err != nil {
		t.Fatal(err)
	}
	if should {
		t.Fatalf("expected shouldFetch=false for old meta")
	}
}

// tiny wrapper to avoid importing encoding/json repeatedly here
func jsonMarshalIndent(v any) ([]byte, error) {
	return json.MarshalIndent(v, "", "  ")
}

func TestSavePage_WritesFilesAndMeta(t *testing.T) {
	dir := t.TempDir()
	c := newTestCrawler(t, dir)
	c.opts.RecrawlWindow = time.Hour

	u, canon, _ := c.normalizeURL("gemini://example.org/notes.gmi")
	host, id := pageID(u)
	content := []byte("=> /next\n# Title\n")
	mime := "text/gemini; charset=utf-8"
	if err := c.savePage(Job{u, canon, host, id}, mime, content); err != nil {
		t.Fatalf("savePage: %v", err)
	}

	contentPath, err := c.contentPath(host, id, mime)
	if err != nil {
		t.Fatalf("contentPath: %v", err)
	}
	if _, err := os.Stat(contentPath); err != nil {
		t.Fatalf("content missing: %v", err)
	}
	b, _ := os.ReadFile(contentPath)
	if string(b) != string(content) {
		t.Fatalf("content mismatch")
	}

	metaPath := c.metaPath(host, id)
	mb, err := os.ReadFile(metaPath)
	if err != nil {
		t.Fatalf("meta missing: %v", err)
	}
	var m pageMeta
	if err := json.Unmarshal(mb, &m); err != nil {
		t.Fatalf("meta json: %v", err)
	}
	if m.Status != "success" || !strings.HasPrefix(strings.ToLower(m.MIME), "text/gemini") {
		t.Fatalf("bad meta: %+v", m)
	}
	if m.SizeBytes != len(content) {
		t.Fatalf("size mismatch: %d", m.SizeBytes)
	}
}

func TestAppendToQueueDedup(t *testing.T) {
	dir := t.TempDir()
	c := newTestCrawler(t, dir)

	// Pre-seed seen with one URL
	c.seen["gemini://example.org/"] = struct{}{}
	urls := []string{"gemini://example.org/", "gemini://example.org/a", "gemini://example.org/b"}
	c.appendToQueueDedup(urls)

	f, err := os.Open(c.opts.QueuePath)
	if err != nil {
		t.Fatalf("open queue: %v", err)
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	lines := []string{}
	for s.Scan() {
		lines = append(lines, s.Text())
	}
	if err := s.Err(); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(lines) != 3 {
		t.Fatalf("expected 3 lines, got %d: %v", len(lines), lines)
	}
}

func TestLogError_Format(t *testing.T) {
	dir := t.TempDir()
	c := newTestCrawler(t, dir)

	c.logError("gemini://example.org/", fmt.Errorf("some\nmultiline error"))

	b, err := os.ReadFile(c.opts.ErrorLogPath)
	if err != nil {
		t.Fatalf("error log read: %v", err)
	}
	line := strings.TrimSpace(string(b))
	parts := strings.Split(line, "\t")
	if len(parts) != 3 {
		t.Fatalf("expected 3 fields, got %d: %q", len(parts), line)
	}
	if parts[1] != "gemini://example.org/" {
		t.Fatalf("url field: %s", parts[1])
	}
	if strings.Contains(parts[2], "\n") {
		t.Fatalf("message not sanitized: %q", parts[2])
	}
}

func TestThrottle_Waits(t *testing.T) {
	dir := t.TempDir()
	c := newTestCrawler(t, dir)
	c.opts.Throttle = 150 * time.Millisecond
	host := "example.org"
	c.lastReq[host] = time.Now()
	start := time.Now()
	if err := c.throttle(Job{nil, "", host, ""}); err != nil {
		t.Fatalf("throttle: %v", err)
	}
	elapsed := time.Since(start)
	if elapsed < 140*time.Millisecond {
		t.Fatalf("expected ~150ms wait, got %v", elapsed)
	}
}
