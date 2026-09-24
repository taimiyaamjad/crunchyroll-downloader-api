package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// Temporary download store
//
// Every video the API produces is registered here with an expiry. A background
// sweeper deletes the whole job directory once it expires, so downloads do not
// accumulate on disk even if the client never comes back for the file.
// ---------------------------------------------------------------------------

type tempEntry struct {
	dir       string
	file      string
	expiresAt time.Time
}

type tempStore struct {
	mu      sync.Mutex
	entries map[string]*tempEntry
	ttl     time.Duration
}

func newTempStore(ttl time.Duration) *tempStore {
	return &tempStore{entries: map[string]*tempEntry{}, ttl: ttl}
}

// add registers a freshly downloaded file, starting its expiry clock.
func (s *tempStore) add(id, dir, file string) {
	s.mu.Lock()
	s.entries[id] = &tempEntry{dir: dir, file: file, expiresAt: time.Now().Add(s.ttl)}
	s.mu.Unlock()
}

// touch restarts the expiry clock, used while a file is being streamed so a
// slow client cannot have the file deleted out from under it.
func (s *tempStore) touch(id string) {
	s.mu.Lock()
	if e, ok := s.entries[id]; ok {
		e.expiresAt = time.Now().Add(s.ttl)
	}
	s.mu.Unlock()
}

// get returns a live entry, reporting false once it has expired.
func (s *tempStore) get(id string) (*tempEntry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[id]
	if !ok || time.Now().After(e.expiresAt) {
		return nil, false
	}
	return e, true
}

// remove deletes a single job and its files immediately.
func (s *tempStore) remove(id string) {
	s.mu.Lock()
	e, ok := s.entries[id]
	delete(s.entries, id)
	s.mu.Unlock()
	if ok {
		_ = os.RemoveAll(e.dir)
	}
}

// sweep deletes every expired entry. It is safe to call concurrently.
func (s *tempStore) sweep() {
	now := time.Now()
	s.mu.Lock()
	var expired []*tempEntry
	for id, e := range s.entries {
		if now.After(e.expiresAt) {
			expired = append(expired, e)
			delete(s.entries, id)
		}
	}
	s.mu.Unlock()

	for _, e := range expired {
		log.Printf("cleanup: deleting expired download %s", e.dir)
		_ = os.RemoveAll(e.dir)
	}
}

// run sweeps expired downloads until stop is closed.
func (s *tempStore) run(stop <-chan struct{}) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			s.sweep()
		case <-stop:
			return
		}
	}
}

// count returns how many downloads are currently tracked (for /api/health).
func (s *tempStore) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.entries)
}

// ---------------------------------------------------------------------------
// HTTP server
// ---------------------------------------------------------------------------

type apiServer struct {
	store *tempStore
	root  string
	ttl   time.Duration
	jobs  chan struct{} // concurrency gate for downloads
	start time.Time
}

func runServer() {
	root := *apiDir
	if root == "" {
		root = filepath.Join(os.TempDir(), "crdl-api")
	}
	if err := os.MkdirAll(root, 0o777); err != nil {
		log.Fatalf("failed to create API download dir %s: %v", root, err)
	}

	if *etpRt != "" {
		if t, err := tryGetAccessToken(*etpRt); err == nil {
			setCredentials(t, *etpRt)
		} else {
			log.Printf("warning: could not obtain access token from -etp-rt: %v", err)
		}
	}
	backoff = newDownloadBackoff(*downloadDelay)
	quietProgress = true

	limit := *maxJobs
	if limit < 1 {
		limit = 1
	}
	srv := &apiServer{
		store: newTempStore(*cleanupAfter),
		root:  root,
		ttl:   *cleanupAfter,
		jobs:  make(chan struct{}, limit),
		start: time.Now(),
	}

	stop := make(chan struct{})
	go srv.store.run(stop)

	mux := http.NewServeMux()
	mux.HandleFunc("/", srv.handleIndex)
	mux.HandleFunc("/api/health", srv.handleHealth)
	mux.HandleFunc("/api/search", srv.handleSearch)
	mux.HandleFunc("/api/download", srv.handleDownload)
	mux.HandleFunc("/api/file/", srv.handleFile)

	log.Printf("Crunchyroll API listening on %s", *listenAddr)
	log.Printf("downloads: %s | auto-delete after %s | max concurrent jobs: %d", root, srv.ttl, limit)
	if getToken() == "" {
		log.Printf("no -etp-rt given: each request must pass etp_rt=... until a token is set")
	}

	httpSrv := &http.Server{
		Addr:              *listenAddr,
		Handler:           withLogging(mux),
		ReadHeaderTimeout: 15 * time.Second,
	}
	if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("API server error: %v", err)
	}
}

func withLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		log.Printf("%s %s %s (%s)", r.RemoteAddr, r.Method, r.URL.Path, time.Since(start).Round(time.Millisecond))
	})
}

func (s *apiServer) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"service": "crunchyroll-downloader-api",
		"endpoints": []string{
			"GET /api/search?q=<query>&limit=10",
			"GET /api/download?url=<episode_url>&language=en&quality=1080p",
			"GET /api/file/<job_id>",
			"GET /api/health",
		},
		"examples": []string{
			"/api/download?url=https://www.crunchyroll.com/watch/GYXXXXXX&language=en&quality=1080p",
			"/api/download?https://www.crunchyroll.com/watch/GYXXXXXX?language(en)?1080p",
			"/api/download?url=https://www.crunchyroll.com/watch/GYXXXXXX&language=en&format=json",
		},
		"cleanup": fmt.Sprintf("downloaded videos are deleted %s after the download finishes", s.ttl),
		"notes": []string{
			"language accepts a full locale (en-US) or a bare code (en); a bare code is matched against the episode's available dubs.",
			"quality is a video height such as 1080p, 720p, 480p or 360p. audio_quality defaults to 192k.",
			"format=json returns a JSON descriptor with /api/file/<job_id> instead of streaming the video body.",
			"pass etp_rt=... to override the server account for a single request.",
		},
	})
}

func (s *apiServer) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status":          "ok",
		"uptime_seconds":  int(time.Since(s.start).Seconds()),
		"active_jobs":     len(s.jobs),
		"tracked_files":   s.store.count(),
		"cleanup_seconds": int(s.ttl.Seconds()),
	})
}

// ---------------------------------------------------------------------------
// Download endpoint
// ---------------------------------------------------------------------------

var (
	langParenRe = regexp.MustCompile(`(?i)^(?:language|lang|audio)\(([^)]+)\)$`)
	qualityRe   = regexp.MustCompile(`(?i)^\d{3,4}p$`)
)

type downloadRequest struct {
	url          string
	language     string
	quality      string
	audioQuality string
	subs         []string
	cc           []string
	etpRT        string
	asJSON       bool
}

// parseDownloadRequest reads the parameters from either the standard query
// string form (url=...&language=...&quality=...) or the terse browser form
// (/api/download?<episode-url>?language(en)?1080p).
func parseDownloadRequest(r *http.Request) (*downloadRequest, error) {
	q := r.URL.Query()
	req := &downloadRequest{
		url:          firstNonEmpty(q.Get("url"), q.Get("episode"), q.Get("ep")),
		language:     firstNonEmpty(q.Get("language"), q.Get("lang"), q.Get("audio_lang"), q.Get("audio")),
		quality:      firstNonEmpty(q.Get("quality"), q.Get("video_quality"), q.Get("res"), q.Get("resolution")),
		audioQuality: firstNonEmpty(q.Get("audio_quality"), q.Get("audioq")),
		etpRT:        firstNonEmpty(q.Get("etp_rt"), q.Get("etp-rt")),
		subs:         parseLangs(firstNonEmpty(q.Get("subs"), q.Get("subs_lang"), q.Get("subtitles"))),
		cc:           parseLangs(q.Get("cc")),
	}
	req.asJSON = strings.EqualFold(q.Get("format"), "json")

	// Fall back to the terse form whenever the standard keys were not used.
	if req.url == "" || req.language == "" || req.quality == "" {
		lu, ll, lq := parseLooseDownloadQuery(r.URL.RawQuery)
		if req.url == "" {
			req.url = lu
		}
		if req.language == "" {
			req.language = ll
		}
		if req.quality == "" {
			req.quality = lq
		}
	}

	if req.url == "" {
		return nil, fmt.Errorf("missing episode url, use /api/download?url=<episode_url>&language=en&quality=1080p")
	}
	return req, nil
}

// parseLooseDownloadQuery extracts an episode URL, language and quality from a
// raw query that is not a normal key=value list, e.g.
// "https://www.crunchyroll.com/watch/GY123?language(en)?1080p".
func parseLooseDownloadQuery(raw string) (epURL, language, quality string) {
	if raw == "" {
		return
	}
	if decoded, err := url.QueryUnescape(raw); err == nil {
		raw = decoded
	}

	// First pass: explicit key=value pairs, plus language(x) and bare qualities.
	for _, token := range strings.Split(raw, "?") {
		for _, pair := range strings.Split(token, "&") {
			key, value, ok := strings.Cut(pair, "=")
			if !ok {
				pair = strings.TrimSpace(pair)
				if m := langParenRe.FindStringSubmatch(pair); m != nil {
					if language == "" {
						language = m[1]
					}
				} else if qualityRe.MatchString(pair) && quality == "" {
					quality = pair
				}
				continue
			}
			switch strings.ToLower(strings.TrimSpace(key)) {
			case "url", "episode", "ep", "video":
				if epURL == "" {
					epURL = value
				}
			case "language", "lang":
				if language == "" {
					language = value
				}
			case "quality", "q", "res", "resolution":
				if quality == "" {
					quality = value
				}
			}
		}
	}

	// Second pass: a bare URL with its scheme.
	for _, token := range strings.Split(raw, "?") {
		token = strings.TrimSpace(token)
		if strings.HasPrefix(token, "http://") || strings.HasPrefix(token, "https://") {
			if epURL == "" {
				epURL = token
			}
			break
		}
	}
	return
}

func (s *apiServer) handleDownload(w http.ResponseWriter, r *http.Request) {
	req, err := parseDownloadRequest(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	if req.etpRT != "" {
		t, err := tryGetAccessToken(req.etpRT)
		if err != nil {
			writeError(w, http.StatusUnauthorized, "invalid etp_rt cookie: "+err.Error())
			return
		}
		setCredentials(t, req.etpRT)
	}
	if getToken() == "" {
		writeError(w, http.StatusUnauthorized, "no Crunchyroll credentials: start the server with -etp-rt or pass etp_rt=... on the request")
		return
	}

	contentType, contentId := parseUrl(req.url)
	if contentType != "watch" {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("only /watch/ episode URLs are supported by /api/download (got %q); use /api/search to find episodes", req.url))
		return
	}

	// Concurrency gate: refuse rather than queue indefinitely, so callers get a
	// clear retry signal instead of a request that hangs for minutes.
	select {
	case s.jobs <- struct{}{}:
		defer func() { <-s.jobs }()
	default:
		writeError(w, http.StatusTooManyRequests, "server busy: too many concurrent downloads, retry shortly")
		return
	}

	info, err := safeEpisodeInfo(contentId)
	if err != nil {
		writeError(w, http.StatusBadGateway, "failed to fetch episode info: "+err.Error())
		return
	}

	language := resolveAudioLocale(req.language, info)
	quality := normalizeQuality(req.quality)
	audioQuality := strings.TrimSpace(req.audioQuality)
	if audioQuality == "" {
		audioQuality = "192k"
	}

	jobID := newJobID()
	outDir := filepath.Join(s.root, jobID)
	if err := os.MkdirAll(outDir, 0o777); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to create job directory: "+err.Error())
		return
	}

	log.Printf("download started: episode=%s language=%s quality=%s audio=%s job=%s", contentId, language, quality, audioQuality, jobID)
	start := time.Now()
	videoPath, err := downloadEpisodeWithRetry(contentId, info, []string{language}, req.subs, req.cc, &quality, &audioQuality, outDir)
	if err != nil {
		_ = os.RemoveAll(outDir)
		writeError(w, http.StatusBadGateway, "download failed: "+err.Error())
		return
	}

	fileInfo, statErr := os.Stat(videoPath)
	if statErr != nil || fileInfo.IsDir() {
		_ = os.RemoveAll(outDir)
		writeError(w, http.StatusNotFound, fmt.Sprintf("no video was produced for language %q; available audio: %s",
			language, strings.Join(availableAudioLocales(info), ", ")))
		return
	}

	s.store.add(jobID, outDir, videoPath)
	log.Printf("download finished: job=%s size=%d in %s", jobID, fileInfo.Size(), time.Since(start).Round(time.Second))

	if req.asJSON {
		writeJSON(w, http.StatusOK, map[string]any{
			"status":             "ready",
			"job_id":             jobID,
			"filename":           filepath.Base(videoPath),
			"url":                "/api/file/" + jobID,
			"size_bytes":         fileInfo.Size(),
			"language":           language,
			"quality":            quality,
			"expires_in_seconds": int(s.ttl.Seconds()),
		})
		return
	}

	s.serveEntry(w, r, jobID, videoPath, fileInfo)
}

func (s *apiServer) handleFile(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/api/file/")
	if id == "" || strings.ContainsAny(id, `/\`) {
		writeError(w, http.StatusBadRequest, "invalid job id")
		return
	}
	entry, ok := s.store.get(id)
	if !ok {
		writeError(w, http.StatusNotFound, fmt.Sprintf("file not found or already deleted (files are removed %s after download)", s.ttl))
		return
	}
	fileInfo, err := os.Stat(entry.file)
	if err != nil {
		s.store.remove(id)
		writeError(w, http.StatusNotFound, "file not found or already deleted")
		return
	}
	s.serveEntry(w, r, id, entry.file, fileInfo)
}

// serveEntry streams a downloaded MKV, supporting HTTP range requests, then
// restarts its expiry clock so a slow client keeps the file long enough.
func (s *apiServer) serveEntry(w http.ResponseWriter, r *http.Request, id, path string, fileInfo os.FileInfo) {
	f, err := os.Open(path)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to open file: "+err.Error())
		return
	}
	defer f.Close()

	name := filepath.Base(path)
	w.Header().Set("Content-Type", "video/x-matroska")
	if disposition := mime.FormatMediaType("attachment", map[string]string{"filename": name}); disposition != "" {
		w.Header().Set("Content-Disposition", disposition)
	}
	w.Header().Set("X-Job-Id", id)
	w.Header().Set("X-Expires-In", strconv.Itoa(int(s.ttl.Seconds())))

	http.ServeContent(w, r, name, fileInfo.ModTime(), f)
	s.store.touch(id)
}

// ---------------------------------------------------------------------------
// Search endpoint
// ---------------------------------------------------------------------------

type posterImage struct {
	Source string `json:"source"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
}

type searchItem struct {
	ID              string   `json:"id"`
	Type            string   `json:"type"`
	Title           string   `json:"title"`
	Description     string   `json:"description"`
	SlugTitle       string   `json:"slug_title"`
	AudioLocales    []string `json:"audio_locales"`
	SubtitleLocales []string `json:"subtitle_locales"`
	IsPremium       bool     `json:"is_premium"`
	Images          struct {
		PosterTall [][]posterImage `json:"poster_tall"`
	} `json:"images"`
	EpisodeMetadata *struct {
		SeriesTitle   string `json:"series_title"`
		EpisodeTitle  string `json:"episode_title"`
		EpisodeNumber int    `json:"episode_number"`
		SeasonNumber  int    `json:"season_number"`
	} `json:"episode_metadata"`
	SeriesMetadata *struct {
		SeriesTitle string `json:"series_title"`
	} `json:"series_metadata"`
}

// SearchResult is the normalized shape returned to API clients.
type SearchResult struct {
	ID              string   `json:"id"`
	Type            string   `json:"type"`
	Title           string   `json:"title"`
	Description     string   `json:"description,omitempty"`
	URL             string   `json:"url"`
	Image           string   `json:"image,omitempty"`
	AudioLocales    []string `json:"audio_locales,omitempty"`
	SubtitleLocales []string `json:"subtitle_locales,omitempty"`
	EpisodeNumber   int      `json:"episode_number,omitempty"`
	SeasonNumber    int      `json:"season_number,omitempty"`
	SeriesTitle     string   `json:"series_title,omitempty"`
	IsPremium       bool     `json:"is_premium,omitempty"`
}

func (s *apiServer) handleSearch(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	query := firstNonEmpty(q.Get("q"), q.Get("query"), q.Get("search"))
	if query == "" {
		writeError(w, http.StatusBadRequest, "missing search query, use /api/search?q=<title>")
		return
	}
	if etp := firstNonEmpty(q.Get("etp_rt"), q.Get("etp-rt")); etp != "" {
		t, err := tryGetAccessToken(etp)
		if err != nil {
			writeError(w, http.StatusUnauthorized, "invalid etp_rt cookie: "+err.Error())
			return
		}
		setCredentials(t, etp)
	}
	if getToken() == "" {
		writeError(w, http.StatusUnauthorized, "no Crunchyroll credentials: start the server with -etp-rt or pass etp_rt=... on the request")
		return
	}

	limit := 10
	if n, err := strconv.Atoi(q.Get("limit")); err == nil && n > 0 {
		if n > 50 {
			n = 50
		}
		limit = n
	}

	results, err := searchCrunchyroll(query, limit)
	if err != nil {
		writeError(w, http.StatusBadGateway, "search failed: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"query":   query,
		"count":   len(results),
		"results": results,
	})
}

func searchCrunchyroll(query string, limit int) ([]SearchResult, error) {
	endpoint := fmt.Sprintf(
		"https://www.crunchyroll.com/content/v2/discover/search?q=%s&n=%d&type=top_results&ratings=true&preferred_audio_language=ja-JP&locale=en-US",
		url.QueryEscape(query), limit,
	)
	req, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+getToken())
	req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64; rv:147.0) Gecko/20100101 Firefox/147.0")

	resp, err := DoRequest(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("crunchyroll returned status %d", resp.StatusCode)
	}

	var payload struct {
		Data []struct {
			Type  string            `json:"type"`
			Items []json.RawMessage `json:"items"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("unexpected search response: %w", err)
	}

	var results []SearchResult
	for _, group := range payload.Data {
		for _, raw := range group.Items {
			var item searchItem
			if err := json.Unmarshal(raw, &item); err != nil || item.ID == "" {
				continue
			}

			result := SearchResult{
				ID:              item.ID,
				Type:            firstNonEmpty(item.Type, group.Type),
				Title:           item.Title,
				Description:     item.Description,
				Image:           firstPoster(item.Images.PosterTall),
				AudioLocales:    item.AudioLocales,
				SubtitleLocales: item.SubtitleLocales,
				IsPremium:       item.IsPremium,
			}
			if item.EpisodeMetadata != nil {
				result.SeriesTitle = item.EpisodeMetadata.SeriesTitle
				result.EpisodeNumber = item.EpisodeMetadata.EpisodeNumber
				result.SeasonNumber = item.EpisodeMetadata.SeasonNumber
				if result.Title == "" {
					result.Title = item.EpisodeMetadata.EpisodeTitle
				}
			}
			if item.SeriesMetadata != nil && result.SeriesTitle == "" {
				result.SeriesTitle = item.SeriesMetadata.SeriesTitle
			}
			result.URL = contentURL(result.Type, item.ID)

			results = append(results, result)
			if len(results) >= limit {
				return results, nil
			}
		}
	}
	return results, nil
}

func firstPoster(sets [][]posterImage) string {
	for _, set := range sets {
		for _, img := range set {
			if img.Source != "" {
				return img.Source
			}
		}
	}
	return ""
}

func contentURL(itemType, id string) string {
	switch itemType {
	case "series", "season", "movie_listing", "music":
		return "https://www.crunchyroll.com/series/" + id
	default:
		return "https://www.crunchyroll.com/watch/" + id
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// safeEpisodeInfo converts the panic-based getEpisodeInfo into an error so a bad
// episode ID or an API hiccup cannot take down the whole server.
func safeEpisodeInfo(id string) (info EpisodeInfo, err error) {
	defer func() {
		if rec := recover(); rec != nil {
			err = fmt.Errorf("%v", rec)
		}
	}()
	info = getEpisodeInfo(id)
	if info.EpisodeMetadata.SeriesTitle == "" && info.Title == "" && len(info.EpisodeMetadata.Versions) == 0 {
		return info, fmt.Errorf("episode %s not found", id)
	}
	return info, nil
}

// availableAudioLocales lists the dubs the episode actually offers.
func availableAudioLocales(info EpisodeInfo) []string {
	var out []string
	for _, v := range info.EpisodeMetadata.Versions {
		if v != nil {
			out = append(out, v.AudioLocale)
		}
	}
	if len(out) == 0 && info.EpisodeMetadata.AudioLocale != "" {
		out = append(out, info.EpisodeMetadata.AudioLocale)
	}
	return out
}

// resolveAudioLocale maps a requested locale to one the episode offers. A bare
// code such as "en" is matched to the first available "en-*" dub; an empty
// request falls back to the episode's primary audio locale.
func resolveAudioLocale(requested string, info EpisodeInfo) string {
	requested = strings.TrimSpace(requested)
	locales := availableAudioLocales(info)

	if requested == "" {
		if info.EpisodeMetadata.AudioLocale != "" {
			return info.EpisodeMetadata.AudioLocale
		}
		if len(locales) > 0 {
			return locales[0]
		}
		return "ja-JP"
	}

	for _, locale := range locales {
		if strings.EqualFold(locale, requested) {
			return locale
		}
	}
	if !strings.Contains(requested, "-") {
		for _, locale := range locales {
			if strings.HasPrefix(strings.ToLower(locale), strings.ToLower(requested)+"-") {
				return locale
			}
		}
	}
	return requested
}

// normalizeQuality turns "", "1080" and "1080P" into "1080p".
func normalizeQuality(quality string) string {
	quality = strings.ToLower(strings.TrimSpace(quality))
	if quality == "" {
		return "1080p"
	}
	if !strings.HasSuffix(quality, "p") {
		quality += "p"
	}
	return quality
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func newJobID() string {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return hex.EncodeToString(buf)
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(payload)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}
