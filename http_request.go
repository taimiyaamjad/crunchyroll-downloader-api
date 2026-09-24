package main

import (
	"net/http"
)

func DoRequest(req *http.Request) (*http.Response, error) {
	return doRequest(req, true)
}

// doRequest performs req and, on a 401, refreshes the access token once from
// the stored etp_rt cookie and retries. canRetry prevents an infinite loop when
// the credential is genuinely invalid.
func doRequest(req *http.Request, canRetry bool) (*http.Response, error) {
	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusUnauthorized && canRetry {
		print("Access token expired. Refetching one...\n")
		resp.Body.Close()

		// Refetch an access token. In server mode the cookie is whatever the
		// request (or -etp-rt) last supplied; if there is none, return the 401
		// to the caller rather than panicking the handler.
		etp := getEtpRt()
		if etp == "" {
			return resp, nil
		}
		newToken, err := tryGetAccessToken(etp)
		if err != nil {
			return resp, nil
		}
		setToken(newToken)
		req.Header.Set("Authorization", "Bearer "+newToken)

		// The first attempt already consumed req.Body, so reset it from
		// GetBody before retrying or the retry sends an empty/truncated
		// payload (which broke license requests as "invalid license type").
		if req.GetBody != nil {
			if body, err := req.GetBody(); err == nil {
				req.Body = body
			}
		}
		// and retry the request
		return doRequest(req, false)
	}

	return resp, err
}
