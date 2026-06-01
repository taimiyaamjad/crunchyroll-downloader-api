# Changelog

## 1.2.0

- Added support for downloading multiple audio tracks and multiple subtitles into a single file (`--audio-lang ja-JP,en-US`, `--subs-lang en-US,es-419`). The first of each is marked as the default track, and each track is tagged with its language so media servers can select them.
- An episode is skipped if any requested audio or subtitle language is unavailable for it.
- Parallel segment downloads (10 workers) for much faster downloads
- Retry with backoff on connection errors instead of crashing
- Added `--urls` flag to batch download from a text file with one URL per line
- Invalid URLs in batch mode are skipped instead of stopping the whole process

## 1.1.1

- Optimized code, tried to handle errors
- Some random fixes
- Added a way to automatically refetch an access token if the current one expires

## 1.1.0

- Added support for downloading entire seasons
- Fixed MPD parsing
- Temporary downloaded files (video, audio segments and subtitles) are now stored in the OS temporary files then deleted
- Fixed FFmpeg merge command
- Docs improvements
- Support for `device_id.bin` and `private_key.pem` files

## 1.0.0

Initial release
