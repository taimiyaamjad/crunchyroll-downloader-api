package main

import (
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/unki2aut/go-mpd"
)

func parseManifest(url string) *mpd.MPD {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		panic(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64; rv:147.0) Gecko/20100101 Firefox/147.0")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		panic(err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		panic(err)
	}
	mpd := new(mpd.MPD)
	mpd.Decode(body)

	return mpd
}

func getBaseUrl(set *mpd.AdaptationSet, isVideoSet bool, quality string) (*string, *string) {
	for _, representation := range set.Representations {
		if isVideoSet {
			toInt, _ := strconv.ParseInt(strings.ReplaceAll(quality, "p", ""), 10, 64)
			if *representation.Height == uint64(toInt) {
				return &representation.BaseURL[0].Value, representation.ID
			}
		} else {
			toInt, _ := strconv.ParseInt(strings.ReplaceAll(quality, "k", ""), 10, 64)
			if (*representation.Bandwidth - 1) == uint64(toInt*1000) {
				return &representation.BaseURL[0].Value, representation.ID
			}
		}
	}
	return nil, nil
}

func expandTimeline(timeline []*mpd.SegmentTimelineS, startNumber int64) []int64 {
	var result []int64
	segNum := startNumber

	for _, s := range timeline {
		repeat := int64(0)
		if s.R != nil && *s.R > 0 {
			repeat = *s.R
		}

		total := repeat + 1 // DASH rule: total segments = r + 1

		for i := int64(0); i < total; i++ {
			result = append(result, segNum)
			segNum++
		}
	}

	return result
}
