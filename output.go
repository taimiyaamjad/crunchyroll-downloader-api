package main

import (
	"fmt"
	"os"
	"os/exec"
)

// mergeEverything merges audio, video and subtitles in a single MKV container
func mergeEverything(subtitlesLang *string, info EpisodeInfo) {
	args := []string{
		"-i", "temp_video.mp4", "-i", "temp_audio.mp3",
	}

	if _, err := os.Stat("subs.ass"); err == nil {
		args = append(args,
			"-i", "subs.ass",
			"-c:s", "copy", // keep subtitles as ASS
			"-metadata:s:s:0", fmt.Sprintf("title=%s", languageNames[*subtitlesLang]),
		)
	}

	args = append(args,
		"-c:v", "copy", "-c:a", "copy",
		"-metadata:g", "title="+fmt.Sprintf("S%02vE%02v - %s", info.EpisodeMetadata.SeasonNumber, info.EpisodeMetadata.EpisodeNumber, info.Title),
		"-metadata:g", "show="+info.EpisodeMetadata.SeriesTitle,
		"-metadata:g", "track="+fmt.Sprintf("%v", info.EpisodeMetadata.EpisodeNumber),
		"-metadata:g", "season_number="+fmt.Sprintf("%v", info.EpisodeMetadata.EpisodeNumber),
		"output.mkv",
	)

	cmd := exec.Command("ffmpeg", args...)
	if err := cmd.Run(); err != nil {
		panic(err)
	}

	print("\nOutput file: output.mkv\n")
}
