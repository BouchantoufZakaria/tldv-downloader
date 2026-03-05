package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ─────────────────────────────────────────────
//  CONFIG
// ─────────────────────────────────────────────

const (
	segmentWorkers  = 32   // concurrent HLS segment downloads
	batchWorkers    = 4    // concurrent meeting downloads (batch mode)
	requestTimeout  = 30   // seconds for API / playlist requests
	downloadTimeout = 3600 // seconds for full download
	maxRetries      = 3
	apiBase         = "https://gw.tldv.io/v1/meetings"
	userAgent       = "Mozilla/5.0 (Windows NT 10.0; Win64; x64)"
)

// ─────────────────────────────────────────────
//  DATA TYPES
// ─────────────────────────────────────────────

type MeetingInfo struct {
	Name      string
	Timestamp string
	SourceURL string
	RawData   map[string]interface{}
}

type Segment struct {
	Index int
	URL   string
}

type SegmentResult struct {
	Index int
	Path  string
	Err   error
}

type StreamVariant struct {
	Bandwidth int
	URL       string
}

// ─────────────────────────────────────────────
//  HTTP CLIENT
// ─────────────────────────────────────────────

func newHTTPClient() *http.Client {
	return &http.Client{Timeout: requestTimeout * time.Second}
}

func getWithAuth(client *http.Client, rawURL, token string) (*http.Response, error) {
	req, err := http.NewRequest("GET", rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", token)
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/json")
	return client.Do(req)
}

// ─────────────────────────────────────────────
//  AUTH TOKEN
// ─────────────────────────────────────────────

func prepareToken(token string) string {
	token = strings.TrimSpace(token)
	lo := strings.ToLower(token)
	if !strings.HasPrefix(lo, "bearer ") {
		token = "Bearer " + token
	}
	return token
}

// ─────────────────────────────────────────────
//  FILENAME SANITIZER
// ─────────────────────────────────────────────

var invalidChars = regexp.MustCompile(`[<>:"/\\|?*\x00-\x1f]`)
var multiUnder = regexp.MustCompile(`_{2,}`)

func sanitizeFilename(name string) string {
	s := invalidChars.ReplaceAllString(name, "_")
	s = multiUnder.ReplaceAllString(s, "_")
	s = strings.Trim(s, "_ ")
	if len(s) > 100 {
		s = s[:100]
	}
	if s == "" {
		return "TLDV_Meeting"
	}
	return s
}

// ─────────────────────────────────────────────
//  MEETING API
// ─────────────────────────────────────────────

func extractMeetingID(rawURL string) (string, error) {
	rawURL = strings.TrimRight(strings.TrimSpace(rawURL), "/")
	parts := strings.Split(rawURL, "/")
	id := parts[len(parts)-1]
	if len(id) < 10 {
		return "", fmt.Errorf("invalid meeting ID in URL: %s", rawURL)
	}
	return id, nil
}

func fetchMeetingData(client *http.Client, meetingID, token string) (map[string]interface{}, error) {
	apiURL := fmt.Sprintf("%s/%s/watch-page?noTranscript=true", apiBase, meetingID)
	resp, err := getWithAuth(client, apiURL, token)
	if err != nil {
		return nil, fmt.Errorf("network error: %w", err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case 401:
		return nil, fmt.Errorf("unauthorized – invalid auth token")
	case 404:
		return nil, fmt.Errorf("meeting not found (ID: %s)", meetingID)
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("API returned HTTP %d", resp.StatusCode)
	}

	var data map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return nil, fmt.Errorf("JSON decode error: %w", err)
	}
	return data, nil
}

func parseMeetingInfo(data map[string]interface{}) (*MeetingInfo, error) {
	meeting, _ := data["meeting"].(map[string]interface{})
	video, _ := data["video"].(map[string]interface{})

	name, _ := meeting["name"].(string)
	if name == "" {
		name = "TLDV_Meeting"
	}

	sourceURL, _ := video["source"].(string)
	if sourceURL == "" {
		return nil, fmt.Errorf("video source URL not found in meeting data")
	}

	createdAt, _ := meeting["createdAt"].(string)
	var t time.Time
	var err error
	if createdAt != "" {
		t, err = time.Parse("2006-01-02T15:04:05.000Z", createdAt)
		if err != nil {
			t, err = time.Parse(time.RFC3339, createdAt)
		}
	}
	if err != nil || createdAt == "" {
		t = time.Now()
	}

	return &MeetingInfo{
		Name:      sanitizeFilename(name),
		Timestamp: t.Format("2006-01-02_15-04-05"),
		SourceURL: sourceURL,
		RawData:   data,
	}, nil
}

// ─────────────────────────────────────────────
//  HLS / M3U8 PARSER
// ─────────────────────────────────────────────

// resolveURL makes relative HLS URLs absolute against a base URL
func resolveURL(base, ref string) string {
	if strings.HasPrefix(ref, "http://") || strings.HasPrefix(ref, "http://") || strings.HasPrefix(ref, "https://") {
		return ref
	}
	b, err := url.Parse(base)
	if err != nil {
		return ref
	}
	r, err := url.Parse(ref)
	if err != nil {
		return ref
	}
	return b.ResolveReference(r).String()
}

// parseMasterPlaylist returns stream variants sorted by bandwidth descending
func parseMasterPlaylist(content, baseURL string) []StreamVariant {
	lines := strings.Split(content, "\n")
	var variants []StreamVariant
	for i, line := range lines {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "#EXT-X-STREAM-INF:") {
			bw := 0
			for _, attr := range strings.Split(line[len("#EXT-X-STREAM-INF:"):], ",") {
				if strings.HasPrefix(attr, "BANDWIDTH=") {
					bw, _ = strconv.Atoi(strings.TrimPrefix(attr, "BANDWIDTH="))
				}
			}
			if i+1 < len(lines) {
				segURL := strings.TrimSpace(lines[i+1])
				if segURL != "" && !strings.HasPrefix(segURL, "#") {
					variants = append(variants, StreamVariant{
						Bandwidth: bw,
						URL:       resolveURL(baseURL, segURL),
					})
				}
			}
		}
	}
	sort.Slice(variants, func(i, j int) bool {
		return variants[i].Bandwidth > variants[j].Bandwidth
	})
	return variants
}

// parseMediaPlaylist returns ordered segment URLs
func parseMediaPlaylist(content, baseURL string) []string {
	var segments []string
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		segments = append(segments, resolveURL(baseURL, line))
	}
	return segments
}

// fetchPlaylist downloads a playlist text
func fetchPlaylist(client *http.Client, rawURL string) (string, error) {
	resp, err := client.Get(rawURL)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return string(b), err
}

// resolveSegments: given the top-level m3u8 URL, return all .ts / segment URLs
func resolveSegments(client *http.Client, m3u8URL string) ([]string, error) {
	content, err := fetchPlaylist(client, m3u8URL)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch playlist: %w", err)
	}

	// Is it a master playlist?
	if strings.Contains(content, "#EXT-X-STREAM-INF") {
		variants := parseMasterPlaylist(content, m3u8URL)
		if len(variants) == 0 {
			return nil, fmt.Errorf("master playlist has no stream variants")
		}
		bestURL := variants[0].URL
		fmt.Printf("   📡 Found %d quality levels – selecting highest bandwidth\n", len(variants))
		// Re-fetch the media playlist
		content, err = fetchPlaylist(client, bestURL)
		if err != nil {
			return nil, fmt.Errorf("failed to fetch media playlist: %w", err)
		}
		m3u8URL = bestURL
	}

	segments := parseMediaPlaylist(content, m3u8URL)
	if len(segments) == 0 {
		return nil, fmt.Errorf("no segments found in playlist")
	}
	return segments, nil
}

// ─────────────────────────────────────────────
//  SEGMENT DOWNLOADER  (the fast bit)
// ─────────────────────────────────────────────

func downloadSegment(client *http.Client, seg Segment, tmpDir string, retries int) SegmentResult {
	destPath := filepath.Join(tmpDir, fmt.Sprintf("seg_%06d.ts", seg.Index))

	for attempt := 0; attempt < retries; attempt++ {
		err := func() error {
			resp, err := client.Get(seg.URL)
			if err != nil {
				return err
			}
			defer resp.Body.Close()
			if resp.StatusCode != 200 {
				return fmt.Errorf("HTTP %d", resp.StatusCode)
			}
			f, err := os.Create(destPath)
			if err != nil {
				return err
			}
			defer f.Close()
			_, err = io.Copy(f, resp.Body)
			return err
		}()

		if err == nil {
			return SegmentResult{Index: seg.Index, Path: destPath}
		}
		if attempt < retries-1 {
			time.Sleep(time.Duration(attempt+1) * 500 * time.Millisecond)
		} else {
			return SegmentResult{Index: seg.Index, Err: fmt.Errorf("segment %d failed after %d retries: %w", seg.Index, retries, err)}
		}
	}
	return SegmentResult{Index: seg.Index, Err: fmt.Errorf("unreachable")}
}

// downloadAllSegments uses a worker pool to fetch all segments concurrently
func downloadAllSegments(client *http.Client, segments []string, tmpDir string) ([]string, error) {
	total := len(segments)
	results := make([]SegmentResult, total)

	jobs := make(chan Segment, total)
	resCh := make(chan SegmentResult, total)

	var downloaded int64

	// Progress printer
	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				fmt.Printf("\r   ⬇️  Segments: %d / %d (100%%)            \n", total, total)
				return
			case <-ticker.C:
				n := atomic.LoadInt64(&downloaded)
				pct := int(float64(n) / float64(total) * 100)
				bar := strings.Repeat("█", pct/5) + strings.Repeat("░", 20-pct/5)
				fmt.Printf("\r   ⬇️  [%s] %d/%d (%d%%)", bar, n, total, pct)
			}
		}
	}()

	// Spawn workers
	workers := segmentWorkers
	if workers > total {
		workers = total
	}
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			segClient := &http.Client{Timeout: 60 * time.Second}
			for seg := range jobs {
				r := downloadSegment(segClient, seg, tmpDir, maxRetries)
				atomic.AddInt64(&downloaded, 1)
				resCh <- r
			}
		}()
	}

	// Enqueue jobs
	for i, u := range segments {
		jobs <- Segment{Index: i, URL: u}
	}
	close(jobs)

	// Wait then close results
	go func() {
		wg.Wait()
		close(resCh)
	}()

	// Collect results
	var errs []string
	for r := range resCh {
		if r.Err != nil {
			errs = append(errs, r.Err.Error())
		} else {
			results[r.Index] = r
		}
	}
	close(done)

	if len(errs) > 0 {
		return nil, fmt.Errorf("%d segments failed:\n  %s", len(errs), strings.Join(errs, "\n  "))
	}

	// Return paths in order
	paths := make([]string, total)
	for i, r := range results {
		paths[i] = r.Path
	}
	return paths, nil
}

// ─────────────────────────────────────────────
//  MERGE WITH FFMPEG
// ─────────────────────────────────────────────

func mergeSegments(segmentPaths []string, outputFile, tmpDir string) error {
	// Write concat list
	listFile := filepath.Join(tmpDir, "concat_list.txt")
	f, err := os.Create(listFile)
	if err != nil {
		return err
	}
	for _, p := range segmentPaths {
		abs, _ := filepath.Abs(p)
		fmt.Fprintf(f, "file '%s'\n", strings.ReplaceAll(abs, "'", "'\\''"))
	}
	f.Close()

	fmt.Println("   🔗 Merging segments with ffmpeg...")
	cmd := exec.Command(
		"ffmpeg",
		"-f", "concat",
		"-safe", "0",
		"-i", listFile,
		"-c", "copy",
		"-y",
		outputFile,
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// ─────────────────────────────────────────────
//  MAIN DOWNLOAD FLOW
// ─────────────────────────────────────────────

func downloadVideo(rawURL, token, outputDir string) (string, error) {
	client := newHTTPClient()
	downloadClient := &http.Client{Timeout: downloadTimeout * time.Second}
	_ = downloadClient

	meetingID, err := extractMeetingID(rawURL)
	if err != nil {
		return "", err
	}

	fmt.Printf("   🔍 Meeting ID : %s\n", meetingID)

	// Fetch meeting metadata
	fmt.Println("   📡 Fetching meeting data from API...")
	data, err := fetchMeetingData(client, meetingID, token)
	if err != nil {
		return "", err
	}

	info, err := parseMeetingInfo(data)
	if err != nil {
		return "", err
	}

	fmt.Printf("   🎬 Title      : %s\n", info.Name)
	fmt.Printf("   📅 Date       : %s\n", info.Timestamp)
	fmt.Printf("   🔗 Source     : %s\n", info.SourceURL)

	// Ensure output directory
	if err := os.MkdirAll(outputDir, 0755); err != nil {
		return "", fmt.Errorf("cannot create output dir: %w", err)
	}

	outputFile := filepath.Join(outputDir, fmt.Sprintf("%s_%s.mp4", info.Timestamp, info.Name))

	// Create temp dir for segments
	tmpDir, err := os.MkdirTemp("", "tldv_segments_*")
	if err != nil {
		return "", fmt.Errorf("cannot create temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	// Resolve segments from HLS playlist
	fmt.Println("   📋 Parsing HLS playlist...")
	segments, err := resolveSegments(client, info.SourceURL)
	if err != nil {
		return "", fmt.Errorf("playlist error: %w", err)
	}
	fmt.Printf("   📦 Found %d segments – downloading with %d workers\n", len(segments), segmentWorkers)

	start := time.Now()
	segmentPaths, err := downloadAllSegments(client, segments, tmpDir)
	if err != nil {
		return "", fmt.Errorf("segment download failed: %w", err)
	}
	elapsed := time.Since(start)
	fmt.Printf("   ✅ All segments downloaded in %s\n", elapsed.Round(time.Second))

	// Merge
	if err := mergeSegments(segmentPaths, outputFile, tmpDir); err != nil {
		return "", fmt.Errorf("merge failed: %w", err)
	}

	// Stats
	stat, _ := os.Stat(outputFile)
	if stat != nil {
		fmt.Printf("   📊 Output size: %.2f MB\n", float64(stat.Size())/(1024*1024))
	}

	// Save metadata JSON
	jsonFile := filepath.Join(outputDir, fmt.Sprintf("%s_%s.json", info.Timestamp, info.Name))
	if b, err := json.MarshalIndent(info.RawData, "", "  "); err == nil {
		os.WriteFile(jsonFile, b, 0644)
		fmt.Printf("   💾 Metadata   : %s\n", jsonFile)
	}

	return outputFile, nil
}

// ─────────────────────────────────────────────
//  BATCH DOWNLOAD
// ─────────────────────────────────────────────

type BatchResult struct {
	URL     string
	File    string
	Elapsed time.Duration
	Err     error
}

func downloadBatch(urls []string, token, outputDir string, workers int) []BatchResult {
	jobs := make(chan string, len(urls))
	resultsCh := make(chan BatchResult, len(urls))

	for i := 0; i < workers; i++ {
		go func(id int) {
			for rawURL := range jobs {
				start := time.Now()
				fmt.Printf("\n[Worker %d] ▶ Starting: %s\n", id+1, rawURL)
				file, err := downloadVideo(rawURL, token, outputDir)
				resultsCh <- BatchResult{
					URL:     rawURL,
					File:    file,
					Elapsed: time.Since(start),
					Err:     err,
				}
			}
		}(i)
	}

	for _, u := range urls {
		jobs <- u
	}
	close(jobs)

	var results []BatchResult
	for range urls {
		results = append(results, <-resultsCh)
	}
	return results
}

// ─────────────────────────────────────────────
//  CLI HELPERS
// ─────────────────────────────────────────────

func prompt(reader *bufio.Reader, question string) string {
	fmt.Print(question)
	line, _ := reader.ReadString('\n')
	return strings.TrimSpace(line)
}

func checkFFmpeg() error {
	cmd := exec.Command("ffmpeg", "-version")
	cmd.Stdout = nil
	cmd.Stderr = nil
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("ffmpeg not found – please install it (see instructions below)")
	}
	return nil
}

func printHelp() {
	fmt.Println(`
📋  How to get your Authorization Token
────────────────────────────────────────
1. Go to https://tldv.io and log in
2. Open the meeting you want to download
3. Open DevTools  →  F12  (or right-click → Inspect)
4. Click the "Network" tab
5. Refresh the page  (F5)
6. In the filter box type:  watch-page
7. Click the request that contains "watch-page?noTranscript=true"
8. Go to Headers → Request Headers
9. Copy the full "Authorization" value  (starts with "Bearer …")

⚠️  The token expires when you log out.
`)
}

// ─────────────────────────────────────────────
//  ENTRY POINT
// ─────────────────────────────────────────────

func main() {
	fmt.Println("🎬 TLDV Downloader – Go Edition (concurrent HLS)")
	fmt.Printf("   OS: %s/%s  |  CPUs: %d\n", runtime.GOOS, runtime.GOARCH, runtime.NumCPU())
	fmt.Println(strings.Repeat("─", 55))

	// args shortcut: tldv_downloader --help
	if len(os.Args) > 1 && (os.Args[1] == "--help" || os.Args[1] == "-h") {
		printHelp()
		return
	}

	// Check ffmpeg
	if err := checkFFmpeg(); err != nil {
		fmt.Printf("❌ %v\n", err)
		fmt.Println("   Install: https://ffmpeg.org/download.html")
		os.Exit(1)
	}
	fmt.Println("✅ ffmpeg found")

	reader := bufio.NewReader(os.Stdin)

	batchMode := strings.ToLower(prompt(reader, "\n📋 Batch download mode? (y/N): ")) == "y"

	fmt.Println("\n" + strings.Repeat("─", 55))
	fmt.Println("🔑 Need help getting the auth token? Run with --help")
	fmt.Println(strings.Repeat("─", 55))
	token := prepareToken(prompt(reader, "\n🔐 Enter your Authorization token: "))
	if token == "" {
		fmt.Println("❌ Token is required")
		os.Exit(1)
	}

	outputDir := strings.TrimSpace(prompt(reader, "\n📁 Output directory (Enter = current dir): "))
	if outputDir == "" {
		outputDir = "."
	}

	if !batchMode {
		// ── Single download ──────────────────────────────
		rawURL := prompt(reader, "\n📎 Enter the TLDV meeting URL: ")
		if rawURL == "" {
			fmt.Println("❌ URL is required")
			os.Exit(1)
		}

		if strings.ToLower(prompt(reader, "\n❓ Proceed with download? (y/N): ")) != "y" {
			fmt.Println("Cancelled.")
			return
		}

		fmt.Println("\n" + strings.Repeat("─", 55))
		start := time.Now()
		file, err := downloadVideo(rawURL, token, outputDir)
		if err != nil {
			fmt.Printf("\n❌ Download failed: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("\n🎉 Done in %s  →  %s\n", time.Since(start).Round(time.Second), file)

	} else {
		// ── Batch download ───────────────────────────────
		fmt.Println("\n📎 Enter URLs (one per line, empty line to finish):")
		var urls []string
		for {
			u := prompt(reader, "   URL: ")
			if u == "" {
				break
			}
			urls = append(urls, u)
		}
		if len(urls) == 0 {
			fmt.Println("❌ No URLs provided")
			os.Exit(1)
		}

		workersStr := prompt(reader, fmt.Sprintf("\n🔧 Parallel meeting downloads (1-%d, default %d): ", batchWorkers*2, batchWorkers))
		workers := batchWorkers
		if n, err := strconv.Atoi(workersStr); err == nil && n > 0 {
			workers = n
		}

		fmt.Printf("\n📋 Ready: %d videos, %d workers, output → %s\n", len(urls), workers, outputDir)
		if strings.ToLower(prompt(reader, "❓ Proceed? (y/N): ")) != "y" {
			fmt.Println("Cancelled.")
			return
		}

		fmt.Println("\n" + strings.Repeat("─", 55))
		results := downloadBatch(urls, token, outputDir, workers)

		fmt.Println("\n" + strings.Repeat("─", 55))
		fmt.Println("📊 Batch Summary")
		fmt.Println(strings.Repeat("─", 55))
		ok, fail := 0, 0
		for _, r := range results {
			if r.Err != nil {
				fmt.Printf("❌ %s\n   Error: %v\n", r.URL, r.Err)
				fail++
			} else {
				fmt.Printf("✅ %s\n   File: %s  (%s)\n", r.URL, r.File, r.Elapsed.Round(time.Second))
				ok++
			}
		}
		fmt.Printf("\n🎉 %d succeeded, %d failed\n", ok, fail)
	}
}