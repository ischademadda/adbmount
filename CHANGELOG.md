# Changelog

## Unreleased

- Single `adbmount` binary with subcommands replaces the `adbfs` binary plus bash launcher.
- New: `devices`, `doctor`, `ls`, `pull`, `push` (rsync-style, parallel, size-verified, resumable),
  `wifi` (enable / pair / connect / scan / disconnect / usb), `completion`, `up --open`.
- One phone visible over USB and Wi-Fi is no longer reported as "several devices".

## Initial version

- `adbfs` FUSE-T filesystem over adb with read-ahead, write-back on close, NFC/NFD handling,
  macOS junk filtering and retry on transient adb errors.
