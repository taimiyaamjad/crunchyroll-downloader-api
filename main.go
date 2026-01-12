package main

import (
	"flag"
	"fmt"
	"os"
	"slices"
	"strings"
)

var (
	token         = ""
	audioLang     = flag.String("audio-lang", "ja-JP", "Audio language")
	subtitlesLang = flag.String("subs-lang", "en-US", "Subtitles language")
	videoQuality  = flag.String("video-quality", "1080p", "Video quality")
	audioQuality  = flag.String("audio-quality", "192k", "Audio quality")
	seasonNumber  = flag.Int("season", 0, "Season number. Not used if an episode link is entered")
	etpRt         = flag.String("etp-rt", "", "The \"etp_rt\" cookie value of your account")
)

func main() {
	url := flag.String("url", "", "URL of the episode/season to download")
	flag.Parse()

	if *url == "" {
		flag.Usage()
		os.Exit(1)
	}
	if *etpRt == "" {
		fmt.Println("You must specify the \"-etp-rt\" option!\n- Open Crunchyroll on your browser and log in.\n- Open developer tools (Ctrl+Shift+I), go to \"Application\", and then \"Cookies\".\n- The value of the \"ept_rt\" cookie is what you need to input into this option.")
		os.Exit(1)
	}

	contentType := strings.Split(*url, "/")[3]
	contentId := strings.Split(*url, "/")[4]
	if len(contentId) != 9 && len(contentId) != 14 {
		print("Invalid URL format, please paste a link like this: https://www.crunchyroll.com/watch/GWDU82Z05/water-hashira-giyu-tomiokas-pain\n")
		os.Exit(1)
	}
	if contentType != "watch" && contentType != "series" {
		print("Invalid URL!\n")
		os.Exit(1)
	}

	// Fetch Crunchyroll access token
	token = GetAccessToken(*etpRt)

	// Episode link
	if contentType == "watch" {
		// Fetch some things
		info := getEpisodeInfo(contentId)
		// Crunchyroll GUIDs works like this: a GUID = an audio language of an episode (so one episode has a GUID for each
		// audio language it has)
		if info.EpisodeMetadata.AudioLocale != *audioLang {
			// Run though info.EpisodeMetadata.Versions to find the correct episode GUID
			correctGuidI := slices.IndexFunc(info.EpisodeMetadata.Versions, func(v *DubVersion) bool {
				return v.AudioLocale == *audioLang
			})

			if correctGuidI == -1 {
				print("! Invalid audio locale. Please put the locale in the \"ja-JP\", \"en-US\"... format.\n")
				os.Exit(1)
			}
			correctGuid := info.EpisodeMetadata.Versions[correctGuidI]
			contentId = (*correctGuid).GUID
		}

		downloadEpisode(contentId, videoQuality, audioQuality, subtitlesLang, info)
	} else { // Anime link
		seasons := getSeasons(contentId)

		if *seasonNumber != 0 {
			var seasonId string
			for _, season := range seasons {
				if season.SeasonNumber == *seasonNumber {
					seasonId = season.ID
					break
				}
			}
			if seasonId == "" {
				fmt.Printf("This anime has no season %v! (note that Crunchyroll may have put weird seasons numbers)", *seasonNumber)
				os.Exit(1)
			}

			episodes := getSeasonEpisodes(seasonId)
			downloadSeason(videoQuality, audioQuality, subtitlesLang, episodes)
		} else {
			print("No season number precised, downloading all seasons...\n")

			for _, season := range seasons {
				episodes := getSeasonEpisodes(season.ID)
				downloadSeason(videoQuality, audioQuality, subtitlesLang, episodes)
			}
		}
	}
}
