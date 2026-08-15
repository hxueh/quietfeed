# QuietFeed

QuietFeed is a small, single-user RSS synchronization daemon written in Go. It stores feeds and articles in SQLite and exposes the Google Reader-compatible API used by Reeder Classic. It has no web UI and listens on a Unix-domain socket behind an HTTPS reverse proxy.

## Features

- Google Reader-compatible synchronization for Reeder Classic
- SQLite storage with WAL enabled
- Feed folders, unread state, stars, and OPML import
- Configurable refresh and retention periods
- Conditional feed requests with failure backoff
- A 10 MiB decompressed response limit and private-network fetch protection
- A hardened systemd unit and Unix socket integration with Caddy

## Build and run

Go 1.26 or newer is required.

```sh
go build -o quietfeed .
mkdir -p ./run
QUIETFEED_PASSWORD='choose-a-long-password' QUIETFEED_SOCKET="$PWD/run/quietfeed.sock" ./quietfeed
```

The database is created as `quietfeed.db`. The service refreshes feeds immediately at startup and every 20 minutes thereafter. During each refresh, read articles older than 90 days are deleted; unread and starred articles are retained unless they have also been marked read.

The first successful fetch for a newly added feed keeps at most the 20 newest articles. Later refreshes add newer posts without backfilling entries older than that initial boundary.

Feeds that fail three consecutive refreshes are retried only every six refresh periods. With the default 20-minute interval, that means one retry every 120 minutes. Any successful response resets the feed to the normal schedule.

By default, feed URLs resolving to loopback, private, link-local, unspecified, or multicast addresses are rejected. Set `QUIETFEED_ALLOW_PRIVATE_FEEDS=true` only when private-network feeds are intentional and every authenticated client is trusted.

## Install on Linux

Install Go and Caddy. Package names may differ outside Ubuntu and Debian:

```sh
sudo apt update
sudo apt install -y golang-go caddy
```

Build QuietFeed:

```sh
git clone https://github.com/hxueh/quietfeed.git
cd quietfeed
make build
```

Create the service account and directories, then install the binary and example settings:

```sh
sudo useradd --system --home-dir /var/lib/quietfeed --create-home --shell /usr/sbin/nologin quietfeed
sudo install -o quietfeed -g quietfeed -m 0750 -d /var/lib/quietfeed
sudo install -o root -g root -m 0755 bin/quietfeed /usr/local/bin/quietfeed
sudo install -o root -g root -m 0600 quietfeed.env.example /etc/quietfeed.env
sudoedit /etc/quietfeed.env
```

Set a long random `QUIETFEED_PASSWORD` in `/etc/quietfeed.env`. Then install and start the service:

```sh
sudo install -o root -g root -m 0644 quietfeed.service /etc/systemd/system/quietfeed.service
sudo systemctl daemon-reload
sudo systemctl enable --now quietfeed
```

The included `quietfeed.service` expects:

- binary: `/usr/local/bin/quietfeed`
- settings: `/etc/quietfeed.env`
- writable data directory: `/var/lib/quietfeed`
- service user: `quietfeed`; socket-access group: `caddy`

## Public HTTPS with Caddy

The included `Caddyfile` proxies directly to `/run/quietfeed/quietfeed.sock`. Replace `rss.example.com` with a hostname whose DNS record points to the server. Caddy obtains and renews its HTTPS certificate automatically.

Install the configuration and enable Caddy:

```sh
sudo install -o root -g root -m 0644 Caddyfile /etc/caddy/Caddyfile
sudo systemctl reload caddy
sudo systemctl enable --now caddy
```

In Reeder Classic, use:

- Server: `https://rss.example.com/api/greader.php`
- Username: the value of `QUIETFEED_USERNAME` (default `reader`)
- Password: the value of `QUIETFEED_PASSWORD`

Add feeds from Reeder's subscription controls. QuietFeed supports folders, unsubscribe/rename, unread counts, reading-list/feed/folder/starred streams, mark read/unread, starring, and mark-all-read. It limits each client to five failed login attempts per 15-minute window.

## Google Reader API compatibility

QuietFeed implements the subset needed by Reeder Classic:

- `ClientLogin`, token, and user information
- subscription list, quick-add, edit, and unsubscribe
- tag list, rename, and disable
- reading-list, feed, folder, starred, and read streams
- item IDs and item contents
- unread counts, edit-tag, and mark-all-as-read

It is not a complete Google Reader implementation. It is single-user, has no web interface, and does not implement sharing, recommendations, social features, preferences, or multi-user administration. Other Google Reader clients may work but are not currently tested.

To bulk-import an OPML file without starting another server process:

```sh
quietfeed -import-opml subscriptions.opml -db /var/lib/quietfeed/quietfeed.db
```

Remove a subscription by its exact feed URL:

```sh
quietfeed -remove-feed https://example.com/feed.xml -db /var/lib/quietfeed/quietfeed.db
```

## Settings

| Variable | Default | Meaning |
|---|---:|---|
| `QUIETFEED_USERNAME` | `reader` | Reeder login name |
| `QUIETFEED_PASSWORD` | required | Reeder login password |
| `QUIETFEED_SOCKET` | `/run/quietfeed/quietfeed.sock` | Unix socket path |
| `QUIETFEED_DB` | `quietfeed.db` | SQLite file |
| `QUIETFEED_REFRESH_INTERVAL` | `20m` | Feed refresh interval |
| `QUIETFEED_READ_RETENTION` | `2160h` | Retention period for read articles (90 days) |
| `QUIETFEED_FETCH_TIMEOUT` | `20s` | Per-feed request timeout |
| `QUIETFEED_INITIAL_ITEMS` | `20` | Maximum articles retained from a newly added feed's first fetch |
| `QUIETFEED_MAX_ITEMS` | `1000` | Maximum items parsed per refresh/API request |
| `QUIETFEED_MAX_FEED_BYTES` | `10485760` | Maximum decompressed feed response size (10 MiB) |
| `QUIETFEED_ALLOW_PRIVATE_FEEDS` | `false` | Permit feeds resolving to private or local network addresses |

Invalid explicitly configured durations, integers, or booleans stop startup instead of silently using defaults.

## Backups

Stop QuietFeed before copying `quietfeed.db`, or use SQLite's online backup facilities. The database contains subscriptions, article contents, reading state, and active login sessions, so protect backups as private data.

## Security

Run QuietFeed behind HTTPS and keep `/etc/quietfeed.env` readable only by root. See [SECURITY.md](SECURITY.md) for vulnerability reporting and the supported-version policy.

## Contributing

Bug reports and pull requests are welcome. See [CONTRIBUTING.md](CONTRIBUTING.md).

## License

QuietFeed is available under the [MIT License](LICENSE).
