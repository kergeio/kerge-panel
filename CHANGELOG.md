# Changelog

Notable changes to the Kerge panel. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions
follow [Semantic Versioning](https://semver.org/). Release candidates
are not listed separately.

## [Unreleased]

The first release, v0.1.0.

### Added

- Dashboard with live metrics for every host, as cards or as a list, and
  a summary of current time, hosts online, today's traffic and renewals
  due within 7 days.
- Host pages with charts over 1 hour to 90 days (mean or maximum), read
  from per-minute and per-hour rollups; raw samples and rollups are kept
  for configurable periods.
- Host management: add a host and get its install command, edit, reset
  access (revokes the agent's credential), remove with its history.
- Renewal reminders per host, with snooze and acknowledgement.
- Monthly traffic per host with a reset day, a metering mode and an
  optional limit; network interfaces excluded by name pattern, by default
  and per host.
- First-run setup with a one-time code from the log, a single admin
  account (Argon2id), sessions, CSRF protection and login rate limiting;
  `reset-admin` for a lost password.
- Addresses and tokens masked on screen until revealed.
- Settings for language, time zone, date format and data retention.
- Docker image for linux/amd64 and linux/arm64 on
  `ghcr.io/kergeio/kerge`, with the license texts of all code in it in
  `/usr/share/doc/kerge-panel/`, run with Docker Compose: with its own Caddy
  (automatic certificates) or behind a reverse proxy you already run,
  each for a domain proxied by Cloudflare or not.
- `install.sh`: installs, upgrades and switches modes, after checking the
  signature and checksums of everything it downloads; fetches
  Cloudflare's address ranges on every run; checks ports and DNS first
  and reaches the panel through its public address last.
