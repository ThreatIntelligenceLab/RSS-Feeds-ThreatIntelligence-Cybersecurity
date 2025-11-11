package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type Result struct {
	ID       int
	Domain   string
	FeedURL  string
	LastItem string
	Health   string // broken, healthy, not an rss feed
}

var dateTagRE = regexp.MustCompile(`(?is)<(?:pubDate|published|updated|dc:date)>(.*?)</(?:pubDate|published|updated|dc:date)>`)

func parseDateGuess(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	// remove any surrounding CDATA
	s = strings.TrimPrefix(s, "<![CDATA[")
	s = strings.TrimSuffix(s, "]]>")
	s = strings.TrimSpace(s)
	layouts := []string{
		time.RFC1123,
		time.RFC1123Z,
		time.RFC822,
		time.RFC822Z,
		time.RFC3339,
		time.RFC3339Nano,
		"Mon, 02 Jan 2006 15:04:05 MST",
		"2006-01-02 15:04:05",
		"02 Jan 2006",
	}
	var err error
	for _, l := range layouts {
		var t time.Time
		t, err = time.Parse(l, s)
		if err == nil {
			return t, nil
		}
	}
	// try parsing as RFC1123 with GMT fallback
	if t, e := time.Parse(time.RFC1123, s+" GMT"); e == nil {
		return t, nil
	}
	return time.Time{}, fmt.Errorf("unparseable date")
}

func inspectFeedBody(body string, contentType string) (isRSS bool, last string, health string) {
	lower := strings.ToLower(body)
	if strings.Contains(contentType, "html") || strings.Contains(lower, "<html") || strings.Contains(lower, "<!doctype html") {
		return false, "", "not an rss feed"
	}

	// detect RSS/Atom-like content
	if strings.Contains(lower, "<rss") || strings.Contains(lower, "<feed") || strings.Contains(lower, "<rdf:rdf") || strings.Contains(lower, "<item") || strings.Contains(lower, "<entry") {
		// try to extract dates
		matches := dateTagRE.FindAllStringSubmatch(body, -1)
		var latest time.Time
		for _, m := range matches {
			if len(m) < 2 {
				continue
			}
			if t, err := parseDateGuess(strings.TrimSpace(m[1])); err == nil {
				if t.After(latest) {
					latest = t
				}
			}
		}
		if !latest.IsZero() {
			return true, latest.UTC().Format(time.RFC3339), "healthy"
		}
		// no dates found but looks like a feed -> healthy
		return true, "", "healthy"
	}

	// otherwise treat as broken/unrecognized
	return false, "", "broken"
}

func main() {
	f, err := os.Open("rss_feeds.txt")
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to open rss_feeds.txt: %v\n", err)
		os.Exit(1)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	var urls []string
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "```") {
			continue
		}
		urls = append(urls, line)
	}
	if err := scanner.Err(); err != nil {
		fmt.Fprintf(os.Stderr, "error reading rss_feeds.txt: %v\n", err)
		os.Exit(1)
	}

	concurrency := 5
	sem := make(chan struct{}, concurrency)
	client := &http.Client{Timeout: 20 * time.Second}

	results := make([]Result, len(urls))
	var wg sync.WaitGroup
	var processed int32
	progressCh := make(chan string, len(urls))

	// printer goroutine: show progress in terminal as messages arrive
	go func() {
		for msg := range progressCh {
			fmt.Println(msg)
		}
	}()
	for i, u := range urls {
		wg.Add(1)
		sem <- struct{}{}
		go func(idx int, feedURL string) {
			defer wg.Done()
			defer func() { <-sem }()

			r := Result{ID: idx + 1, FeedURL: feedURL}
			if pu, err := url.Parse(feedURL); err == nil {
				r.Domain = pu.Host
			}

			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			req, err := http.NewRequestWithContext(ctx, "GET", feedURL, nil)
			if err != nil {
				r.Health = "broken"
				results[idx] = r
				return
			}
			req.Header.Set("User-Agent", "rss-health-checker/1.0")
			req.Header.Set("Accept", "application/rss+xml, application/atom+xml, application/xml, text/xml, */*")

			// request only the first chunk to keep memory and bandwidth low
			req.Header.Set("Range", "bytes=0-262143") // 256KiB
			resp, err := client.Do(req)
			if err != nil {
				r.Health = "broken"
				results[idx] = r
				return
			}
			defer resp.Body.Close()

			if resp.StatusCode >= 400 {
				r.Health = "broken"
				results[idx] = r
				return
			}

			// Read a limited amount of the body (we only need to detect feed & dates)
			const maxRead = 256 * 1024 // 256KiB
			lr := io.LimitReader(resp.Body, maxRead)
			data, err := io.ReadAll(lr)
			if err != nil {
				r.Health = "broken"
				results[idx] = r
				return
			}

			contentType := strings.ToLower(resp.Header.Get("Content-Type"))
			isRSS, last, health := inspectFeedBody(string(data), contentType)
			r.Health = health
			if isRSS {
				r.LastItem = last
			}

			// clear sensitive/large temporary memory ASAP
			for i := range data {
				data[i] = 0
			}
			data = nil

			results[idx] = r

			// report progress
			n := atomic.AddInt32(&processed, 1)
			progressCh <- fmt.Sprintf("%s  %d/%d  %s  ->  %s", time.Now().Format(time.RFC3339), n, len(urls), r.FeedURL, r.Health)
		}(i, u)
	}

	wg.Wait()
	// all work done, close progress channel so printer goroutine can exit
	close(progressCh)
	// Sort results by health (preferred order: healthy, not an rss feed, broken)
	rank := map[string]int{
		"healthy":         0,
		"not an rss feed": 1,
		"broken":          2,
	}
	getRank := func(h string) int {
		if h == "" {
			return rank["broken"]
		}
		if v, ok := rank[h]; ok {
			return v
		}
		// unknown -> treat as broken
		return rank["broken"]
	}

	sort.SliceStable(results, func(i, j int) bool {
		ri := getRank(results[i].Health)
		rj := getRank(results[j].Health)
		if ri != rj {
			return ri < rj
		}
		// tie-breaker: domain then feed url
		if results[i].Domain != results[j].Domain {
			return results[i].Domain < results[j].Domain
		}
		return results[i].FeedURL < results[j].FeedURL
	})

	// Print markdown table (reassign sequential ids for sorted output)
	outFile := "rss_health.md"
	fout, err := os.Create(outFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to create %s: %v\n", outFile, err)
	}
	var writer *bufio.Writer
	if fout != nil {
		writer = bufio.NewWriter(fout)
	}

	header := "| id | domain | rss_feed_url | last_item_date | health |"
	sep := "|---|---|---|---|---|"
	// write header to both terminal and file (if available)
	fmt.Println(header)
	fmt.Println(sep)
	if writer != nil {
		fmt.Fprintln(writer, header)
		fmt.Fprintln(writer, sep)
	}

	for i, r := range results {
		id := i + 1
		urlEscaped := strings.ReplaceAll(r.FeedURL, "|", "%7C")
		domain := r.Domain
		if domain == "" {
			domain = "-"
		}
		last := r.LastItem
		if last == "" {
			last = "-"
		}
		health := r.Health
		if health == "" {
			health = "broken"
		}
		line := fmt.Sprintf("| %d | %s | %s | %s | %s |", id, domain, urlEscaped, last, health)
		fmt.Println(line)
		if writer != nil {
			fmt.Fprintln(writer, line)
		}
	}

	if writer != nil {
		writer.Flush()
		fout.Close()
		fmt.Printf("Wrote markdown results to %s\n", outFile)
	}
}
