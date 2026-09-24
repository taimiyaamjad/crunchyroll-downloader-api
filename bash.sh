#!/bin/bash

clear

echo "========================================"
echo "     Crunchyroll Downloader "
echo "========================================"
echo

# ETP-RT
read -rp "Enter etp-rt: " ETP_RT
echo
echo

# Video quality
echo "Select video quality:"
echo "1) 1080p"
echo "2) 720p"
echo "3) 480p"
echo "4) 360p"
echo

read -rp "Enter choice [1-4]: " QUALITY_CHOICE

case "$QUALITY_CHOICE" in
    1) VIDEO_QUALITY="1080p" ;;
    2) VIDEO_QUALITY="720p" ;;
    3) VIDEO_QUALITY="480p" ;;
    4) VIDEO_QUALITY="360p" ;;
    *)
        echo "Invalid quality selection."
        exit 1
        ;;
esac

echo

# Audio language
echo "Select audio language:"
echo "1) English (en-US)"
echo "2) Hindi (hi-IN)"
echo

read -rp "Enter choice [1-2]: " LANGUAGE_CHOICE

case "$LANGUAGE_CHOICE" in
    1) AUDIO_LANG="en-US" ;;
    2) AUDIO_LANG="hi-IN" ;;
    *)
        echo "Invalid language selection."
        exit 1
        ;;
esac

echo

# Video URL
read -rp "Enter Crunchyroll video URL: " VIDEO_URL

clear

echo
echo "========================================"
echo "Settings:"
echo "Quality : $VIDEO_QUALITY"
echo "Audio   : $AUDIO_LANG"
echo "Subs    : None"
echo "URL     : $VIDEO_URL"
echo "========================================"
echo
sleep 2

clear

# Run downloader
./crunchyroll-downloader \
    -url "$VIDEO_URL" \
    -etp-rt "$ETP_RT" \
    -video-quality "$VIDEO_QUALITY" \
    -audio-lang "$AUDIO_LANG" \
    -subs-lang ""

echo "Done.. Successfully"
