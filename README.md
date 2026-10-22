# adbmount

[![ci](https://github.com/ischademadda/adbmount/actions/workflows/ci.yml/badge.svg)](https://github.com/ischademadda/adbmount/actions/workflows/ci.yml)
[![license: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

[Русская версия](README.ru.md)

Mount an Android phone's storage (internal memory and SD card) on macOS as a normal read-write
volume over **adb**. It needs no kernel extension (it uses [FUSE-T](https://www.fuse-t.org/)) and
nothing on the phone except USB debugging.

```
phone ──USB── adb ──► adbfs (Go, cgofuse) ──► FUSE-T (NFS) ──► Finder, Python, ffmpeg, foobar2000…
```

The repository contains two things: `adbfs`, the filesystem daemon (Go), and `adbmount`, a small
shell helper that finds the SD card, starts one `adbfs` per volume and unmounts them cleanly.

## Requirements

* macOS (Apple Silicon or Intel)
* [FUSE-T](https://www.fuse-t.org/) and Android platform tools
* Go (only to build; the version in `go.mod`)
* An Android phone with **USB debugging** enabled and the computer authorized

## Install

```bash
brew install --cask fuse-t        # asks for your admin password
brew install android-platform-tools go

git clone https://github.com/ischademadda/adbmount.git
cd adbmount
make build                        # builds ./adbfs
sudo make install                 # optional: copies adbfs + adbmount to /usr/local/bin
```

`adbmount` looks for `adbfs` next to itself (override with `ADBFS_BIN=/path/to/adbfs`), so keep the
two files together. Use `make install PREFIX=$HOME/.local` to install without sudo.

## Usage

```bash
adbmount up            # internal storage → ~/mnt/android, SD card → ~/mnt/sdcard (auto-detected)
adbmount up --ro       # same, but read-only (safe for scanning/browsing)
adbmount up --only sdcard
adbmount status
adbmount down
```

`adbmount` keeps pid files in `~/Library/Caches/adbfs/run` and logs next to them
(`adbmount log sdcard`). Change the mount root with `ADBMOUNT_DIR=/some/dir`. With several devices
attached, pick one with `ANDROID_SERIAL=<serial> adbmount up` (see `adb devices`).

Direct use:

```bash
adbfs -root /storage/ABCD-1234 -mount ~/mnt/sdcard -volname SDCard [-ro] [-allow-junk]
```

| flag | meaning |
|---|---|
| `-root` | directory on the phone exposed as the mount root (default `/sdcard`) |
| `-mount` | local mount point, created if missing |
| `-serial` | adb device serial when several devices are attached |
| `-adb` | path to the adb binary (default: `adb` from `PATH`) |
| `-ro` | read-only mount; every mutation returns `EROFS` |
| `-allow-junk` | store macOS `._*` / `.DS_Store` files instead of discarding them |
| `-volname` | name shown in Finder |

Set `ADBFS_TRACE=1` to log every operation with its duration.

> **Note:** read-write mode modifies the phone's storage directly. Start with `--ro` if you only
> want to look around, and keep a backup of anything you cannot lose. The software is provided
> as-is, see [LICENSE](LICENSE).

## How it works

* **Listing**: one `adb exec-out find … -exec stat` per directory, cached for 10 s. Changes made
  *outside* the mount (e.g. by the phone itself) become visible within that window.
* **Reading**: a file is streamed in the background into a local temp copy, never more than 8 MB
  ahead of what was read. A read far past the streamed part (tag readers jump to the end of a
  file) is served by a separate `dd skip=` range request instead of waiting. Both were needed:
  `tail -c +N` scans the file from the start and took ~1 s on a 300 MB FLAC.
* **Writing**: a file opened for writing is first downloaded completely, modified locally, and
  uploaded **when it is flushed/closed** via `temp file + mv`, so the phone never holds a
  half-written file. A failed upload keeps the file dirty and the next flush retries.
  Nothing appears on the phone before the writer closes the file (except that `create` makes an
  empty file immediately so later lookups see it).
* **Renames/deletes/mkdir** are single `mv`/`rm`/`mkdir` calls on the phone, so moving folders
  inside a volume is instant regardless of size.
* **Unicode**: macOS hands names over as NFD while Android stores NFC (or vice versa). Lookups
  fall back to an NFC-normalised walk, so both spellings work.
* **macOS junk**: `._*`, `.DS_Store`, `.Trashes`, `.fseventsd`, … are accepted while open and then
  discarded, so copying from a Mac no longer litters the card with AppleDouble files. The volume
  also serves `.metadata_never_index` so Spotlight does not crawl the phone.
* **Resilience**: transient adb errors (device offline, daemon restarted, "closed") are retried
  with back-off for ~10 s. Per-process temp dirs live in `~/Library/Caches/adbfs/<pid>-*`; dirs
  left behind by a crashed run are removed on the next start.

Measured on a USB-attached phone: sequential read ≈ 17–20 MB/s, write ≈ 8–9 MB/s, catalog
scan of ~50 FLAC files ≈ 16 s (≈ 0.3 s/file, dominated by one adb round trip per file).

## Limitations

* Writes are whole-file: changing one tag in a 300 MB FLAC re-uploads 300 MB.
* No symlinks, hard links, xattrs or permissions (Android's `/sdcard` has none).
* Free space shown in Finder is refreshed every 30 s.
* Only one writer per file is meaningful; there is no locking.
* macOS only (FUSE-T). Linux users have better options, e.g. `adb` + `sshfs`/`madbfs`.
* For bulk work that rewrites many files (re-encoding a whole library), running the tool on the
  phone itself (Termux) is much faster than going through the cable.

## Development

```bash
make check      # go vet + go test + build
```

`device` (in `adb.go`) is the only thing the filesystem knows about the phone. `fs_test.go` implements
it in memory, so the whole filesystem — paging, create/write/flush, truncate, rename, unlink of
open files, junk filtering, NFC/NFD, read-ahead back-pressure, upload retry — is tested without
a phone or FUSE. Tests were mutation-checked (breaking flush, unlink or back-pressure makes
specific tests fail).

| file | role |
|---|---|
| `main.go` | flags, temp-dir hygiene, mount |
| `fs.go` | the FUSE filesystem (cache, downloads, write-back) |
| `adb.go` | `device` interface + adb implementation, retries |
| `fs_test.go`, `main_test.go` | in-memory device + tests |
| `adbmount` | mount/unmount/status helper |

## Troubleshooting

* **Mount is empty / `Resource busy` on unmount**: `adbmount down` (force-unmounts and kills only
  the pids it started). Never `pkill -f adbfs`: it also matches the other mount.
* **`no phone visible`**: check the cable, enable USB debugging and accept the authorization prompt
  on the phone; `adb devices` must show `device`, not `unauthorized`.
* **Finder is slow on huge folders**: use `--ro` while browsing; every directory is one adb call.
* **A folder created on the phone does not show up**: wait ≤ 10 s (listing cache).
* **`cannot stat /storage/... on device`**: the card is not mounted/unlocked on the phone, or the
  UUID changed; `adb shell ls /storage` shows the real name.
* **Build fails with `fuse.h` not found**: install FUSE-T (`brew install --cask fuse-t`).

## License

[MIT](LICENSE)
