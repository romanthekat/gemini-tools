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

type Options struct {
	DBDir         string
	QueuePath     string
	ErrorLogPath  string
	Throttle      time.Duration
	RecrawlWindow time.Duration
	MaxResponseKB int
	Workers       int
}

type Crawler struct {
	ctx context.Context
	wg  sync.WaitGroup

	opts    Options
	seen    map[string]struct{}
	lastReq map[string]time.Time //TODO replace with Host type or IP address

	jobsCandidates   chan RawJob
	workersHostsList []map[Host]struct{}
	workersJobsList  []chan Job

	seenMu      sync.Mutex // protects seen map
	lastReqMu   sync.Mutex // protects lastReq map
	fileQueueMu sync.Mutex // protects queue file append operations
}

func New(opts Options, ctx context.Context) *Crawler {
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
		opts.Throttle = 1500 * time.Millisecond
	}
	if opts.RecrawlWindow == 0 {
		opts.RecrawlWindow = 72 * time.Hour
	}
	if opts.MaxResponseKB == 0 {
		opts.MaxResponseKB = 512
	}
	if opts.Workers <= 0 {
		opts.Workers = 4
	}

	var workersJobsList []chan Job
	for i := 0; i < opts.Workers; i++ {
		workersJobsList = append(workersJobsList, make(chan Job, 2048))
	}

	var workersHostsList []map[Host]struct{}
	for i := 0; i < opts.Workers; i++ {
		workersHostsList = append(workersHostsList, make(map[Host]struct{}))
	}

	return &Crawler{
		ctx:              ctx,
		opts:             opts,
		seen:             make(map[string]struct{}, 4096),
		lastReq:          make(map[string]time.Time),
		jobsCandidates:   make(chan RawJob, 8192),
		workersJobsList:  workersJobsList,
		workersHostsList: workersHostsList,
	}
}

const PermissionsFull = 0o755
const PermissionsNonExecutable = 0o644

type pageMeta struct {
	URL         string    `json:"url"`
	LastCrawled time.Time `json:"last_crawled"`
	Status      string    `json:"status"`
	MIME        string    `json:"mime"`
	SizeBytes   int       `json:"size_bytes"`
	Version     int       `json:"version"`
}

type RawJob string
type Host string

type Job struct {
	link      *url.URL
	canonical string

	host string
	id   string
}

// Run processes the queue and continues while new items are added (single worker)
func (c *Crawler) Run() error {
	queue, err := c.readFileQueue()
	if err != nil {
		return err
	}

	go c.startJobsCandidatesProcessor()
	go c.processInitialQueue(queue)
	go c.scheduledPrintWorkersStats()
	c.startWorkers()

	c.wg.Wait()

	return nil
}

func (c *Crawler) startJobsCandidatesProcessor() {
	for jobCandidate := range c.jobsCandidates {
		select {
		case <-c.ctx.Done():
			return
		default:
		}

		err := c.processJobCandidate(jobCandidate)
		if err != nil {
			//TODO skip errors for now, but can be useful to analyse later
			//fmt.Println(err)
		}
	}
}

func (c *Crawler) readFileQueue() ([]string, error) {
	queueFile, err := os.Open(c.opts.QueuePath)
	if err != nil {
		return nil, fmt.Errorf("open queue: %w", err)
	}
	defer queueFile.Close()

	if err := os.MkdirAll(c.opts.DBDir, PermissionsFull); err != nil {
		return nil, fmt.Errorf("mkdir db: %w", err)
	}

	// Load initial queue into memory
	queue, err := c.getQueue(queueFile)
	if err != nil {
		return nil, err
	}
	return queue, nil
}

func (c *Crawler) startWorkers() {
	for i := range c.opts.Workers {
		//fmt.Printf("starting worker %d\n", i)
		//wg.Go(worker(workersJobsList[i], wg, c, i))

		c.wg.Go(func() {
			c.worker(i, c.workersJobsList[i])
		})
	}
}

func (c *Crawler) processJobCandidate(job RawJob) error {
	jobString := string(job)

	//gemini://)/
	if len(job) <= 10 {
		return fmt.Errorf("rejecting invalid URL: %s", job)
	}

	if strings.HasPrefix(string(job), "gemini://!") {
		return fmt.Errorf("rejecting invalid URL: %s", job)
	}

	link, canonical, err := c.normalizeURL(jobString)
	//TODO only get geminiLink? gemini.GetFullGeminiLink
	if err != nil {
		//TODO add to ignore list?
		return fmt.Errorf("error: invalid URL: %s", job)
	}

	if strings.HasSuffix(jobString, ".pdf") ||
		strings.HasSuffix(jobString, ".zip") ||
		strings.HasSuffix(jobString, ".jpg") ||
		strings.HasSuffix(jobString, ".png") ||
		strings.HasSuffix(jobString, ".bin") {
		return fmt.Errorf("rejecting binary files for now: %s", job)
	}

	//TODO workaround to avoid humongous sites
	geoguessGamePastFirstCountry := strings.Contains(jobString, "gemini://gemi.dev") &&
		strings.Contains(jobString, "cgi-bin/witw.cgi/game") &&
		!strings.Contains(jobString, "?,")
	if geoguessGamePastFirstCountry ||
		//strings.Contains(jobString, "gemini://gmi.noulin.net") ||
		strings.Contains(jobString, "gemini://musicbrainz.uploadedlobster.com") ||
		strings.Contains(jobString, "gemini://git.thebackupbox.net") {
		return fmt.Errorf("rejected due to custom rules: %s", job)
	}

	host, id := pageID(link)
	workerNumber := c.findWorkerToDoTheJob(host)

	c.wg.Go(func() {
		c.workersJobsList[workerNumber] <- Job{
			link:      link,
			canonical: canonical,
			host:      host,
			id:        id,
		}
	})

	return nil
}

func (c *Crawler) processInitialQueue(queue []string) {
	for jobNum := 0; jobNum < len(queue); jobNum++ {
		select {
		case <-c.ctx.Done():
			return
		default:
		}

		if jobNum%100_000 == 0 {
			fmt.Printf("❗ file queue processing progress: %d out of %d\n", jobNum, len(queue))
		}

		job := queue[jobNum]
		c.jobsCandidates <- RawJob(job)
	}
}

func (c *Crawler) scheduledPrintWorkersStats() {
	t := time.NewTicker(5 * time.Second)
	for {
		select {
		case <-t.C:
			{
				fmt.Printf("workers stats:\n")
				totalActiveWorkers := 0

				for i, jobs := range c.workersJobsList {
					jobsLength := len(jobs)
					if jobsLength > 0 {
						fmt.Printf("worker %d has %d jobs\n", i, jobsLength)
						totalActiveWorkers += 1
					}
				}
				//fmt.Printf("total jobs in queue: %d\n", len(jobs))
				fmt.Printf("total active workers: %d\n", totalActiveWorkers)
				fmt.Println()
			}
		case <-c.ctx.Done():
			return
		default:
		}
	}
}

func (c *Crawler) findWorkerToDoTheJob(host string) int {
	//not correct in concurrent env, but good enough as an approximation
	var minJobs int
	var minJobsWorkerNum int

	for workerNum, workersHosts := range c.workersHostsList {
		queueLen := len(c.workersJobsList[workerNum])
		if queueLen <= minJobs {
			minJobs = queueLen
			minJobsWorkerNum = workerNum
		}

		if _, servesThisHost := workersHosts[Host(host)]; servesThisHost {
			return workerNum
		}
	}

	// if no one has served this host - make a random worker pick it up
	c.workersHostsList[minJobsWorkerNum][Host(host)] = struct{}{}
	return minJobsWorkerNum
}

func (c *Crawler) worker(number int, jobs <-chan Job) {
	for job := range jobs {
		should, err := c.shouldFetch(job)
		if err != nil {
			fmt.Printf("error: %s %v\n", job.canonical, err)
			c.logError(job.canonical, err)
			continue
		}
		if !should {
			//fmt.Printf("skip - too early to refresh: %s (remaining %d)\n", canonicalLink, remaining(linkNum))
			continue
		}

		if err := c.throttle(job); err != nil {
			return
		}

		fmt.Printf("fetching: %s\n", job.canonical)
		err, status, length := c.doRequest(job)
		if err != nil {
			c.logError(job.canonical, err)
			_ = c.writeErrorMeta(job, status, length)
		}
	}

	fmt.Printf("job channel for worker is closed")
}

func (c *Crawler) doRequest(job Job) (error, string, int) {
	// Ensure URL for request contains default port
	reqURL, err := gemini.GetFullGeminiLink(job.canonical)
	if err != nil {
		return err, "job.canonical-error", 0
	}

	resp, err := gemini.DoRequest(reqURL)
	if err != nil {
		return err, "request-error", 0
	}

	responseLength := len(resp.Body)

	if resp.Status != gemini.StatusSuccess {
		err := fmt.Errorf("status %d: %s", resp.Status, resp.Meta)
		return err, fmt.Sprintf("status-%d", resp.Status), responseLength
	}

	textualResponse := strings.Contains(job.canonical, ".gmi") ||
		strings.Contains(job.canonical, ".txt")
	if !textualResponse {
		if max := c.opts.MaxResponseKB; max > 0 && responseLength > max*1024 {
			err := fmt.Errorf("response too large: %d bytes", responseLength)
			return err, "too-large", responseLength
		}
	}

	mime := resp.Meta
	if err := c.savePage(job, mime, resp.Body); err != nil {
		return err, "save-error", responseLength
	}

	//fmt.Printf("saved: %s/%s %s %dB\n", host, id, mime, responseLength)
	c.processBody(job, resp)

	return nil, "", 0
}

func (c *Crawler) processBody(job Job, resp *gemini.Response) {
	// Extract and append links for gemtext only
	if strings.HasPrefix(strings.ToLower(resp.Meta), gemini.GeminiMediaType) {
		links := c.extractLinks(job.link, resp.Body)
		added := 0
		if len(links) > 0 {
			toAdd := make([]string, 0, len(links))
			for _, link := range links {
				if c.checkSeen(link) {
					continue
				}

				toAdd = append(toAdd, link)
				c.jobsCandidates <- RawJob(link)
			}

			//TODO should only append canonical (non-rejected) urls, this impl looks wonky
			if len(toAdd) > 0 {
				c.appendToQueueDedup(toAdd)
				added = len(toAdd)
			}
		}
		if added > 0 {
			fmt.Printf("discovered %d links (added %d)\n", len(links), added)
		}
	}
}

func (c *Crawler) getQueue(queueFile *os.File) ([]string, error) {
	queue := make([]string, 0, 1024)
	scanner := bufio.NewScanner(queueFile)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		queue = append(queue, line)
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan queue: %w", err)
	}

	return queue, nil
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
	canonicalLink := canonicalString(u)

	hashBytes := sha256.Sum256([]byte(canonicalLink))
	hash := hex.EncodeToString(hashBytes[:])

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
	return filepath.Join(c.pagesDir(host), "meta", id+".meta.json")
}

func (c *Crawler) contentPath(host, id, mime string) (string, error) {
	ext := ".bin"
	mimeLower := strings.ToLower(mime)
	if strings.HasPrefix(mimeLower, gemini.GeminiMediaType) {
		ext = ".gmi"
	} else if strings.HasPrefix(mimeLower, "text/") {
		ext = ".txt"
	} else if strings.HasPrefix(mimeLower, "image/jpeg") {
		ext = ".jpg"
	} else if strings.HasPrefix(mimeLower, "image/png") {
		ext = ".png"
	}
	return filepath.Join(c.pagesDir(host), id+ext), nil
}

func (c *Crawler) shouldFetch(job Job) (bool, error) {
	//seen in this session
	if c.checkSeen(job.canonical) {
		// already processed/queued in this run
		return false, nil
	}
	c.addSeen(job.canonical)

	//check already in db
	metaPath := c.metaPath(job.host, job.id)
	bytes, err := os.ReadFile(metaPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return true, nil
		}
		return false, err
	}

	var meta pageMeta
	if err := json.Unmarshal(bytes, &meta); err != nil {
		return true, nil // malformed meta, try fetching anew
	}

	//do not recrawl non-gemini files (e.g. images)
	if !strings.HasPrefix(strings.ToLower(meta.MIME), gemini.GeminiMediaType) {
		return false, nil
	}

	if time.Since(meta.LastCrawled) < c.opts.RecrawlWindow {
		return false, nil
	}
	return true, nil
}

// TODO both IP instead of host?
func (c *Crawler) throttle(job Job) error {
	c.lastReqMu.Lock()
	defer c.lastReqMu.Unlock()

	now := time.Now()
	if lastRequested, ok := c.lastReq[job.host]; ok {
		elapsed := now.Sub(lastRequested)
		if wait := c.opts.Throttle - elapsed; wait > 0 {
			// Unlock during sleep to avoid blocking other hosts in future concurrency
			time.Sleep(wait)
		}
	}

	c.lastReq[job.host] = time.Now()
	return nil
}

func (c *Crawler) savePage(job Job, mime string, body []byte) error {
	if err := os.MkdirAll(c.pagesDir(job.host), PermissionsFull); err != nil {
		return err
	}
	contentPath, err := c.contentPath(job.host, job.id, mime)
	if err != nil {
		return err
	}
	contentPathTemp := contentPath + ".tmp"
	if err := os.WriteFile(contentPathTemp, body, PermissionsNonExecutable); err != nil {
		return err
	}
	if err := os.Rename(contentPathTemp, contentPath); err != nil {
		return err
	}

	meta := pageMeta{
		URL:         job.canonical,
		LastCrawled: time.Now().UTC(),
		Status:      "success",
		MIME:        mime,
		SizeBytes:   len(body),
		Version:     1,
	}

	metaBytes, _ := json.MarshalIndent(&meta, "", "  ")
	metaPath := c.metaPath(job.host, job.id)
	// ensure meta directory exists
	if err := os.MkdirAll(filepath.Dir(metaPath), PermissionsFull); err != nil {
		return err
	}

	metaPathTemp := metaPath + ".tmp"
	if err := os.WriteFile(metaPathTemp, metaBytes, PermissionsNonExecutable); err != nil {
		return err
	}
	return os.Rename(metaPathTemp, metaPath)
}

// TODO unify with savePage meta section?
func (c *Crawler) writeErrorMeta(job Job, status string, size int) error {
	if err := os.MkdirAll(c.pagesDir(job.host), PermissionsFull); err != nil {
		return err
	}
	meta := pageMeta{
		URL:         job.canonical,
		LastCrawled: time.Now().UTC(),
		Status:      status,
		MIME:        "",
		SizeBytes:   size,
		Version:     1,
	}

	metaBytes, _ := json.MarshalIndent(&meta, "", "  ")
	metaPath := c.metaPath(job.host, job.id)
	// ensure meta directory exists
	if err := os.MkdirAll(filepath.Dir(metaPath), PermissionsFull); err != nil {
		return err
	}

	metaPathTemp := metaPath + ".tmp"
	if err := os.WriteFile(metaPathTemp, metaBytes, PermissionsNonExecutable); err != nil {
		return err
	}
	return os.Rename(metaPathTemp, metaPath)
}

func (c *Crawler) extractLinks(base *url.URL, body []byte) []string {
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

		_, canon, err := c.normalizeURL(abs.String())
		if err != nil {
			continue
		}

		out = append(out, canon)
	}
	return out
}

func (c *Crawler) addSeen(link string) {
	c.seenMu.Lock()
	defer c.seenMu.Unlock()
	c.seen[link] = struct{}{}
}

func (c *Crawler) appendToQueueDedup(urls []string) {
	c.fileQueueMu.Lock()
	defer c.fileQueueMu.Unlock()
	f, err := os.OpenFile(c.opts.QueuePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, PermissionsNonExecutable)
	if err != nil {
		return
	}
	defer f.Close()
	// The caller already performed in-run deduplication using c.seen and built the list.
	for _, u := range urls {
		_, canonical, err := c.normalizeURL(u)
		if err != nil {
			continue
		}

		_, _ = f.WriteString(canonical + "\n")
	}
}

func (c *Crawler) logError(urlStr string, err error) {
	_ = os.MkdirAll(filepath.Dir(c.opts.ErrorLogPath), PermissionsFull)
	file, fileErr := os.OpenFile(c.opts.ErrorLogPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, PermissionsNonExecutable)
	if fileErr != nil {
		return
	}

	defer file.Close()
	msg := strings.ReplaceAll(err.Error(), "\n", " ")
	line := fmt.Sprintf("%s\t%s\t%s\n", urlStr, time.Now().UTC().Format(time.RFC3339), msg)
	_, err = file.WriteString(line)
}

func (c *Crawler) checkSeen(link string) bool {
	c.seenMu.Lock()
	defer c.seenMu.Unlock()
	_, ok := c.seen[link]
	return ok
}
