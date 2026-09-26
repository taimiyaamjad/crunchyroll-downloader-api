package main

import (
	"archive/zip"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseLooseDownloadQuery(t *testing.T) {
	tests := []struct {
		name     string
		raw      string
		epURL    string
		language string
		quality  string
		season   int
	}{
		{
			name:     "user watch format with space",
			raw:      "https://www.crunchyroll.com/watch/GY1234?language=hi? quality=1080p",
			epURL:    "https://www.crunchyroll.com/watch/GY1234",
			language: "hi",
			quality:  "1080p",
		},
		{
			name:     "terse browser form",
			raw:      "https://www.crunchyroll.com/watch/GY1234?language(en)?1080p",
			epURL:    "https://www.crunchyroll.com/watch/GY1234",
			language: "en",
			quality:  "1080p",
		},
		{
			name:     "standard key/value form",
			raw:      "url=https://www.crunchyroll.com/watch/GY1234&language=en-US&quality=720p",
			epURL:    "https://www.crunchyroll.com/watch/GY1234",
			language: "en-US",
			quality:  "720p",
		},
		{
			name:     "series whole season terse form",
			raw:      "https://www.crunchyroll.com/series/GY9999?season=1?language=hi?1080p",
			epURL:    "https://www.crunchyroll.com/series/GY9999",
			language: "hi",
			quality:  "1080p",
			season:   1,
		},
		{
			name:     "series season short token",
			raw:      "https://www.crunchyroll.com/series/GY9999?s2?hi?720p",
			epURL:    "https://www.crunchyroll.com/series/GY9999",
			language: "hi",
			quality:  "720p",
			season:   2,
		},
		{
			name:     "mixed url first then language(x)",
			raw:      "url=https://www.crunchyroll.com/watch/GY1234?language(hi)?480p",
			epURL:    "https://www.crunchyroll.com/watch/GY1234",
			language: "hi",
			quality:  "480p",
		},
		{
			name:     "bare url only",
			raw:      "https://www.crunchyroll.com/watch/GY1234",
			epURL:    "https://www.crunchyroll.com/watch/GY1234",
			language: "",
			quality:  "",
		},
		{
			name:     "percent-encoded",
			raw:      "https%3A%2F%2Fwww.crunchyroll.com%2Fwatch%2FGY1234%3Flanguage(en)%3F1080p",
			epURL:    "https://www.crunchyroll.com/watch/GY1234",
			language: "en",
			quality:  "1080p",
		},
		{
			name:     "episode alias",
			raw:      "ep=GY1234&lang=ja-JP&resolution=360p",
			epURL:    "GY1234",
			language: "ja-JP",
			quality:  "360p",
		},
		{
			name:     "empty",
			raw:      "",
			epURL:    "",
			language: "",
			quality:  "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			epURL, language, quality, season, _, _ := parseLooseDownloadQuery(tt.raw)
			if epURL != tt.epURL || language != tt.language || quality != tt.quality || (tt.season != 0 && season != tt.season) {
				t.Fatalf("got (%q, %q, %q, %d), want (%q, %q, %q, %d)",
					epURL, language, quality, season, tt.epURL, tt.language, tt.quality, tt.season)
			}
		})
	}
}

func TestParseWatchRequest(t *testing.T) {
	// A raw space is not a valid HTTP request target; browsers encode it as %20.
	req := httptest.NewRequest("GET", "/api/watch?https://www.crunchyroll.com/watch/GY1234?language=hi?%20quality=1080p", nil)
	got, err := parseDownloadRequest(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.url != "https://www.crunchyroll.com/watch/GY1234" {
		t.Errorf("url = %q", got.url)
	}
	if got.language != "hi" {
		t.Errorf("language = %q", got.language)
	}
	if got.quality != "1080p" {
		t.Errorf("quality = %q", got.quality)
	}
}

func TestParseSeasonDownloadRequest(t *testing.T) {
	req := httptest.NewRequest("GET", "/api/download?url=https://www.crunchyroll.com/series/GY1234&season=1&language=hi&quality=1080p", nil)
	got, err := parseDownloadRequest(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.url != "https://www.crunchyroll.com/series/GY1234" {
		t.Errorf("url = %q", got.url)
	}
	if got.season != 1 {
		t.Errorf("season = %d, want 1", got.season)
	}
	if got.language != "hi" {
		t.Errorf("language = %q, want hi", got.language)
	}
}

func TestCanonicalLocale(t *testing.T) {
	cases := map[string]string{
		"hi":    "hi-IN",
		"hindi": "hi-IN",
		"HI":    "hi-IN",
		"en":    "en-US",
		"ja":    "ja-JP",
		"ja-JP": "ja-JP",
		"hi-IN": "hi-IN",
	}
	for in, want := range cases {
		if got := canonicalLocale(in); got != want {
			t.Errorf("canonicalLocale(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseDownloadRequestMissingURL(t *testing.T) {
	req := httptest.NewRequest("GET", "/api/download?language=en&quality=1080p", nil)
	if _, err := parseDownloadRequest(req); err == nil {
		t.Fatal("expected an error when no URL is supplied")
	}
}

func TestParseDownloadRequestJSONFormat(t *testing.T) {
	req := httptest.NewRequest("GET", "/api/download?url=https://www.crunchyroll.com/watch/GY1234&format=json", nil)
	got, err := parseDownloadRequest(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !got.asJSON {
		t.Fatal("format=json should set asJSON")
	}
}

func TestNormalizeQuality(t *testing.T) {
	cases := map[string]string{
		"":       "1080p",
		"1080":   "1080p",
		"1080P":  "1080p",
		" 720p ": "720p",
		"480p":   "480p",
	}
	for in, want := range cases {
		if got := normalizeQuality(in); got != want {
			t.Errorf("normalizeQuality(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestResolveAudioLocale(t *testing.T) {
	info := EpisodeInfo{
		EpisodeMetadata: EpisodeMetadata{
			AudioLocale: "ja-JP",
			Versions: []*DubVersion{
				{AudioLocale: "ja-JP"},
				{AudioLocale: "en-US"},
				{AudioLocale: "hi-IN"},
			},
		},
	}

	cases := map[string]string{
		"":      "ja-JP", // primary fallback
		"en":    "en-US", // bare code matched against dubs
		"en-US": "en-US", // exact
		"EN-us": "en-US", // case-insensitive
		"hi":    "hi-IN", // Hindi short code mapped to hi-IN
		"hindi": "hi-IN",
		"pt-BR": "pt-BR", // unavailable locale passes through
	}
	for in, want := range cases {
		if got := resolveAudioLocale(in, info); got != want {
			t.Errorf("resolveAudioLocale(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCreateZipArchive(t *testing.T) {
	dir := t.TempDir()
	f1 := filepath.Join(dir, "ep1.mkv")
	f2 := filepath.Join(dir, "ep2.mkv")
	if err := os.WriteFile(f1, []byte("fake episode 1 video"), 0o666); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f2, []byte("fake episode 2 video"), 0o666); err != nil {
		t.Fatal(err)
	}

	zipPath := filepath.Join(dir, "season.zip")
	if err := createZipArchive(zipPath, []string{f1, f2}, dir); err != nil {
		t.Fatalf("createZipArchive failed: %v", err)
	}

	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		t.Fatalf("failed to open created zip: %v", err)
	}
	defer zr.Close()

	if len(zr.File) != 2 {
		t.Fatalf("expected 2 files in zip, got %d", len(zr.File))
	}
}

// TestTempStoreSweep checks that files are actually removed once their TTL
// elapses, which is what frees disk space after the 10-minute window.
func TestTempStoreSweep(t *testing.T) {
	root := t.TempDir()
	jobDir := filepath.Join(root, "job1")
	if err := os.MkdirAll(jobDir, 0o777); err != nil {
		t.Fatal(err)
	}
	video := filepath.Join(jobDir, "episode.mkv")
	if err := os.WriteFile(video, []byte("fake video"), 0o666); err != nil {
		t.Fatal(err)
	}

	store := newTempStore(20*time.Millisecond, 0)
	store.add("job1", jobDir, video)

	if _, ok := store.get("job1"); !ok {
		t.Fatal("fresh entry should be present")
	}
	if store.count() != 1 {
		t.Fatalf("count = %d, want 1", store.count())
	}

	time.Sleep(40 * time.Millisecond)
	store.sweep()

	if store.count() != 0 {
		t.Fatalf("entry should have been swept, count = %d", store.count())
	}
	if _, err := os.Stat(jobDir); !os.IsNotExist(err) {
		t.Fatalf("job directory should be deleted, stat err = %v", err)
	}
	if _, ok := store.get("job1"); ok {
		t.Fatal("expired entry should not be returned by get")
	}
}

// TestTempStoreTouchExtends makes sure a slow client streaming a file keeps it
// alive past its original expiry.
func TestTempStoreTouchExtends(t *testing.T) {
	store := newTempStore(50*time.Millisecond, 0)
	store.add("job1", t.TempDir(), "video.mkv")

	time.Sleep(30 * time.Millisecond)
	store.touch("job1")
	time.Sleep(30 * time.Millisecond)

	if _, ok := store.get("job1"); !ok {
		t.Fatal("touched entry should still be alive")
	}
}

// TestTempStoreWatchIdleSweep verifies the core requirement: a watch stream is
// deleted once nobody has been watching it for the idle timeout.
func TestTempStoreWatchIdleSweep(t *testing.T) {
	root := t.TempDir()
	jobDir := filepath.Join(root, "watch1")
	if err := os.MkdirAll(jobDir, 0o777); err != nil {
		t.Fatal(err)
	}
	video := filepath.Join(jobDir, "episode.mkv")
	if err := os.WriteFile(video, []byte("fake stream"), 0o666); err != nil {
		t.Fatal(err)
	}

	// Long download TTL, short watch idle timeout.
	store := newTempStore(time.Hour, 20*time.Millisecond)
	store.addWatch("watch1", jobDir, video, "watch:GY1:hi-IN:1080p")

	if _, ok := store.get("watch1"); !ok {
		t.Fatal("fresh watch entry should be present")
	}

	time.Sleep(40 * time.Millisecond)
	store.sweep()

	if store.count() != 0 {
		t.Fatalf("idle watch entry should have been swept, count = %d", store.count())
	}
	if _, err := os.Stat(jobDir); !os.IsNotExist(err) {
		t.Fatalf("idle watch directory should be deleted, stat err = %v", err)
	}
	if _, _, ok := store.findByKey("watch:GY1:hi-IN:1080p"); ok {
		t.Fatal("idle watch entry should not be reusable from cache")
	}
}

// TestTempStoreWatchActiveNotDeleted makes sure an in-progress stream is never
// deleted, even past the idle timeout, and that the countdown starts again once
// the viewer stops.
func TestTempStoreWatchActiveNotDeleted(t *testing.T) {
	root := t.TempDir()
	jobDir := filepath.Join(root, "watch2")
	if err := os.MkdirAll(jobDir, 0o777); err != nil {
		t.Fatal(err)
	}
	video := filepath.Join(jobDir, "episode.mkv")
	if err := os.WriteFile(video, []byte("fake stream"), 0o666); err != nil {
		t.Fatal(err)
	}

	store := newTempStore(time.Hour, 30*time.Millisecond)
	store.addWatch("watch2", jobDir, video, "")

	// Simulate a viewer streaming for longer than the idle timeout.
	store.acquire("watch2")
	time.Sleep(60 * time.Millisecond)
	store.sweep()
	if _, ok := store.get("watch2"); !ok {
		t.Fatal("an actively streamed file must not be swept")
	}
	if _, err := os.Stat(jobDir); err != nil {
		t.Fatalf("actively streamed directory must still exist: %v", err)
	}

	// Viewer stops; after the idle window it should be deleted.
	store.release("watch2")
	time.Sleep(60 * time.Millisecond)
	store.sweep()
	if store.count() != 0 {
		t.Fatalf("entry should be swept after the viewer left, count = %d", store.count())
	}
}

// TestTempStoreWatchTouchKeepsAlive checks that continued playback (a new range
// request) keeps refreshing the idle clock.
func TestTempStoreWatchTouchKeepsAlive(t *testing.T) {
	store := newTempStore(time.Hour, 40*time.Millisecond)
	store.addWatch("watch3", t.TempDir(), "video.mkv", "")

	for i := 0; i < 3; i++ {
		time.Sleep(25 * time.Millisecond)
		store.touch("watch3")
		store.sweep()
		if _, ok := store.get("watch3"); !ok {
			t.Fatalf("watch entry died on iteration %d despite being touched", i)
		}
	}
}

// TestTempStoreDownloadIgnoresWatchIdle makes sure the watch idle timeout does
// not shorten a normal download's TTL.
func TestTempStoreDownloadIgnoresWatchIdle(t *testing.T) {
	store := newTempStore(time.Hour, 10*time.Millisecond)
	store.add("job1", t.TempDir(), "video.mkv")

	time.Sleep(30 * time.Millisecond)
	store.sweep()

	if _, ok := store.get("job1"); !ok {
		t.Fatal("a normal download must not be removed by the watch idle timeout")
	}
}

// TestTempStoreSweepInterval checks the sweeper runs often enough to honour a
// short idle timeout, but never faster than once a second.
func TestTempStoreSweepInterval(t *testing.T) {
	if got := newTempStore(time.Hour, 2*time.Minute).sweepInterval(); got != 30*time.Second {
		t.Errorf("default interval = %s, want 30s", got)
	}
	if got := newTempStore(time.Hour, 2*time.Second).sweepInterval(); got != time.Second {
		t.Errorf("short idle interval = %s, want 1s", got)
	}
}

// ---------------------------------------------------------------------------
// API token authentication
// ---------------------------------------------------------------------------

// TestLoadOrCreateAPITokenDefault verifies the permanent, documented token is
// used and persisted on the first run, then reloaded unchanged afterwards.
func TestLoadOrCreateAPITokenDefault(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "api_token")

	info, err := loadOrCreateAPIToken("", path)
	if err != nil {
		t.Fatal(err)
	}
	if info.token != defaultAPIToken {
		t.Fatalf("token = %q, want built-in %q", info.token, defaultAPIToken)
	}
	if !info.builtin {
		t.Fatal("first run should report the built-in token")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("token file was not written: %v", err)
	}
	if strings.TrimSpace(string(b)) != defaultAPIToken {
		t.Fatalf("token file = %q, want %q", strings.TrimSpace(string(b)), defaultAPIToken)
	}

	// A restart must load the exact same permanent token from disk.
	again, err := loadOrCreateAPIToken("", path)
	if err != nil {
		t.Fatal(err)
	}
	if again.token != defaultAPIToken {
		t.Fatalf("reloaded token = %q, want %q", again.token, defaultAPIToken)
	}
	if again.builtin || again.explicit {
		t.Fatalf("reloaded token should come from the file, got %+v", again)
	}
}

// TestLoadOrCreateAPITokenExplicitWins checks -api-token overrides everything.
func TestLoadOrCreateAPITokenExplicitWins(t *testing.T) {
	path := filepath.Join(t.TempDir(), "api_token")
	if err := os.WriteFile(path, []byte("crdl_from_file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := loadOrCreateAPIToken("crdl_from_flag", path)
	if err != nil {
		t.Fatal(err)
	}
	if info.token != "crdl_from_flag" || !info.explicit {
		t.Fatalf("explicit token not honoured: %+v", info)
	}
}

// TestLoadOrCreateAPITokenFileWins checks a previously saved token is reused.
func TestLoadOrCreateAPITokenFileWins(t *testing.T) {
	path := filepath.Join(t.TempDir(), "api_token")
	if err := os.WriteFile(path, []byte("crdl_from_file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := loadOrCreateAPIToken("", path)
	if err != nil {
		t.Fatal(err)
	}
	if info.token != "crdl_from_file" {
		t.Fatalf("token = %q, want crdl_from_file", info.token)
	}
}

// TestExtractAPIToken covers every way a caller can present the token.
func TestExtractAPIToken(t *testing.T) {
	tests := []struct {
		name      string
		target    string
		headers   map[string]string
		want      string
		wantQuery bool
	}{
		{
			name:    "authorization bearer",
			target:  "/api/download?url=x",
			headers: map[string]string{"Authorization": "Bearer crdl_abc"},
			want:    "crdl_abc",
		},
		{
			name:    "x-api-key header",
			target:  "/api/download?url=x",
			headers: map[string]string{"X-API-Key": "crdl_abc"},
			want:    "crdl_abc",
		},
		{
			name:      "query parameter",
			target:    "/api/download?url=x&token=crdl_abc",
			want:      "crdl_abc",
			wantQuery: true,
		},
		{
			name:      "terse query with ampersand",
			target:    "/api/watch?https://x/watch/GY1?language(hi)&token=crdl_abc",
			want:      "crdl_abc",
			wantQuery: true,
		},
		{
			name:      "terse query without ampersand",
			target:    "/api/watch?https://x/watch/GY1?token=crdl_abc",
			want:      "crdl_abc",
			wantQuery: true,
		},
		{
			name:   "no token",
			target: "/api/download?url=x",
			want:   "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", tc.target, nil)
			for k, v := range tc.headers {
				req.Header.Set(k, v)
			}
			got, fromQuery := extractAPIToken(req)
			if got != tc.want {
				t.Fatalf("token = %q, want %q", got, tc.want)
			}
			if fromQuery != tc.wantQuery {
				t.Fatalf("fromQuery = %v, want %v", fromQuery, tc.wantQuery)
			}
		})
	}
}

// TestTokenMatches makes sure comparison rejects empty and wrong values.
func TestTokenMatches(t *testing.T) {
	if !tokenMatches("crdl_secret", "crdl_secret") {
		t.Error("identical tokens should match")
	}
	if tokenMatches("crdl_wrong", "crdl_secret") {
		t.Error("different tokens must not match")
	}
	if tokenMatches("", "crdl_secret") || tokenMatches("crdl_secret", "") {
		t.Error("empty tokens must never match")
	}
}

// TestRequireAuth verifies the middleware blocks unauthenticated API calls and
// lets public endpoints and valid tokens through.
func TestRequireAuth(t *testing.T) {
	auth := &apiAuth{token: "crdl_secret"}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	handler := auth.requireAuth(next)

	protected := []struct {
		name   string
		mutate func(*http.Request)
	}{
		{name: "no token", mutate: func(*http.Request) {}},
		{name: "wrong token", mutate: func(r *http.Request) { r.Header.Set("Authorization", "Bearer nope") }},
	}
	for _, tc := range protected {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/api/download?url=x", nil)
			tc.mutate(req)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", rec.Code)
			}
		})
	}

	// Valid token via header.
	req := httptest.NewRequest("GET", "/api/download?url=x", nil)
	req.Header.Set("X-API-Key", "crdl_secret")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Body.String() != "ok" {
		t.Fatalf("valid token: status = %d body = %q", rec.Code, rec.Body.String())
	}

	// Valid token via query also sets a cookie for the browser.
	req = httptest.NewRequest("GET", "/api/download?url=x&token=crdl_secret", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("query token: status = %d, want 200", rec.Code)
	}
	var found bool
	for _, c := range rec.Result().Cookies() {
		if c.Name == apiTokenCookie && c.Value == "crdl_secret" {
			found = true
		}
	}
	if !found {
		t.Fatalf("query token should set the %s cookie", apiTokenCookie)
	}

	// Public endpoints never require a token.
	for _, path := range []string{"/", "/api/health"} {
		rec = httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("public path %s: status = %d, want 200", path, rec.Code)
		}
	}
}

// TestAuthDisabledAllowsEverything checks a nil *apiAuth disables the check.
func TestAuthDisabledAllowsEverything(t *testing.T) {
	var auth *apiAuth
	handler := auth.requireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest("GET", "/api/download?url=x", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("disabled auth: status = %d, want 200", rec.Code)
	}
}
