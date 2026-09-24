package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/google/uuid"
)

var deviceId = uuid.NewString()

// tokenMu guards the package-level access token and the etp_rt cookie it was
// derived from. The API server refreshes the token from concurrent request
// goroutines, so reads and writes must be synchronized; the CLI is
// single-threaded but pays nothing for it.
var tokenMu sync.RWMutex
var etpRtCookie string

// getToken returns the current Crunchyroll access token.
func getToken() string {
	tokenMu.RLock()
	defer tokenMu.RUnlock()
	return token
}

// setToken replaces the current Crunchyroll access token.
func setToken(t string) {
	tokenMu.Lock()
	token = t
	tokenMu.Unlock()
}

// getEtpRt returns the etp_rt cookie last used to obtain an access token. It is
// needed to silently refresh an expired token mid-download.
func getEtpRt() string {
	tokenMu.RLock()
	defer tokenMu.RUnlock()
	return etpRtCookie
}

// setCredentials stores an access token together with the etp_rt cookie it came
// from, so a later 401 can be refreshed automatically.
func setCredentials(accessToken, etpRt string) {
	tokenMu.Lock()
	token = accessToken
	etpRtCookie = etpRt
	tokenMu.Unlock()
}

type CrunchyrollTokenResponse struct {
	AccessToken string `json:"access_token"`
}

// GetAccessToken fetches an access token from Crunchyroll, panicking on
// failure. The CLI relies on this; server code should use tryGetAccessToken.
func GetAccessToken(etpRt string) string {
	t, err := tryGetAccessToken(etpRt)
	if err != nil {
		panic(err)
	}
	return t
}

// tryGetAccessToken is GetAccessToken without the panic, so an HTTP handler can
// turn a bad credential into a 401 response instead of an aborted connection.
func tryGetAccessToken(etpRt string) (string, error) {
	if strings.TrimSpace(etpRt) == "" {
		return "", fmt.Errorf("empty etp_rt cookie")
	}

	body := url.Values{}
	body.Set("device_id", deviceId)
	body.Set("device_type", "Firefox on Linux")
	body.Set("grant_type", "etp_rt_cookie")

	req, err := http.NewRequest(http.MethodPost, "https://www.crunchyroll.com/auth/v1/token", strings.NewReader(body.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Basic bm9haWhkZXZtXzZpeWcwYThsMHE6")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64; rv:147.0) Gecko/20100101 Firefox/147.0")
	req.AddCookie(&http.Cookie{Name: "device_id", Value: deviceId})
	req.AddCookie(&http.Cookie{Name: "etp_rt", Value: etpRt})

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	// Parse JSON response
	res, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	var result CrunchyrollTokenResponse
	if err := json.Unmarshal(res, &result); err != nil {
		return "", fmt.Errorf("failed to get access token: %w", err)
	}
	if result.AccessToken == "" {
		return "", fmt.Errorf("failed to get access token: Crunchyroll returned status %d", resp.StatusCode)
	}

	return result.AccessToken, nil
}
