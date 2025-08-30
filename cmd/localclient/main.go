package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/romanthekat/gemini-tools/internal/gemini"
)

const (
	LinkPrefix    = "=>"
	Header1Prefix = "#"
	Header2Prefix = "##"
	Header3Prefix = "###"
)

// meta schema matches crawler
type pageMeta struct {
	URL         string    `json:"url"`
	LastCrawled time.Time `json:"last_crawled"`
	Status      string    `json:"status"`
	MIME        string    `json:"mime"`
	SizeBytes   int       `json:"size_bytes"`
	Version     int       `json:"version"`
}

type State struct {
	Links   []string
	History []string
	// current page canonical URL
	Current *url.URL
}

func (s *State) clearLinks() { s.Links = make([]string, 0, 100) }
func NewState() *State       { return &State{make([]string, 0, 100), make([]string, 0, 100), nil} }

var (
	dbDir     string
	queuePath string
)

func main() {
	flag.StringVar(&dbDir, "db", "data", "database root directory")
	flag.StringVar(&queuePath, "queue", "queue.txt", "path to queue file to append missing links")
	flag.Parse()

	reader := bufio.NewReader(os.Stdin)
	state := NewState()
	printHelp()

	for {
		input, err := getUserInput(reader)
		if err != nil {
			fmt.Println("user input read failed:", err)
			os.Exit(-1)
		}

		link, doNothing, err := processUserInput(input, state)
		if err != nil {
			fmt.Println("error processing user input:", err)
			continue
		}
		if doNothing {
			continue
		}

		if err := openLocal(state, link); err != nil {
			fmt.Println("\u001B[31m", err.Error(), "\u001B[0m") // red
			// append to queue
			canon := canonicalString(link)
			appendToQueue(canon)
			continue
		}
	}
}

func printHelp() {
	fmt.Println("gemini://url\topen url from local DB (append to queue if missing)")
	fmt.Println("number\t\topen link from current page by number")
	fmt.Println("b\t\tgo back")
	fmt.Println("q\t\tquit")
	fmt.Println("h\t\tprint this summary")
	fmt.Println("g\t\topen Project Gemini homepage")
	fmt.Println("t\t\tshow top 20 sites in local DB")
	fmt.Println()
}

func getUserInput(reader *bufio.Reader) (string, error) {
	fmt.Print("🔴➡ ")
	input, err := reader.ReadString('\n')
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(input), nil
}

func processUserInput(input string, state *State) (*url.URL, bool, error) {
	linkRaw := ""
	switch input {
	case "":
		return nil, true, nil
	case "q":
		os.Exit(0)
	case "h":
		printHelp()
		return nil, true, nil
	case "g":
		linkRaw = "gemini://geminiprotocol.net/"
	case "t":
		if err := showTop(state); err != nil {
			fmt.Println("\u001B[31m", err.Error(), "\u001B[0m")
		}
		return nil, true, nil
	case "b":
		if len(state.History) < 2 {
			fmt.Println("\u001B[31mNo history yet\u001B[0m")
			return nil, true, nil
		}
		linkRaw = state.History[len(state.History)-2]
		state.History = state.History[:len(state.History)-2]
		fmt.Println(">", linkRaw)
	default:
		// Treat it as link number first
		if idx, err := strconv.Atoi(input); err == nil {
			if idx > len(state.Links) || idx <= 0 {
				fmt.Println("\u001B[31mNo link with this number\u001B[0m")
				return nil, true, nil
			}
			linkRaw = state.Links[idx-1]
			fmt.Println(">", linkRaw)
		} else {
			// Treat this as a URL
			linkRaw = input
			if !strings.HasPrefix(strings.ToLower(linkRaw), "gemini://") {
				linkRaw = "gemini://" + linkRaw
			}
		}
	}
	link, err := url.Parse(linkRaw)
	if err != nil {
		return nil, false, fmt.Errorf("error parsing URL: %w", err)
	}
	if link.Scheme == "" {
		link.Scheme = "gemini"
	}
	if link.Scheme != "gemini" {
		return nil, false, fmt.Errorf("unsupported scheme: %s", link.Scheme)
	}
	if link.Path == "" {
		link.Path = "/"
	}
	link.Fragment = ""
	// Normalize host lowercase
	link.Host = strings.ToLower(link.Host)
	return link, false, nil
}

func openLocal(state *State, link *url.URL) error {
	host, id := pageID(link)
	metaPath := filepath.Join(dbDir, host, "pages", "meta", id+".meta.json")
	mb, err := os.ReadFile(metaPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("not found in local DB: %s", canonicalString(link))
		}
		return fmt.Errorf("read meta failed: %w", err)
	}
	var m pageMeta
	if err := json.Unmarshal(mb, &m); err != nil {
		return fmt.Errorf("invalid meta: %w", err)
	}
	mime := strings.ToLower(m.MIME)
	ext := ".bin"
	if strings.HasPrefix(mime, gemini.GeminiMediaType) {
		ext = ".gmi"
	} else if strings.HasPrefix(mime, "text/") {
		ext = ".txt"
	} else if strings.HasPrefix(mime, "image/jpeg") {
		ext = ".jpg"
	} else if strings.HasPrefix(mime, "image/png") {
		ext = ".png"
	}
	contentPath := filepath.Join(dbDir, host, "pages", id+ext)
	cb, err := os.ReadFile(contentPath)
	if err != nil {
		return fmt.Errorf("content missing: %w", err)
	}
	// Display
	body := string(cb)
	if strings.HasPrefix(mime, "text/gemini") {
		state.clearLinks()
		pre := false
		for _, line := range strings.Split(body, "\n") {
			if strings.HasPrefix(line, "```") {
				fmt.Println(line)
				pre = !pre
			} else if pre {
				fmt.Println(line)
			} else if strings.HasPrefix(line, LinkPrefix) {
				if err := processLink(state, link, line); err != nil {
					return err
				}
			} else if strings.HasPrefix(line, Header3Prefix) {
				fmt.Printf("\u001B[33m%s\u001B[0m\n", line) // orange
			} else if strings.HasPrefix(line, Header2Prefix) {
				fmt.Printf("\u001B[32m%s\u001B[0m\n", line) // green
			} else if strings.HasPrefix(line, Header1Prefix) {
				fmt.Printf("\u001B[31m%s\u001B[0m\n", line) // red
			} else {
				fmt.Println(line)
			}
		}
	} else if strings.HasPrefix(mime, "text/") {
		fmt.Print(body)
	} else {
		fmt.Printf("\u001B[31munsupported type: %s\u001B[0m\n", m.MIME)
	}
	state.History = append(state.History, link.String())
	state.Current = link
	return nil
}

func processLink(state *State, base *url.URL, line string) error {
	line = strings.TrimSpace(strings.TrimPrefix(line, LinkPrefix))
	parts := strings.Fields(line)
	if len(parts) == 0 {
		return nil
	}
	parsedLink, err := url.Parse(parts[0])
	if err != nil {
		return fmt.Errorf("parsing link failed: %w", err)
	}
	abs := base.ResolveReference(parsedLink)
	if abs.Scheme == "" {
		abs.Scheme = "gemini"
	}
	if abs.Path == "" {
		abs.Path = "/"
	}
	abs.Host = strings.ToLower(abs.Host)
	abs.Fragment = ""
	absoluteLink := abs.String()
	var linkNum string
	if len(parts) == 1 {
		linkNum = absoluteLink
	} else {
		linkNum = strings.Join(parts[1:], " ")
	}
	state.Links = append(state.Links, absoluteLink)
	fmt.Printf("[%d] \u001B[34m%s\u001B[0m\n", len(state.Links), linkNum) // blue
	return nil
}

// -------- mapping URL to local ID (mirrors crawler) --------

func canonicalString(u *url.URL) string {
	host := u.Host
	if h, p, ok := strings.Cut(host, ":"); ok && p == "1965" {
		host = h
	}
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

func pageID(u *url.URL) (host, id string) {
	host = strings.ToLower(u.Host)
	if h, p, ok := strings.Cut(host, ":"); ok && p == "1965" {
		host = h
	}
	canon := canonicalString(u)
	h := sha256.Sum256([]byte(canon))
	hash := hex.EncodeToString(h[:])
	slug := slugFromPath(u.Path)
	id = fmt.Sprintf("%s__%s", slug, hash)
	return host, id
}

func appendToQueue(canon string) {
	f, err := os.OpenFile(queuePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.WriteString(canon + "\n")
}

// showTop lists top 20 hosts in local DB by number of saved pages
func showTop(state *State) error {
	entries, err := os.ReadDir(dbDir)
	if err != nil {
		return fmt.Errorf("read db dir failed: %w", err)
	}
	type item struct {
		host  string
		count int
	}
	items := make([]item, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		host := e.Name()
		pagesPath := filepath.Join(dbDir, host, "pages")
		pEntries, err := os.ReadDir(pagesPath)
		if err != nil {
			// skip hosts without pages dir
			continue
		}
		cnt := 0
		for _, pe := range pEntries {
			if pe.IsDir() {
				// skip meta/ or any other directories
				continue
			}
			name := pe.Name()
			if strings.HasSuffix(name, ".tmp") {
				continue
			}
			// meta is stored under pages/meta/, so normal page files live directly under pages/
			cnt++
		}
		if cnt > 0 {
			items = append(items, item{host: host, count: cnt})
		}
	}
	if len(items) == 0 {
		fmt.Println("No pages found in local DB")
		state.clearLinks()
		return nil
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].count == items[j].count {
			return items[i].host < items[j].host
		}
		return items[i].count > items[j].count
	})

	state.clearLinks()
	fmt.Println("Top sites by pages:")
	limit := 20
	if len(items) < limit {
		limit = len(items)
	}
	for i := 0; i < limit; i++ {
		it := items[i]
		link := "gemini://" + it.host + "/"
		state.Links = append(state.Links, link)
		fmt.Printf("[%d] \u001B[34m%s\u001B[0m (%d pages)\n", i+1, it.host, it.count)
	}
	return nil
}
