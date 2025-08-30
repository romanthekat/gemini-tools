package main

import (
	"context"
	"flag"
	"fmt"
	"time"

	"github.com/romanthekat/gemini-tools/internal/crawler"
)

func main() {
	var (
		queuePath    = flag.String("queue", "queue.txt", "path to queue file (one URL per line)")
		dbDir        = flag.String("db", "data", "database root directory")
		errorLogPath = flag.String("error-log", "error_queue.log", "path to error log file")
		throttleMS   = flag.Int("throttle-ms", 1500, "per-host minimum interval between requests in milliseconds")
		recrawlHours = flag.Int("recrawl-hours", 24*32, "do not recrawl a page within this many hours")
		maxRespKB    = flag.Int("max-kb", 500, "maximum response size to save (in KB)")
		workers      = flag.Int("workers", 4, "number of concurrent workers")
	)
	flag.Parse()

	opts := crawler.Options{
		DBDir:         *dbDir,
		QueuePath:     *queuePath,
		ErrorLogPath:  *errorLogPath,
		Throttle:      time.Duration(*throttleMS) * time.Millisecond,
		RecrawlWindow: time.Duration(*recrawlHours) * time.Hour,
		MaxResponseKB: *maxRespKB,
		Workers:       *workers,
	}

	c := crawler.New(opts, context.Background())
	if err := c.Run(); err != nil {
		fmt.Println("crawler error:", err)
	}
}
