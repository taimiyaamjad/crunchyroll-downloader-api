package main

import (
	"archive/zip"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html/template"
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
//
// Two lifetimes are supported:
//   - downloads: deleted a fixed TTL after they finish (default 10 minutes)
//   - watch streams: deleted once no one has been actively streaming for the
//     watch idle timeout (default 2 minutes), and never while a stream is open
// ---------------------------------------------------------------------------

type tempEntry struct {
	dir          string
	file         string
	cacheKey     string
	kind         string        // "download" or "watch"
	expiresAt    time.Time     // absolute expiry, used by download entries
	idleTimeout  time.Duration // >0 for watch entries: delete after this much inactivity
	lastActivity time.Time     // last time the entry was streamed or touched
	active       int           // number of streams currently reading the file
}

type tempStore struct {
	mu        sync.Mutex
	entries   map[string]*tempEntry
	ttl       time.Duration
	watchIdle time.Duration
}

func newTempStore(ttl, watchIdle time.Duration) *tempStore {
	return &tempStore{entries: map[string]*tempEntry{}, ttl: ttl, watchIdle: watchIdle}
}

// add registers a freshly downloaded file, starting its expiry clock.
func (s *tempStore) add(id, dir, file string) {
	s.addEntry(id, dir, file, "", "download")
}

// addWithKey registers a freshly downloaded file with an optional cache key.
func (s *tempStore) addWithKey(id, dir, file, cacheKey string) {
	s.addEntry(id, dir, file, cacheKey, "download")
}

// addWatch registers an online-stream entry. Unlike a download, a watch entry
// is deleted once nobody has been streaming it for the watch idle timeout,
// rather than a fixed time after the download finished.
func (s *tempStore) addWatch(id, dir, file, cacheKey string) {
	s.addEntry(id, dir, file, cacheKey, "watch")
}

func (s *tempStore) addEntry(id, dir, file, cacheKey, kind string) {
	now := time.Now()
	e := &tempEntry{
		dir:          dir,
		file:         file,
		cacheKey:     cacheKey,
		kind:         kind,
		expiresAt:    now.Add(s.ttl),
		lastActivity: now,
	}
	if kind == "watch" && s.watchIdle > 0 {
		e.idleTimeout = s.watchIdle
	}
	s.mu.Lock()
	s.entries[id] = e
	s.mu.Unlock()
}

// expired reports whether an entry should be deleted. A stream that is being
// read right now is never expired; a watch entry dies after the idle timeout,
// and a download after its absolute TTL. Callers must hold s.mu.
func (s *tempStore) expired(e *tempEntry, now time.Time) bool {
	if e.active > 0 {
		return false
	}
	if e.idleTimeout > 0 {
		return now.Sub(e.lastActivity) > e.idleTimeout
	}
	return now.After(e.expiresAt)
}

// findByKey searches for a live entry with a matching cacheKey.
func (s *tempStore) findByKey(key string) (string, *tempEntry, bool) {
	if key == "" {
		return "", nil, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for id, e := range s.entries {
		if e.cacheKey == key && !s.expired(e, now) {
			return id, e, true
		}
	}
	return "", nil, false
}

// touch restarts an entry's countdown: the idle clock for watch entries and the
// expiry clock for downloads.
func (s *tempStore) touch(id string) {
	s.mu.Lock()
	if e, ok := s.entries[id]; ok {
		now := time.Now()
		e.lastActivity = now
		e.expiresAt = now.Add(s.ttl)
	}
	s.mu.Unlock()
}

// acquire marks an entry as actively being streamed so the sweeper cannot
// delete it mid-stream, and refreshes its activity clock.
func (s *tempStore) acquire(id string) {
	s.mu.Lock()
	if e, ok := s.entries[id]; ok {
		e.active++
		e.lastActivity = time.Now()
	}
	s.mu.Unlock()
}

// release marks one stream as finished and restarts the countdown, so the file
// survives briefly for seeks/reconnects before the idle sweeper removes it.
func (s *tempStore) release(id string) {
	s.mu.Lock()
	if e, ok := s.entries[id]; ok {
		if e.active > 0 {
			e.active--
		}
		now := time.Now()
		e.lastActivity = now
		e.expiresAt = now.Add(s.ttl)
	}
	s.mu.Unlock()
}

// expiresIn reports the lifetime hint for an entry: the idle timeout for watch
// streams, otherwise the standard download TTL.
func (s *tempStore) expiresIn(id string, fallback time.Duration) time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.entries[id]; ok && e.idleTimeout > 0 {
		return e.idleTimeout
	}
	return fallback
}

// get returns a live entry, reporting false once it has expired.
func (s *tempStore) get(id string) (*tempEntry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[id]
	if !ok || s.expired(e, time.Now()) {
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
		if s.expired(e, now) {
			expired = append(expired, e)
			delete(s.entries, id)
		}
	}
	s.mu.Unlock()

	for _, e := range expired {
		reason := "ttl"
		if e.idleTimeout > 0 {
			reason = "idle"
		}
		log.Printf("cleanup: deleting %s %s (%s)", e.kind, e.dir, reason)
		_ = os.RemoveAll(e.dir)
	}
}

// sweepInterval picks how often to look for dead entries. With a short watch
// idle timeout the sweeper runs more often so deletion happens close to the
// promised window instead of up to 30s late.
func (s *tempStore) sweepInterval() time.Duration {
	const def = 30 * time.Second
	if s.watchIdle <= 0 || s.watchIdle >= def {
		return def
	}
	interval := s.watchIdle / 2
	if interval < time.Second {
		interval = time.Second
	}
	return interval
}

// run sweeps expired downloads until stop is closed.
func (s *tempStore) run(stop <-chan struct{}) {
	ticker := time.NewTicker(s.sweepInterval())
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
	store     *tempStore
	root      string
	ttl       time.Duration
	watchIdle time.Duration
	ticketTTL time.Duration
	jobs      chan struct{} // concurrency gate for downloads
	start     time.Time
	auth      *apiAuth // nil when -no-auth was given
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
		store:     newTempStore(*cleanupAfter, *watchIdleTimeout),
		root:      root,
		ttl:       *cleanupAfter,
		watchIdle: *watchIdleTimeout,
		ticketTTL: *ticketTTL,
		jobs:      make(chan struct{}, limit),
		start:     time.Now(),
	}

	// Set up the permanent API token that protects every /api/* endpoint.
	if *noAuth {
		log.Printf("WARNING: API token authentication is DISABLED (-no-auth)")
	} else {
		info, err := loadOrCreateAPIToken(*apiToken, *apiTokenFile)
		if err != nil {
			log.Fatalf("failed to set up API token: %v", err)
		}
		srv.auth = newAPIAuth(info.token, *signKey, splitOrigins(*allowOrigin))
		switch {
		case info.explicit:
			log.Printf("API token: using the value passed with -api-token")
		case info.generated:
			log.Printf("API token: generated a new random token and saved it to %s", info.path)
		default:
			log.Printf("loaded permanent API token from %s", info.path)
		}
		if *showToken {
			log.Printf("API token (full): %s", info.token)
		} else {
			log.Printf("API token: %s  (masked; pass -show-token or read %s to see it)", maskToken(info.token), info.path)
		}
		log.Printf("browsers must NOT use the master token: mint short-lived signed URLs with GET /api/ticket (ttl %s)", *ticketTTL)
		if len(srv.auth.origins) > 0 {
			log.Printf("origin allowlist: %s", strings.Join(srv.auth.origins, ", "))
		}
	}

	stop := make(chan struct{})
	go srv.store.run(stop)

	mux := http.NewServeMux()
	mux.HandleFunc("/", srv.handleIndex)
	mux.HandleFunc("/api/health", srv.handleHealth)
	mux.HandleFunc("/api/search", srv.handleSearch)
	mux.HandleFunc("/api/download", srv.handleDownload)
	mux.HandleFunc("/api/watch", srv.handleWatch)
	mux.HandleFunc("/api/ticket", srv.handleTicket)
	mux.HandleFunc("/api/file/", srv.handleFile)

	var handler http.Handler = mux
	if srv.auth != nil {
		handler = srv.auth.requireAuth(mux)
	}

	log.Printf("Crunchyroll API listening on %s", *listenAddr)
	log.Printf("downloads: %s | auto-delete downloads after %s | watch idle timeout %s | max concurrent jobs: %d",
		root, srv.ttl, srv.watchIdle, limit)
	if getToken() == "" {
		log.Printf("no -etp-rt given: each request must pass etp_rt=... until a token is set")
	}
	if srv.auth.enabled() && !isLoopbackAddr(*listenAddr) && len(srv.auth.origins) == 0 {
		log.Printf("WARNING: %s is reachable beyond localhost and no -allow-origin is set; anyone who obtains the token or a signed URL can use this API", *listenAddr)
	}

	httpSrv := &http.Server{
		Addr:              *listenAddr,
		Handler:           withLogging(handler),
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
		"auth":    s.authDescription(),
		"endpoints": []string{
			"GET /api/ticket?path=/api/watch&url=<episode_url>&language=hi&quality=1080p  (master token; mints a short-lived signed URL)",
			"GET /api/search?q=<query>&limit=10",
			"GET /api/watch?url=<episode_url>&language=hi&quality=1080p[&exp=..&sig=..]",
			"GET /api/download?url=<episode_or_series_url>&language=hi&quality=1080p[&exp=..&sig=..]",
			"GET /api/file/<job_id>",
			"GET /api/health",
		},
		"examples": []string{
			"/api/ticket?path=/api/watch&url=https://www.crunchyroll.com/watch/GYXXXXXX&language=hi&quality=1080p&ttl=5m",
			"/api/watch?url=https://www.crunchyroll.com/watch/GYXXXXXX&language=hi&quality=1080p&exp=1730000000&sig=<signature>",
			"/api/watch?https://www.crunchyroll.com/watch/GYXXXXXX?language=hi? quality=1080p",
			"/api/watch?url=https://www.crunchyroll.com/watch/GYXXXXXX&language=hi&player=1",
			"/api/download?url=https://www.crunchyroll.com/watch/GYXXXXXX&language=hi&quality=1080p",
			"/api/download?url=https://www.crunchyroll.com/series/GYXXXXXX&season=1&language=hi&quality=1080p",
			"/api/download?https://www.crunchyroll.com/watch/GYXXXXXX?language(hi)?1080p",
			"/api/download?url=https://www.crunchyroll.com/watch/GYXXXXXX&language=hi&format=json",
		},
		"cleanup": fmt.Sprintf("downloads are deleted %s after they finish; /api/watch streams are deleted %s after the last viewer stops watching", s.ttl, s.watchIdle),
		"notes": []string{
			"the master API token is server-to-server only; never put it in frontend JavaScript or a public URL.",
			"for browser playback, call /api/ticket with the master token to mint a short-lived signed URL, then give that URL to the browser.",
			"signed URLs carry exp+sig instead of the token and expire after the ticket TTL (default 5m).",
			"set -allow-origin to restrict browser requests to your platform's domains.",
			"for Hindi use 'hi' as the short code (automatically maps to hi-IN).",
			"GET /api/watch streams the video directly online in Chrome (inline playback with range requests). Append &player=1 for a built-in web player page.",
			"GET /api/watch files are deleted automatically once nobody has streamed them for the watch idle timeout (default 2m); an in-progress stream is never deleted.",
			"GET /api/download downloads a single episode (.mkv) or an entire season (.zip when given a /series/ URL or season=N).",
			"quality is a video height such as 1080p, 720p, 480p or 360p. audio_quality defaults to 192k.",
			"format=json returns a JSON descriptor with /api/file/<job_id> instead of streaming the video body.",
			"pass etp_rt=... to override the server account for a single request.",
		},
	})
}

func (s *apiServer) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status":             "ok",
		"uptime_seconds":     int(time.Since(s.start).Seconds()),
		"active_jobs":        len(s.jobs),
		"tracked_files":      s.store.count(),
		"cleanup_seconds":    int(s.ttl.Seconds()),
		"watch_idle_seconds": int(s.watchIdle.Seconds()),
		"ticket_ttl_seconds": int(s.ticketTTL.Seconds()),
	})
}

// ticketSignablePath limits which endpoints /api/ticket is allowed to sign, so
// a ticket can never be minted for the ticket endpoint itself or an arbitrary
// path.
func ticketSignablePath(p string) bool {
	switch p {
	case "/api/watch", "/api/download", "/api/search":
		return true
	}
	return strings.HasPrefix(p, "/api/file/")
}

// handleTicket mints a short-lived signed URL. It is the bridge that lets a
// public site stream through this API without ever exposing the master token:
// the platform's backend calls /api/ticket with the master token, and the
// browser only receives the resulting exp+sig URL.
func (s *apiServer) handleTicket(w http.ResponseWriter, r *http.Request) {
	if !s.auth.enabled() {
		writeError(w, http.StatusBadRequest,
			"API token authentication is disabled, so signed URLs are not available; use -api-token or remove -no-auth")
		return
	}

	target := strings.TrimSpace(firstNonEmpty(r.URL.Query().Get("path"), r.URL.Query().Get("endpoint")))
	if target == "" {
		target = "/api/watch"
	}
	if !ticketSignablePath(target) {
		writeError(w, http.StatusBadRequest,
			"path must be one of /api/watch, /api/download, /api/search or /api/file/<job_id>")
		return
	}

	// Forward the caller's parameters, minus the ticket controls and minus any
	// credential. The signed URL is handed to a browser, so it must never carry
	// the master token or the Crunchyroll etp_rt cookie.
	params := url.Values{}
	for key, vals := range r.URL.Query() {
		switch strings.ToLower(key) {
		case "path", "endpoint", "ttl", "absolute",
			"token", "api_key", "apikey", "key", "sig", "exp",
			"etp_rt", "etp-rt":
			continue
		}
		for _, v := range vals {
			params.Add(key, v)
		}
	}

	ttl := s.ticketTTL
	if v := strings.TrimSpace(r.URL.Query().Get("ttl")); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			writeError(w, http.StatusBadRequest, "ttl must be a positive duration such as 5m or 30s")
			return
		}
		ttl = d
	}

	signedPath, exp := s.auth.signQuery(http.MethodGet, target, params.Encode(), ttl)
	resp := map[string]any{
		"url":                signedPath,
		"expires_in_seconds": int(time.Until(time.Unix(exp, 0)).Seconds()),
		"expires_at":         time.Unix(exp, 0).UTC().Format(time.RFC3339),
		"note":               "hand this URL to the browser; it expires and never contains the master API token",
	}
	if v := strings.TrimSpace(r.URL.Query().Get("absolute")); v == "1" || strings.EqualFold(v, "true") {
		resp["absolute_url"] = absoluteURL(r, signedPath)
	}
	writeJSON(w, http.StatusOK, resp)
}

// absoluteURL rebuilds the externally visible URL for a request, honouring a
// reverse proxy's X-Forwarded-Proto.
func absoluteURL(r *http.Request, path string) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if proto := strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")); proto != "" {
		scheme = strings.ToLower(proto)
	}
	return scheme + "://" + r.Host + path
}

// ---------------------------------------------------------------------------
// Download and Watch endpoints
// ---------------------------------------------------------------------------

var (
	langParenRe   = regexp.MustCompile(`(?i)^(?:language|lang|audio)\(([^)]+)\)$`)
	seasonParenRe = regexp.MustCompile(`(?i)^(?:season|s)\(([^)]+)\)$`)
	seasonTokenRe = regexp.MustCompile(`(?i)^(?:s|season)(\d{1,2})$`)
	epTokenRe     = regexp.MustCompile(`(?i)^(?:e|ep|episode)(\d{1,4})$`)
	qualityRe     = regexp.MustCompile(`(?i)^(\d{3,4})p?$`)
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
	season       int  // 0 = first/unspecified, >0 = season number, -1 = all
	episode      int  // specific episode number for watching
	player       bool // render HTML5 video player page
	stream       bool // inline streaming
}

// parseDownloadRequest reads the parameters from either the standard query
// string form (url=...&language=...&quality=...) or the terse browser form
// (/api/download?<url>?language(hi)?1080p).
func parseDownloadRequest(r *http.Request) (*downloadRequest, error) {
	q := r.URL.Query()
	seasonVal := 0
	if sStr := firstNonEmpty(q.Get("season"), q.Get("s"), q.Get("season_number")); sStr != "" {
		if strings.EqualFold(sStr, "all") {
			seasonVal = -1
		} else if n, err := strconv.Atoi(sStr); err == nil {
			seasonVal = n
		}
	}
	epVal := 0
	if eStr := firstNonEmpty(q.Get("episode"), q.Get("ep"), q.Get("e"), q.Get("ep_num")); eStr != "" {
		if n, err := strconv.Atoi(eStr); err == nil {
			epVal = n
		}
	}

	req := &downloadRequest{
		url:          firstNonEmpty(q.Get("url"), q.Get("episode_url"), q.Get("ep_url"), q.Get("series_url")),
		language:     firstNonEmpty(q.Get("language"), q.Get("lang"), q.Get("audio_lang"), q.Get("audio")),
		quality:      firstNonEmpty(q.Get("quality"), q.Get("video_quality"), q.Get("res"), q.Get("resolution")),
		audioQuality: firstNonEmpty(q.Get("audio_quality"), q.Get("audioq")),
		etpRT:        firstNonEmpty(q.Get("etp_rt"), q.Get("etp-rt")),
		subs:         parseLangs(firstNonEmpty(q.Get("subs"), q.Get("subs_lang"), q.Get("subtitles"))),
		cc:           parseLangs(q.Get("cc")),
		season:       seasonVal,
		episode:      epVal,
		player:       q.Get("player") == "1" || strings.EqualFold(q.Get("player"), "true") || strings.EqualFold(q.Get("format"), "player") || strings.EqualFold(q.Get("format"), "html"),
		stream:       q.Get("stream") == "1" || strings.EqualFold(q.Get("stream"), "true") || q.Get("inline") == "1",
	}
	req.asJSON = strings.EqualFold(q.Get("format"), "json")

	// Fall back to the terse form whenever the standard keys were not used.
	if req.url == "" || req.language == "" || req.quality == "" || req.season == 0 {
		lu, ll, lq, ls, le, lp := parseLooseDownloadQuery(r.URL.RawQuery)
		if req.url == "" {
			req.url = lu
		}
		if req.language == "" {
			req.language = ll
		}
		if req.quality == "" {
			req.quality = lq
		}
		if req.season == 0 {
			req.season = ls
		}
		if req.episode == 0 {
			req.episode = le
		}
		if !req.player {
			req.player = lp
		}
	}

	if req.url == "" {
		return nil, fmt.Errorf("missing episode or series url (e.g. /api/watch?url=<url>&language=hi&quality=1080p)")
	}
	return req, nil
}

// parseLooseDownloadQuery extracts parameters from a raw query that is not
// a standard key=value list, e.g.:
// "https://www.crunchyroll.com/watch/GY123?language=hi? quality=1080p"
func parseLooseDownloadQuery(raw string) (epURL, language, quality string, season int, episode int, player bool) {
	if raw == "" {
		return
	}
	if decoded, err := url.QueryUnescape(raw); err == nil {
		raw = decoded
	}

	// First pass: explicit key=value pairs, plus language(x), quality, season, player
	for _, token := range strings.Split(raw, "?") {
		for _, pair := range strings.Split(token, "&") {
			pair = strings.TrimSpace(pair)
			if pair == "" {
				continue
			}
			key, value, ok := strings.Cut(pair, "=")
			if !ok {
				if m := langParenRe.FindStringSubmatch(pair); m != nil {
					if language == "" {
						language = m[1]
					}
				} else if m := seasonParenRe.FindStringSubmatch(pair); m != nil {
					if season == 0 {
						season, _ = strconv.Atoi(m[1])
					}
				} else if m := seasonTokenRe.FindStringSubmatch(pair); m != nil {
					if season == 0 {
						season, _ = strconv.Atoi(m[1])
					}
				} else if m := epTokenRe.FindStringSubmatch(pair); m != nil {
					if episode == 0 {
						episode, _ = strconv.Atoi(m[1])
					}
				} else if m := qualityRe.FindStringSubmatch(pair); m != nil && quality == "" {
					quality = m[1] + "p"
				} else if isBareLanguage(pair) && language == "" {
					language = pair
				} else if strings.EqualFold(pair, "player") {
					player = true
				}
				continue
			}

			k := strings.ToLower(strings.TrimSpace(key))
			v := strings.TrimSpace(value)
			switch k {
			case "url", "episode_url", "ep_url", "series_url", "episode", "ep", "video":
				if epURL == "" {
					epURL = v
				}
			case "language", "lang", "audio", "audio_lang":
				if language == "" {
					language = v
				}
			case "quality", "q", "res", "resolution", "video_quality":
				if quality == "" {
					quality = v
				}
			case "season", "s", "season_number":
				if strings.EqualFold(v, "all") {
					season = -1
				} else if n, err := strconv.Atoi(v); err == nil && season == 0 {
					season = n
				}
			case "ep_num", "episode_number", "e":
				if n, err := strconv.Atoi(v); err == nil && episode == 0 {
					episode = n
				}
			case "player":
				player = v == "1" || strings.EqualFold(v, "true") || strings.EqualFold(v, "yes")
			}
		}
	}

	// Second pass: a bare URL with its scheme or crunchyroll host
	for _, token := range strings.Split(raw, "?") {
		token = strings.TrimSpace(token)
		if strings.HasPrefix(token, "http://") || strings.HasPrefix(token, "https://") || strings.Contains(token, "crunchyroll.com/") {
			if epURL == "" {
				epURL = token
			}
			break
		}
	}
	return
}

// handleWatch directly streams an anime online in Chrome (inline playback),
// with range request support and an optional built-in web player.
func (s *apiServer) handleWatch(w http.ResponseWriter, r *http.Request) {
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
	if contentType == "" || contentId == "" {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid Crunchyroll URL: %s", req.url))
		return
	}

	languageCode := canonicalLocale(req.language)
	quality := normalizeQuality(req.quality)
	audioQuality := strings.TrimSpace(req.audioQuality)
	if audioQuality == "" {
		audioQuality = "192k"
	}

	targetEpisodeID := contentId
	if contentType == "series" {
		primaryAudio := languageCode
		if primaryAudio == "" {
			primaryAudio = "ja-JP"
		}
		primarySubs := "en-US"
		if len(req.subs) > 0 {
			primarySubs = req.subs[0]
		}
		seasons, err := safeGetSeasons(contentId, primaryAudio, primarySubs)
		if err != nil {
			writeError(w, http.StatusBadGateway, "failed to fetch seasons: "+err.Error())
			return
		}
		targetSeason := seasons[0]
		if req.season > 0 {
			for _, sn := range seasons {
				if sn.SeasonNumber == req.season {
					targetSeason = sn
					break
				}
			}
		}
		episodes, err := safeGetSeasonEpisodes(targetSeason.ID, primaryAudio, primarySubs)
		if err != nil {
			writeError(w, http.StatusBadGateway, "failed to fetch season episodes: "+err.Error())
			return
		}
		chosenEpisode := episodes[0]
		if req.episode > 0 {
			for _, ep := range episodes {
				if ep.EpisodeNumber == req.episode {
					chosenEpisode = ep
					break
				}
			}
		}
		targetEpisodeID = chosenEpisode.ID
	}

	info, err := safeEpisodeInfo(targetEpisodeID)
	if err != nil {
		writeError(w, http.StatusBadGateway, "failed to fetch episode info: "+err.Error())
		return
	}

	language := resolveAudioLocale(languageCode, info)
	cacheKey := fmt.Sprintf("watch:%s:%s:%s", targetEpisodeID, language, quality)

	// Check if already downloaded and alive in cache
	if cachedID, entry, ok := s.store.findByKey(cacheKey); ok {
		if fileInfo, err := os.Stat(entry.file); err == nil && !fileInfo.IsDir() {
			if req.player {
				s.renderPlayer(w, r, cachedID, entry.file, info, language, quality)
				return
			}
			s.serveEntry(w, r, cachedID, entry.file, fileInfo, true)
			return
		}
	}

	// Concurrency gate
	select {
	case s.jobs <- struct{}{}:
		defer func() { <-s.jobs }()
	default:
		writeError(w, http.StatusTooManyRequests, "server busy: too many concurrent downloads, retry shortly")
		return
	}

	jobID := newJobID()
	outDir := filepath.Join(s.root, jobID)
	if err := os.MkdirAll(outDir, 0o777); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to create job directory: "+err.Error())
		return
	}

	log.Printf("watch stream preparation started: episode=%s language=%s quality=%s job=%s", targetEpisodeID, language, quality, jobID)
	videoPath, err := downloadEpisodeWithRetry(targetEpisodeID, info, []string{language}, req.subs, req.cc, &quality, &audioQuality, outDir)
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

	s.store.addWatch(jobID, outDir, videoPath, cacheKey)

	if req.player {
		s.renderPlayer(w, r, jobID, videoPath, info, language, quality)
		return
	}

	// Direct online stream (Chrome will stream it inline inside the tab)
	s.serveEntry(w, r, jobID, videoPath, fileInfo, true)
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
	if contentType != "watch" && contentType != "series" {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("unsupported Crunchyroll URL (expected /watch/ or /series/): %s", req.url))
		return
	}

	language := canonicalLocale(req.language)
	quality := normalizeQuality(req.quality)
	audioQuality := strings.TrimSpace(req.audioQuality)
	if audioQuality == "" {
		audioQuality = "192k"
	}

	// Concurrency gate: refuse rather than queue indefinitely
	select {
	case s.jobs <- struct{}{}:
		defer func() { <-s.jobs }()
	default:
		writeError(w, http.StatusTooManyRequests, "server busy: too many concurrent downloads, retry shortly")
		return
	}

	jobID := newJobID()
	outDir := filepath.Join(s.root, jobID)
	if err := os.MkdirAll(outDir, 0o777); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to create job directory: "+err.Error())
		return
	}

	// Case 1: Single episode download
	if contentType == "watch" {
		info, err := safeEpisodeInfo(contentId)
		if err != nil {
			_ = os.RemoveAll(outDir)
			writeError(w, http.StatusBadGateway, "failed to fetch episode info: "+err.Error())
			return
		}

		resolvedLang := resolveAudioLocale(language, info)
		log.Printf("download started: episode=%s language=%s quality=%s audio=%s job=%s", contentId, resolvedLang, quality, audioQuality, jobID)
		start := time.Now()
		videoPath, err := downloadEpisodeWithRetry(contentId, info, []string{resolvedLang}, req.subs, req.cc, &quality, &audioQuality, outDir)
		if err != nil {
			_ = os.RemoveAll(outDir)
			writeError(w, http.StatusBadGateway, "download failed: "+err.Error())
			return
		}

		fileInfo, statErr := os.Stat(videoPath)
		if statErr != nil || fileInfo.IsDir() {
			_ = os.RemoveAll(outDir)
			writeError(w, http.StatusNotFound, fmt.Sprintf("no video was produced for language %q; available audio: %s",
				resolvedLang, strings.Join(availableAudioLocales(info), ", ")))
			return
		}

		s.store.add(jobID, outDir, videoPath)
		log.Printf("download finished: job=%s size=%d in %s", jobID, fileInfo.Size(), time.Since(start).Round(time.Second))

		if req.asJSON {
			writeJSON(w, http.StatusOK, map[string]any{
				"status":             "ready",
				"type":               "episode",
				"job_id":             jobID,
				"filename":           filepath.Base(videoPath),
				"url":                "/api/file/" + jobID,
				"size_bytes":         fileInfo.Size(),
				"language":           resolvedLang,
				"quality":            quality,
				"expires_in_seconds": int(s.ttl.Seconds()),
			})
			return
		}

		s.serveEntry(w, r, jobID, videoPath, fileInfo, false)
		return
	}

	// Case 2: Whole season download
	primaryAudio := language
	if primaryAudio == "" {
		primaryAudio = "ja-JP"
	}
	primarySubs := "en-US"
	if len(req.subs) > 0 {
		primarySubs = req.subs[0]
	}

	seasons, err := safeGetSeasons(contentId, primaryAudio, primarySubs)
	if err != nil {
		_ = os.RemoveAll(outDir)
		writeError(w, http.StatusBadGateway, "failed to fetch seasons: "+err.Error())
		return
	}

	var targetSeasons []Season
	if req.season > 0 {
		for _, sn := range seasons {
			if sn.SeasonNumber == req.season {
				targetSeasons = append(targetSeasons, sn)
				break
			}
		}
		if len(targetSeasons) == 0 {
			_ = os.RemoveAll(outDir)
			var available []int
			for _, s := range seasons {
				available = append(available, s.SeasonNumber)
			}
			writeError(w, http.StatusNotFound, fmt.Sprintf("season %d not found; available seasons: %v", req.season, available))
			return
		}
	} else if req.season == -1 {
		targetSeasons = seasons
	} else {
		targetSeasons = []Season{seasons[0]}
	}

	var allDownloadedFiles []string
	var seriesTitle string
	seasonNum := targetSeasons[0].SeasonNumber

	for _, sn := range targetSeasons {
		episodes, err := safeGetSeasonEpisodes(sn.ID, primaryAudio, primarySubs)
		if err != nil {
			log.Printf("warning: skipping season %d: %v", sn.SeasonNumber, err)
			continue
		}
		if len(episodes) > 0 && seriesTitle == "" {
			seriesTitle = episodes[0].SeriesTitle
		}
		files, err := downloadSeason(&quality, &audioQuality, []string{primaryAudio}, req.subs, req.cc, episodes, outDir)
		if err != nil && len(files) == 0 {
			log.Printf("warning: download season %d returned error: %v", sn.SeasonNumber, err)
			continue
		}
		allDownloadedFiles = append(allDownloadedFiles, files...)
	}

	if len(allDownloadedFiles) == 0 {
		_ = os.RemoveAll(outDir)
		writeError(w, http.StatusBadGateway, "failed to download any episodes for this season")
		return
	}

	if seriesTitle == "" {
		seriesTitle = "Crunchyroll Series"
	}

	var zipName string
	if len(targetSeasons) == 1 {
		zipName = fmt.Sprintf("%s S%02d.zip", sanitizeFilename(seriesTitle), seasonNum)
	} else {
		zipName = fmt.Sprintf("%s All Seasons.zip", sanitizeFilename(seriesTitle))
	}
	zipPath := filepath.Join(outDir, zipName)

	if err := createZipArchive(zipPath, allDownloadedFiles, outDir); err != nil {
		_ = os.RemoveAll(outDir)
		writeError(w, http.StatusInternalServerError, "failed to create zip archive: "+err.Error())
		return
	}

	zipFileInfo, err := os.Stat(zipPath)
	if err != nil {
		_ = os.RemoveAll(outDir)
		writeError(w, http.StatusInternalServerError, "failed to stat zip archive: "+err.Error())
		return
	}

	s.store.add(jobID, outDir, zipPath)
	log.Printf("season download ready: job=%s episodes=%d zip=%s (%d bytes)", jobID, len(allDownloadedFiles), zipName, zipFileInfo.Size())

	if req.asJSON {
		var epList []map[string]any
		for _, f := range allDownloadedFiles {
			fi, _ := os.Stat(f)
			var sz int64
			if fi != nil {
				sz = fi.Size()
			}
			epList = append(epList, map[string]any{
				"filename":   filepath.Base(f),
				"size_bytes": sz,
			})
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"status":             "ready",
			"type":               "season",
			"job_id":             jobID,
			"series_title":       seriesTitle,
			"season_number":      seasonNum,
			"episode_count":      len(allDownloadedFiles),
			"filename":           zipName,
			"url":                "/api/file/" + jobID,
			"size_bytes":         zipFileInfo.Size(),
			"episodes":           epList,
			"expires_in_seconds": int(s.ttl.Seconds()),
		})
		return
	}

	s.serveEntry(w, r, jobID, zipPath, zipFileInfo, false)
}

func (s *apiServer) handleFile(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/api/file/")
	if id == "" || strings.ContainsAny(id, `/\`) {
		writeError(w, http.StatusBadRequest, "invalid job id")
		return
	}
	entry, ok := s.store.get(id)
	if !ok {
		writeError(w, http.StatusNotFound, fmt.Sprintf(
			"file not found or already deleted (downloads last %s; watch streams last %s after the last viewer stops)",
			s.ttl, s.watchIdle))
		return
	}
	fileInfo, err := os.Stat(entry.file)
	if err != nil {
		s.store.remove(id)
		writeError(w, http.StatusNotFound, "file not found or already deleted")
		return
	}
	inline := r.URL.Query().Get("inline") == "1" || r.URL.Query().Get("stream") == "1"
	s.serveEntry(w, r, id, entry.file, fileInfo, inline)
}

// serveEntry streams or downloads a file, supporting HTTP range requests. While
// the body is being written the entry is marked active so the idle sweeper
// cannot delete a file out from under an in-progress stream; when it finishes
// the countdown restarts.
func (s *apiServer) serveEntry(w http.ResponseWriter, r *http.Request, id, path string, fileInfo os.FileInfo, inline bool) {
	f, err := os.Open(path)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to open file: "+err.Error())
		return
	}
	defer f.Close()

	if id != "" {
		s.store.acquire(id)
		defer s.store.release(id)
	}

	name := filepath.Base(path)
	switch strings.ToLower(filepath.Ext(name)) {
	case ".zip":
		w.Header().Set("Content-Type", "application/zip")
	case ".mp4":
		w.Header().Set("Content-Type", "video/mp4")
	case ".webm":
		w.Header().Set("Content-Type", "video/webm")
	default:
		w.Header().Set("Content-Type", "video/x-matroska")
	}

	dispType := "attachment"
	if inline {
		dispType = "inline"
	}
	if disposition := mime.FormatMediaType(dispType, map[string]string{"filename": name}); disposition != "" {
		w.Header().Set("Content-Disposition", disposition)
	}
	w.Header().Set("X-Job-Id", id)
	w.Header().Set("X-Expires-In", strconv.Itoa(int(s.store.expiresIn(id, s.ttl).Seconds())))

	http.ServeContent(w, r, name, fileInfo.ModTime(), f)
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
// code such as "en" is matched to the first available "en-*" dub; "hi" is matched
// to "hi-IN"; an empty request falls back to the episode's primary audio locale.
func resolveAudioLocale(requested string, info EpisodeInfo) string {
	requested = canonicalLocale(requested)
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

// safeGetSeasons wraps getSeasons, recovering from panics so server goroutines stay safe.
func safeGetSeasons(contentId string, audioLocale, subLocale string) (seasons []Season, err error) {
	defer func() {
		if rec := recover(); rec != nil {
			err = fmt.Errorf("%v", rec)
		}
	}()
	seasons = getSeasons(contentId, audioLocale, subLocale)
	if len(seasons) == 0 {
		return nil, fmt.Errorf("no seasons found for series %s", contentId)
	}
	return seasons, nil
}

// safeGetSeasonEpisodes wraps getSeasonEpisodes, recovering from panics.
func safeGetSeasonEpisodes(seasonId string, audioLocale, subLocale string) (episodes []SeasonEpisode, err error) {
	defer func() {
		if rec := recover(); rec != nil {
			err = fmt.Errorf("%v", rec)
		}
	}()
	episodes = getSeasonEpisodes(seasonId, audioLocale, subLocale)
	if len(episodes) == 0 {
		return nil, fmt.Errorf("no episodes found for season %s", seasonId)
	}
	return episodes, nil
}

// createZipArchive bundles files into a zip archive with zero re-compression (Store),
// making it instantaneous for video files and compatible with standard unzip tools.
func createZipArchive(zipPath string, files []string, baseDir string) error {
	zipFile, err := os.Create(zipPath)
	if err != nil {
		return err
	}
	defer zipFile.Close()

	zw := zip.NewWriter(zipFile)
	defer zw.Close()

	for _, file := range files {
		relPath, err := filepath.Rel(baseDir, file)
		if err != nil {
			relPath = filepath.Base(file)
		}
		f, err := os.Open(file)
		if err != nil {
			return err
		}
		info, err := f.Stat()
		if err != nil {
			f.Close()
			return err
		}
		header, err := zip.FileInfoHeader(info)
		if err != nil {
			f.Close()
			return err
		}
		header.Name = filepath.ToSlash(relPath)
		header.Method = zip.Store
		w, err := zw.CreateHeader(header)
		if err != nil {
			f.Close()
			return err
		}
		if _, err := io.Copy(w, f); err != nil {
			f.Close()
			return err
		}
		f.Close()
	}
	return nil
}

func isBareLanguage(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "hi", "hindi", "en", "english", "ja", "jp", "japanese", "es", "spanish", "fr", "french", "de", "german", "it", "italian", "pt", "portuguese", "ru", "russian", "ar", "arabic", "ta", "tamil", "te", "telugu":
		return true
	}
	return false
}

var playerTemplate = template.Must(template.New("player").Parse(`<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <title>{{.Title}} - Watch Online</title>
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <style>
    * { box-sizing: border-box; margin: 0; padding: 0; }
    body { background: #0e0e10; color: #efeff1; font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif; display: flex; flex-direction: column; align-items: center; justify-content: center; min-height: 100vh; padding: 20px; }
    .player-card { width: 100%; max-width: 1080px; background: #18181b; border-radius: 12px; overflow: hidden; box-shadow: 0 8px 30px rgba(0,0,0,0.7); }
    video { width: 100%; aspect-ratio: 16/9; background: #000; display: block; }
    .meta { padding: 20px; }
    h1 { font-size: 1.4rem; margin-bottom: 8px; color: #ff640a; }
    .details { font-size: 0.95rem; color: #adadb8; margin-bottom: 12px; }
    .actions { display: flex; gap: 12px; align-items: center; font-size: 0.85rem; color: #adadb8; }
    .btn { display: inline-block; background: #ff640a; color: #fff; padding: 8px 16px; border-radius: 6px; text-decoration: none; font-weight: 600; }
    .btn:hover { background: #e05500; }
    .tag { background: #26262c; padding: 4px 8px; border-radius: 4px; }
  </style>
</head>
<body>
  <div class="player-card">
    <video controls autoplay playsinline src="{{.StreamURL}}"></video>
    <div class="meta">
      <h1>{{.Title}}</h1>
      <div class="details">{{.SeriesTitle}} &bull; Season {{.SeasonNumber}} Episode {{.EpisodeNumber}} &bull; Audio: <span class="tag">{{.Language}}</span> &bull; Quality: <span class="tag">{{.Quality}}</span></div>
      <div class="actions">
        <a class="btn" href="{{.DownloadURL}}">Download MKV</a>
        <span>Auto-deletes from server after {{.IdleTimeout}} with no active viewer</span>
      </div>
    </div>
  </div>
</body>
</html>`))

func (s *apiServer) renderPlayer(w http.ResponseWriter, r *http.Request, jobID, videoPath string, info EpisodeInfo, language, quality string) {
	streamURL := fmt.Sprintf("/api/file/%s?inline=1", jobID)
	downloadURL := fmt.Sprintf("/api/file/%s", jobID)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	data := map[string]any{
		"Title":         info.Title,
		"SeriesTitle":   info.EpisodeMetadata.SeriesTitle,
		"SeasonNumber":  info.EpisodeMetadata.SeasonNumber,
		"EpisodeNumber": info.EpisodeMetadata.EpisodeNumber,
		"Language":      language,
		"Quality":       quality,
		"StreamURL":     streamURL,
		"DownloadURL":   downloadURL,
		"IdleTimeout":   humanDuration(s.watchIdle),
	}
	_ = playerTemplate.Execute(w, data)
	s.store.touch(jobID)
}

// humanDuration renders a duration for humans, e.g. 2m, 90s, 1m30s.
func humanDuration(d time.Duration) string {
	switch {
	case d <= 0:
		return "0s"
	case d%time.Minute == 0:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	default:
		return d.String()
	}
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
