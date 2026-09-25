package main

import (
	"archive/zip"
	"net/http/httptest"
	"os"
	"path/filepath"
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
	req := httptest.NewRequest("GET", "/api/watch?https://www.crunchyroll.com/watch/GY1234?language=hi? quality=1080p", nil)
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

	store := newTempStore(20 * time.Millisecond)
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
	store := newTempStore(50 * time.Millisecond)
	store.add("job1", t.TempDir(), "video.mkv")

	time.Sleep(30 * time.Millisecond)
	store.touch("job1")
	time.Sleep(30 * time.Millisecond)

	if _, ok := store.get("job1"); !ok {
		t.Fatal("touched entry should still be alive")
	}
}
