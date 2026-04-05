package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand"
	"mime"
	"mime/multipart"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/h2non/bimg"
	"github.com/joho/godotenv"
	"github.com/redis/go-redis/v9"
	"golang.org/x/sync/singleflight"
)

const (
	slackChannelID  = "CDLBHGUQN"
	defaultPort     = "8080"
	defaultRedis    = "redis.hack-club-cat-placeholder-catapi.svc.cluster.local:6379"
	refreshInterval = 30 * time.Minute
	cdnUploadURL    = "https://cdn.hackclub.com/api/v4/upload"

	redisKeyCounter = "cats:counter"
	redisKeySet     = "cats:slack_file_ids"
	redisKeyHash    = "cats:images"
	redisKeyList    = "cats:urls"

	memoryCacheMax = 512
	memoryCacheTTL = 30 * time.Minute
	redisCacheTTL  = 24 * time.Hour
	srcCacheMax    = 128
)

type CatImage struct {
	CDNURL   string `json:"cdn_url"`
	MimeType string `json:"mime_type"`
	Name     string `json:"name"`
}

type SlackResponse struct {
	OK               bool           `json:"ok"`
	Messages         []SlackMessage `json:"messages"`
	HasMore          bool           `json:"has_more"`
	ResponseMetadata struct {
		NextCursor string `json:"next_cursor"`
	} `json:"response_metadata"`
	Error string `json:"error"`
}
type SlackMessage struct {
	Files []SlackFile `json:"files"`
}
type SlackFile struct {
	ID                 string `json:"id"`
	Name               string `json:"name"`
	Mimetype           string `json:"mimetype"`
	URLPrivate         string `json:"url_private"`
	URLPrivateDownload string `json:"url_private_download"`
}
type CDNUploadResponse struct {
	URL   string `json:"url"`
	Error string `json:"error"`
}
type FileInfoResponse struct {
	OK   bool `json:"ok"`
	File struct {
		URLPrivateDownload string `json:"url_private_download"`
		URLPrivate         string `json:"url_private"`
	} `json:"file"`
	Error string `json:"error"`
}

type cacheEntry struct {
	data        []byte
	contentType string
	createdAt   time.Time
}

var (
	images     []CatImage
	imagesMu   sync.RWMutex
	slackToken string
	cdnToken   string
	rdb        *redis.Client
	ctx        = context.Background()

	cdnClient = &http.Client{
		Timeout: 15 * time.Second,
		Transport: &http.Transport{
			MaxIdleConns:        100,
			MaxIdleConnsPerHost: 50,
			IdleConnTimeout:     90 * time.Second,
			DialContext: (&net.Dialer{
				Timeout:   5 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
		},
	}

	procCache   = make(map[string]*cacheEntry)
	procCacheMu sync.RWMutex
	procKeys    []string

	srcCache   = make(map[string]*cacheEntry)
	srcCacheMu sync.RWMutex
	srcKeys    []string

	sfGroup singleflight.Group
)

func main() {
	godotenv.Load()

	slackToken = os.Getenv("SLACK_BOT_TOKEN")
	if slackToken == "" {
		log.Fatal("SLACK_BOT_TOKEN environment variable is required")
	}
	cdnToken = os.Getenv("CDN_API_KEY")
	if cdnToken == "" {
		log.Fatal("CDN_API_KEY environment variable is required")
	}

	port := os.Getenv("PORT")
	if port == "" {
		port = defaultPort
	}

	redisAddr := os.Getenv("REDIS_ADDR")
	if redisAddr == "" {
		redisAddr = defaultRedis
	}

	rdb = redis.NewClient(&redis.Options{
		Addr:     redisAddr,
		PoolSize: 20,
	})
	if err := rdb.Ping(ctx).Err(); err != nil {
		log.Fatalf("Failed to connect to Redis at %s: %v", redisAddr, err)
	}
	log.Printf("Connected to Redis at %s", redisAddr)

	loadFromRedis()

	if err := syncImages(); err != nil {
		log.Printf("Warning: initial sync failed: %v", err)
	}

	imagesMu.RLock()
	log.Printf("Ready with %d cat images", len(images))
	imagesMu.RUnlock()

	go func() {
		syncTicker := time.NewTicker(refreshInterval)
		cleanTicker := time.NewTicker(5 * time.Minute)
		defer syncTicker.Stop()
		defer cleanTicker.Stop()
		for {
			select {
			case <-syncTicker.C:
				if err := syncImages(); err != nil {
					log.Printf("Error syncing images: %v", err)
				}
			case <-cleanTicker.C:
				evictExpired()
			}
		}
	}()

	http.HandleFunc("/", handleIndex)
	http.HandleFunc("/cat", handleRandomCat)
	http.HandleFunc("/cat.json", handleRandomCatJSON)
	http.HandleFunc("/cats.json", handleAllCatsJSON)
	http.HandleFunc("/health", handleHealth)

	log.Printf("Serving cats on :%s", port)
	log.Fatal(http.ListenAndServe(":"+port, nil))
}

func cacheKey(cdnURL string, w, h int, format string) string {
	raw := fmt.Sprintf("%s|%d|%d|%s", cdnURL, w, h, format)
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:16])
}

func procCacheGet(key string) (*cacheEntry, bool) {
	procCacheMu.RLock()
	defer procCacheMu.RUnlock()
	e, ok := procCache[key]
	if !ok {
		return nil, false
	}
	if time.Since(e.createdAt) > memoryCacheTTL {
		return nil, false
	}
	return e, true
}

func procCacheSet(key string, data []byte, ct string) {
	procCacheMu.Lock()
	defer procCacheMu.Unlock()
	for len(procCache) >= memoryCacheMax && len(procKeys) > 0 {
		old := procKeys[0]
		procKeys = procKeys[1:]
		delete(procCache, old)
	}
	procCache[key] = &cacheEntry{data: data, contentType: ct, createdAt: time.Now()}
	procKeys = append(procKeys, key)
}

func srcCacheGet(cdnURL string) ([]byte, bool) {
	srcCacheMu.RLock()
	defer srcCacheMu.RUnlock()
	e, ok := srcCache[cdnURL]
	if !ok {
		return nil, false
	}
	if time.Since(e.createdAt) > memoryCacheTTL {
		return nil, false
	}
	return e.data, true
}

func srcCacheSet(cdnURL string, data []byte) {
	srcCacheMu.Lock()
	defer srcCacheMu.Unlock()
	for len(srcCache) >= srcCacheMax && len(srcKeys) > 0 {
		old := srcKeys[0]
		srcKeys = srcKeys[1:]
		delete(srcCache, old)
	}
	srcCache[cdnURL] = &cacheEntry{data: data, createdAt: time.Now()}
	srcKeys = append(srcKeys, cdnURL)
}

func evictExpired() {
	now := time.Now()

	procCacheMu.Lock()
	var newProcKeys []string
	for _, k := range procKeys {
		if e, ok := procCache[k]; ok && now.Sub(e.createdAt) > memoryCacheTTL {
			delete(procCache, k)
		} else {
			newProcKeys = append(newProcKeys, k)
		}
	}
	procKeys = newProcKeys
	procCacheMu.Unlock()

	srcCacheMu.Lock()
	var newSrcKeys []string
	for _, k := range srcKeys {
		if e, ok := srcCache[k]; ok && now.Sub(e.createdAt) > memoryCacheTTL {
			delete(srcCache, k)
		} else {
			newSrcKeys = append(newSrcKeys, k)
		}
	}
	srcKeys = newSrcKeys
	srcCacheMu.Unlock()
}

func redisCacheKey(key string) string {
	return "cats:proc:" + key
}

func redisCacheGet(key string) ([]byte, string, bool) {
	val, err := rdb.Get(ctx, redisCacheKey(key)).Bytes()
	if err != nil {
		return nil, "", false
	}
	idx := bytes.IndexByte(val, '\n')
	if idx < 0 {
		return nil, "", false
	}
	return val[idx+1:], string(val[:idx]), true
}

func redisCacheSet(key string, data []byte, ct string) {
	val := append([]byte(ct+"\n"), data...)
	rdb.Set(ctx, redisCacheKey(key), val, redisCacheTTL)
}

func fetchSourceImage(cdnURL string) ([]byte, error) {
	if data, ok := srcCacheGet(cdnURL); ok {
		return data, nil
	}

	resp, err := cdnClient.Get(cdnURL)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("CDN returned %d", resp.StatusCode)
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	srcCacheSet(cdnURL, data)
	return data, nil
}

func loadFromRedis() {
	all, err := rdb.HGetAll(ctx, redisKeyHash).Result()
	if err != nil {
		log.Printf("Warning: failed to load from Redis: %v", err)
		return
	}
	var imgs []CatImage
	for _, v := range all {
		var img CatImage
		if err := json.Unmarshal([]byte(v), &img); err != nil {
			continue
		}
		imgs = append(imgs, img)
	}
	imagesMu.Lock()
	images = imgs
	imagesMu.Unlock()
	log.Printf("Loaded %d images from Redis", len(imgs))
}

func nextCatNumber() (int64, error) {
	return rdb.Incr(ctx, redisKeyCounter).Result()
}

func extForMime(mimeType string) string {
	exts, _ := mime.ExtensionsByType(mimeType)
	if len(exts) > 0 {
		for _, e := range exts {
			if e == ".png" || e == ".jpg" || e == ".jpeg" || e == ".gif" || e == ".webp" {
				return e
			}
		}
		return exts[0]
	}
	switch {
	case strings.Contains(mimeType, "png"):
		return ".png"
	case strings.Contains(mimeType, "gif"):
		return ".gif"
	case strings.Contains(mimeType, "webp"):
		return ".webp"
	default:
		return ".jpg"
	}
}

func syncImages() error {
	slackFiles, err := fetchSlackImages()
	if err != nil {
		return err
	}

	newCount := 0
	for _, f := range slackFiles {
		exists, err := rdb.SIsMember(ctx, redisKeySet, f.ID).Result()
		if err != nil {
			log.Printf("Redis error checking %s: %v", f.ID, err)
			continue
		}
		if exists {
			continue
		}

		num, err := nextCatNumber()
		if err != nil {
			log.Printf("Redis counter error: %v", err)
			continue
		}

		ext := extForMime(f.Mimetype)
		catName := fmt.Sprintf("cat%d%s", num, ext)

		cdnURL, err := uploadToCDN(f, catName)
		if err != nil {
			log.Printf("Failed to upload %s to CDN: %v", f.ID, err)
			rdb.Decr(ctx, redisKeyCounter)
			continue
		}

		img := CatImage{CDNURL: cdnURL, MimeType: f.Mimetype, Name: catName}
		imgJSON, _ := json.Marshal(img)

		pipe := rdb.Pipeline()
		pipe.SAdd(ctx, redisKeySet, f.ID)
		pipe.HSet(ctx, redisKeyHash, f.ID, string(imgJSON))
		pipe.RPush(ctx, redisKeyList, cdnURL)
		if _, err := pipe.Exec(ctx); err != nil {
			log.Printf("Redis save error for %s: %v", f.ID, err)
			continue
		}

		imagesMu.Lock()
		images = append(images, img)
		imagesMu.Unlock()

		newCount++
		log.Printf("Uploaded %s -> %s (%s)", f.ID, cdnURL, catName)
	}

	if newCount > 0 {
		imagesMu.RLock()
		log.Printf("Synced %d new images (total: %d)", newCount, len(images))
		imagesMu.RUnlock()
	}
	return nil
}

func fetchSlackImages() ([]SlackFile, error) {
	var allFiles []SlackFile
	cursor := ""
	for {
		u := fmt.Sprintf("https://slack.com/api/conversations.history?channel=%s&limit=200", slackChannelID)
		if cursor != "" {
			u += "&cursor=" + cursor
		}
		req, err := http.NewRequest("GET", u, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+slackToken)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return nil, fmt.Errorf("slack API request failed: %w", err)
		}
		var slackResp SlackResponse
		if err := json.NewDecoder(resp.Body).Decode(&slackResp); err != nil {
			resp.Body.Close()
			return nil, fmt.Errorf("failed to decode slack response: %w", err)
		}
		resp.Body.Close()
		if !slackResp.OK {
			return nil, fmt.Errorf("slack API error: %s", slackResp.Error)
		}
		for _, msg := range slackResp.Messages {
			for _, f := range msg.Files {
				if strings.HasPrefix(f.Mimetype, "image/") {
					allFiles = append(allFiles, f)
				}
			}
		}
		if !slackResp.HasMore || slackResp.ResponseMetadata.NextCursor == "" {
			break
		}
		cursor = slackResp.ResponseMetadata.NextCursor
	}
	return allFiles, nil
}

func downloadFromSlack(fileID string) ([]byte, error) {
	infoURL := fmt.Sprintf("https://slack.com/api/files.info?file=%s", fileID)
	req, err := http.NewRequest("GET", infoURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+slackToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("files.info request failed: %w", err)
	}
	defer resp.Body.Close()
	var info FileInfoResponse
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return nil, fmt.Errorf("failed to decode files.info: %w", err)
	}
	if !info.OK {
		return nil, fmt.Errorf("files.info error: %s", info.Error)
	}
	dlURL := info.File.URLPrivateDownload
	if dlURL == "" {
		dlURL = info.File.URLPrivate
	}
	if dlURL == "" {
		return nil, fmt.Errorf("no download URL in files.info response")
	}
	dlReq, err := http.NewRequest("GET", dlURL, nil)
	if err != nil {
		return nil, err
	}
	dlReq.Header.Set("Authorization", "Bearer "+slackToken)
	dlResp, err := http.DefaultClient.Do(dlReq)
	if err != nil {
		return nil, fmt.Errorf("download failed: %w", err)
	}
	defer dlResp.Body.Close()
	if dlResp.StatusCode != http.StatusOK {
		u, _ := url.Parse(dlURL)
		q := u.Query()
		q.Set("t", slackToken)
		u.RawQuery = q.Encode()
		dlResp2, err := http.DefaultClient.Get(u.String())
		if err != nil {
			return nil, fmt.Errorf("download failed (both methods): %w", err)
		}
		defer dlResp2.Body.Close()
		if dlResp2.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("download failed: status %d / %d", dlResp.StatusCode, dlResp2.StatusCode)
		}
		ct := dlResp2.Header.Get("Content-Type")
		if strings.HasPrefix(ct, "text/html") {
			return nil, fmt.Errorf("slack returned HTML instead of image")
		}
		return io.ReadAll(dlResp2.Body)
	}
	ct := dlResp.Header.Get("Content-Type")
	if strings.HasPrefix(ct, "text/html") {
		return nil, fmt.Errorf("slack returned HTML instead of image")
	}
	return io.ReadAll(dlResp.Body)
}

func uploadToCDN(f SlackFile, filename string) (string, error) {
	imgData, err := downloadFromSlack(f.ID)
	if err != nil {
		return "", fmt.Errorf("download from slack: %w", err)
	}
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	part, err := writer.CreateFormFile("file", filename)
	if err != nil {
		return "", err
	}
	if _, err := part.Write(imgData); err != nil {
		return "", err
	}
	writer.Close()
	req, err := http.NewRequest("POST", cdnUploadURL, &buf)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+cdnToken)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("CDN request failed: %w", err)
	}
	defer resp.Body.Close()
	var cdnResp CDNUploadResponse
	if err := json.NewDecoder(resp.Body).Decode(&cdnResp); err != nil {
		return "", fmt.Errorf("failed to decode CDN response: %w", err)
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return "", fmt.Errorf("CDN upload failed (%d): %s", resp.StatusCode, cdnResp.Error)
	}
	if cdnResp.URL == "" {
		return "", fmt.Errorf("CDN returned empty URL")
	}
	return cdnResp.URL, nil
}

func bimgType(format string) bimg.ImageType {
	switch format {
	case "png":
		return bimg.PNG
	case "gif":
		return bimg.GIF
	case "webp":
		return bimg.WEBP
	default:
		return bimg.JPEG
	}
}

func contentTypeFor(format string) string {
	switch format {
	case "png":
		return "image/png"
	case "gif":
		return "image/gif"
	case "webp":
		return "image/webp"
	default:
		return "image/jpeg"
	}
}

func processImage(data []byte, width, height int, format string) ([]byte, string, error) {
	opts := bimg.Options{
		Type:    bimgType(format),
		Quality: 80,
	}

	if width > 0 && height > 0 {
		opts.Width = width
		opts.Height = height
		opts.Crop = true
		opts.Gravity = bimg.GravitySmart
	} else if width > 0 {
		opts.Width = width
	} else if height > 0 {
		opts.Height = height
	}

	out, err := bimg.NewImage(data).Process(opts)
	if err != nil {
		return nil, "", err
	}

	return out, contentTypeFor(format), nil
}

func parseFormat(r *http.Request) string {
	f := strings.ToLower(r.URL.Query().Get("format"))
	switch f {
	case "png", "jpg", "jpeg", "gif", "webp":
		if f == "jpeg" {
			return "jpg"
		}
		return f
	}
	return ""
}

func sourceFormat(mimeType string) string {
	switch {
	case strings.Contains(mimeType, "png"):
		return "png"
	case strings.Contains(mimeType, "gif"):
		return "gif"
	case strings.Contains(mimeType, "webp"):
		return "webp"
	default:
		return "jpg"
	}
}

func handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	imagesMu.RLock()
	count := len(images)
	imagesMu.RUnlock()
	now := time.Now().UnixMilli()
	fmt.Fprintf(w, `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Placeholder Cats</title>
<link rel="preconnect" href="https://fonts.googleapis.com">
<link rel="preconnect" href="https://fonts.gstatic.com" crossorigin>
<link href="https://fonts.googleapis.com/css2?family=Plus+Jakarta+Sans:wght@400;500;600;700&family=Space+Mono:wght@400;700&display=swap" rel="stylesheet">
<style>
*,*::before,*::after{box-sizing:border-box;margin:0;padding:0}

:root{
  color-scheme:light;
  --bg:#ffffff;--bg-alt:#fdf7f8;--card:#fdf7f8;
  --border:#f3dfe2;--text:#5d3a3f;--text-muted:#857073;
  --primary:#c93a4f;--primary-hover:#b33346;--primary-light:#fce8ea;
  --link:#962b3a;--code-bg:#1f2937;--code-text:#10b981;
  --surface:#fdfafa;
}
@media(prefers-color-scheme:dark){
  :root{
    color-scheme:dark;
    --bg:#1d1d1f;--bg-alt:#212121;--card:#212121;
    --border:#262627;--text:#f9e8ea;--text-muted:#dcb8be;
    --primary:#c93a4f;--primary-hover:#e87485;--primary-light:#2d2d2f;
    --link:#f3a8b2;--code-bg:#0f172a;--code-text:#34d399;
    --surface:#1d1d1f;
  }
}

body{
  font-family:'Plus Jakarta Sans',system-ui,-apple-system,sans-serif;
  background:linear-gradient(165deg,var(--bg) 0%%,var(--surface) 100%%);
  color:var(--text);min-height:100vh;-webkit-font-smoothing:antialiased;
}
a{color:var(--link);text-decoration:none}
a:hover{opacity:.85}

.container{max-width:56rem;margin:0 auto;padding:2.5rem 1.5rem}

/* Hero */
.hero{text-align:center;padding:3rem 0 2rem}
.hero h1{font-size:2.25rem;font-weight:700;letter-spacing:-.03em;margin-bottom:.5rem}
.hero .accent{color:var(--primary)}
.hero p{color:var(--text-muted);font-size:1.05rem;max-width:28rem;margin:0 auto}
.hero .count{
  display:inline-flex;align-items:center;gap:.375rem;
  background:var(--primary-light);color:var(--primary);
  font-size:.8rem;font-weight:600;padding:.25rem .75rem;
  border-radius:9999px;margin-bottom:1rem;
}

/* Cat preview */
.preview{
  display:flex;justify-content:center;gap:1rem;flex-wrap:wrap;
  margin:2rem 0;
}
.preview img{
  width:180px;height:180px;object-fit:cover;border-radius:1rem;
  border:1px solid var(--border);
  box-shadow:0 4px 24px -4px rgba(0,0,0,.08);
  transition:transform .2s;
}
.preview img:hover{transform:scale(1.04)}

/* Code */
code{
  font-family:'Space Mono',monospace;font-size:.8rem;
  background:var(--code-bg);color:var(--code-text);
  padding:.15rem .4rem;border-radius:.3rem;
}
pre{
  background:var(--code-bg);color:var(--code-text);
  font-family:'Space Mono',monospace;font-size:.78rem;
  padding:1rem 1.25rem;border-radius:.75rem;overflow-x:auto;
  line-height:1.6;margin:.75rem 0;
}

/* Endpoints table */
.endpoints{margin:2rem 0}
.endpoints h2{font-size:1.25rem;font-weight:700;letter-spacing:-.02em;margin-bottom:1rem}
.endpoint{
  display:flex;align-items:baseline;gap:.75rem;
  padding:.6rem 0;border-bottom:1px solid var(--border);
}
.endpoint:last-child{border-bottom:none}
.endpoint code{flex-shrink:0}
.endpoint span{font-size:.8rem;color:var(--text-muted)}

/* Section */
.section{margin:2.5rem 0}
.section h2{font-size:1.25rem;font-weight:700;letter-spacing:-.02em;margin-bottom:.75rem}

/* Badges row */
.formats{display:flex;gap:.5rem;flex-wrap:wrap;margin:.5rem 0}
.fmt{
  display:inline-block;font-size:.75rem;font-weight:600;
  padding:.25rem .65rem;border-radius:9999px;
  background:var(--primary-light);color:var(--primary);
}

/* Footer */
.footer{
  text-align:center;padding:2rem 0 1rem;
  font-size:.78rem;color:var(--text-muted);
  border-top:1px solid var(--border);margin-top:3rem;
}
</style>
</head>
<body>
<div class="container">
  <div class="hero">
    <div class="count">%d cats</div>
    <h1>Placeholder <span class="accent">Cats</span></h1>
    <p>Meow?.</p>
  </div>

  <div class="preview">
    <img src="/cat?w=180&h=180&format=webp&_=%[2]d1" alt="cat" loading="eager">
    <img src="/cat?w=180&h=180&format=webp&_=%[2]d2" alt="cat" loading="eager">
    <img src="/cat?w=180&h=180&format=webp&_=%[2]d3" alt="cat" loading="eager">
  </div>

  <div class="section">
    <h2>Quick Start</h2>
    <pre>&lt;img src="https://placeholder.cat/cat?w=400&amp;h=300&amp;format=webp" alt="cat"&gt;</pre>
  </div>

  <div class="endpoints">
    <h2>Endpoints</h2>
    <div class="endpoint"><code>/cat</code> <span>Random cat image, proxied through the server</span></div>
    <div class="endpoint"><code>/cat?w=400&amp;h=300</code> <span>Resized &amp; smart&#8209;cropped to exact dimensions</span></div>
    <div class="endpoint"><code>/cat?w=200</code> <span>Scale to width, preserve aspect ratio</span></div>
    <div class="endpoint"><code>/cat?h=150</code> <span>Scale to height, preserve aspect ratio</span></div>
    <div class="endpoint"><code>/cat?format=webp</code> <span>Convert to any supported format</span></div>
    <div class="endpoint"><code>/cat.json</code> <span>Random cat metadata as JSON</span></div>
    <div class="endpoint"><code>/cats.json</code> <span>All cat images as JSON</span></div>
    <div class="endpoint"><code>/health</code> <span>Health check with cache stats</span></div>
  </div>

  <div class="section">
    <h2>Formats</h2>
    <div class="formats">
      <span class="fmt">JPEG</span>
      <span class="fmt">PNG</span>
      <span class="fmt">WebP</span>
      <span class="fmt">GIF</span>
    </div>
  </div>

  <div class="footer">
    No cats were harmed in the making of this API. They were, however, placed in holders.<br>
  </div>
</div>
</body>
</html>`, count, now)
}

func handleRandomCat(w http.ResponseWriter, r *http.Request) {
	imagesMu.RLock()
	if len(images) == 0 {
		imagesMu.RUnlock()
		http.Error(w, "no cat images available", http.StatusServiceUnavailable)
		return
	}
	img := images[rand.Intn(len(images))]
	imagesMu.RUnlock()

	width, _ := strconv.Atoi(r.URL.Query().Get("w"))
	height, _ := strconv.Atoi(r.URL.Query().Get("h"))
	format := parseFormat(r)

	if width < 0 {
		width = 0
	}
	if height < 0 {
		height = 0
	}
	if width > 4096 {
		width = 4096
	}
	if height > 4096 {
		height = 4096
	}

	needsProcessing := width > 0 || height > 0 || format != ""

	if !needsProcessing {
		data, err := fetchSourceImage(img.CDNURL)
		if err != nil {
			http.Error(w, "failed to fetch image", http.StatusBadGateway)
			return
		}
		ct := img.MimeType
		if ct == "" {
			ct = "image/jpeg"
		}
		w.Header().Set("Content-Type", ct)
		w.Header().Set("Cache-Control", "public, max-age=3600")
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		w.Write(data)
		return
	}

	if format == "" {
		format = sourceFormat(img.MimeType)
	}
	key := cacheKey(img.CDNURL, width, height, format)

	if entry, ok := procCacheGet(key); ok {
		w.Header().Set("Content-Type", entry.contentType)
		w.Header().Set("Cache-Control", "public, max-age=3600")
		w.Header().Set("Content-Length", strconv.Itoa(len(entry.data)))
		w.Header().Set("X-Cache", "memory")
		w.Write(entry.data)
		return
	}

	if data, ct, ok := redisCacheGet(key); ok {
		procCacheSet(key, data, ct)
		w.Header().Set("Content-Type", ct)
		w.Header().Set("Cache-Control", "public, max-age=3600")
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		w.Header().Set("X-Cache", "redis")
		w.Write(data)
		return
	}

	type result struct {
		data []byte
		ct   string
	}

	val, err, _ := sfGroup.Do(key, func() (interface{}, error) {
		srcData, err := fetchSourceImage(img.CDNURL)
		if err != nil {
			return nil, err
		}

		output, contentType, err := processImage(srcData, width, height, format)
		if err != nil {
			return nil, err
		}

		procCacheSet(key, output, contentType)
		go redisCacheSet(key, output, contentType)

		return &result{data: output, ct: contentType}, nil
	})

	if err != nil {
		http.Error(w, "failed to process image: "+err.Error(), http.StatusInternalServerError)
		return
	}

	res := val.(*result)
	w.Header().Set("Content-Type", res.ct)
	w.Header().Set("Cache-Control", "public, max-age=3600")
	w.Header().Set("Content-Length", strconv.Itoa(len(res.data)))
	w.Header().Set("X-Cache", "miss")
	w.Write(res.data)
}

func handleRandomCatJSON(w http.ResponseWriter, r *http.Request) {
	imagesMu.RLock()
	if len(images) == 0 {
		imagesMu.RUnlock()
		http.Error(w, `{"error":"no images"}`, http.StatusServiceUnavailable)
		return
	}
	img := images[rand.Intn(len(images))]
	imagesMu.RUnlock()
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "public, max-age=60")
	json.NewEncoder(w).Encode(img)
}

func handleAllCatsJSON(w http.ResponseWriter, r *http.Request) {
	imagesMu.RLock()
	defer imagesMu.RUnlock()
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "public, max-age=300")
	json.NewEncoder(w).Encode(images)
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	imagesMu.RLock()
	count := len(images)
	imagesMu.RUnlock()

	redisOK := rdb.Ping(ctx).Err() == nil
	status := "ok"
	if !redisOK {
		status = "degraded"
	}

	procCacheMu.RLock()
	procCacheSize := len(procCache)
	procCacheMu.RUnlock()

	srcCacheMu.RLock()
	srcCacheSize := len(srcCache)
	srcCacheMu.RUnlock()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":          status,
		"image_count":     count,
		"redis":           redisOK,
		"proc_cache_size": procCacheSize,
		"src_cache_size":  srcCacheSize,
	})
}
