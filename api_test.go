package main

import (
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
	}{
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
			epURL, language, quality := parseLooseDownloadQuery(tt.raw)
			if epURL != tt.epURL || language != tt.language || quality != tt.quality {
				t.Fatalf("got (%q, %q, %q), want (%q, %q, %q)",
					epURL, language, quality, tt.epURL, tt.language, tt.quality)
			}
		})
	}
}

func TestParseDownloadRequestTerseForm(t *testing.T) {
	req := httptest.NewRequest("GET", "/api/download?https://www.crunchyroll.com/watch/GY1234?language(en)?1080p", nil)
	got, err := parseDownloadRequest(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.url != "https://www.crunchyroll.com/watch/GY1234" {
		t.Errorf("url = %q", got.url)
	}
	if got.language != "en" {
		t.Errorf("language = %q", got.language)
	}
	if got.quality != "1080p" {
		t.Errorf("quality = %q", got.quality)
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
		"hi":    "hi-IN",
		"pt-BR": "pt-BR", // unavailable locale passes through
	}
	for in, want := range cases {
		if got := resolveAudioLocale(in, info); got != want {
			t.Errorf("resolveAudioLocale(%q) = %q, want %q", in, got, want)
		}
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
