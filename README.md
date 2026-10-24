# 📼 tape-drive

[![CI](https://github.com/ruivieira/tape-drive/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/ruivieira/tape-drive/actions/workflows/ci.yml)

A tiny BYOS ("build your own server") for TRMNL e-ink devices. Point it at a
directory of Go HTML templates and a directory of JSON files, and it serves the
firmware API, screenshots each page with headless Chrome and hands the device a
1-bit PNG.

Nothing about the content is compiled in. Templates and data are read from disk
on every render, the JSON is handed to the template untouched, and the playlist
is just a manifest of template/data pairs.

```
templates/*.tmpl ──┐
                   ├─► html/template ► headless Chrome ► dither ► 1-bit PNG
data/*.json ───────┘                                              │
                                                                  ▼
                                       GET /api/display ──► one page per wake
```

## Install

```bash
make install                 # build + install to ~/.local/bin/tape-drive
make install PREFIX=/usr/local
```

Needs Go 1.25+ to build, and a `google-chrome` / `chromium` binary on `PATH` to
render (or point `-chrome` at one).

## Run

```bash
tape-drive \
  -templates ./templates \
  -data ~/.local/share/screens \
  -pages ./pages.json \
  -addr 0.0.0.0:2300 \
  -base-url http://192.0.2.10:2300 \
  -interval 120 -render-every 120
```

`-base-url` must be the URL the *device* uses — it is what gets embedded in the
`image_url` the firmware downloads. Without it, `http://localhost:<port>` is
assumed.

| Flag | Default | Meaning |
| --- | --- | --- |
| `-addr` | `0.0.0.0:2300` | listen address |
| `-base-url` | `http://localhost:<port>` | public URL for `image_url` |
| `-api-key` | empty | expected `Access-Token`; empty accepts any device |
| `-interval` | `900` | seconds between page changes, sent as `refresh_rate` |
| `-templates` | `templates` | root for template paths |
| `-data` | `data` | root for data file paths |
| `-pages` | `pages.json` | playlist manifest |
| `-chrome` | auto-detect | Chrome/Chromium binary |
| `-width` / `-height` | `800` / `480` | panel size, i.e. the screenshot viewport |
| `-render-every` | `0` | re-render at least every N seconds even when nothing changed (for clocks); `0` = only on change |
| `-preview` | | render every page to a PNG and exit, without serving (`-render` is an alias) |
| `-script` | | shell script to run periodically, e.g. one that refreshes the JSON files |
| `-script-interval` | `5m` | how often `-script` runs, independent of `-interval` |
| `-version` | | print version |

`-render out.png` is the quick way to check a layout without a device; with more
than one page it writes `out-<name>.png` per page.

## Playlist manifest

```json
{
  "pages": [
    { "name": "clock" },
    { "name": "prs-1", "template": "pull-requests.html.tmpl", "data": "prs-1.json" },
    { "name": "prs-2", "template": "pull-requests.html.tmpl", "data": "prs-2.json" }
  ]
}
```

- `name` is the page's filename prefix (`<name>-<hash>-<unixtime>`, 31 characters
  max) and what the preview page lists.
- `template` and `data` are resolved against `-templates` and `-data`. Both
  default to the page name (`<name>.html.tmpl`, `<name>.json`), so a page with
  only a name needs no paths. Absolute paths and `~/` work too.
- Several pages may share one template and differ only in their data file.
- A bare `[...]` array is accepted instead of `{"pages": [...]}`.

Pages are served in manifest order, one per device wake, wrapping around.
`/api/display/current` repeats the last page served without advancing.

## Templates

A template is plain Go `html/template`. The context:

| Field | Meaning |
| --- | --- |
| `.Data` | the JSON file, decoded to `map[string]any` / `[]any`; keys keep their JSON spelling. Whole numbers become `int64`, so `{{ if gt .Data.count 0 }}` compares like an integer |
| `.Page.Name` | page name from the manifest |
| `.Page.Index` / `.Page.Total` | 1-based position, for a "page 2 of 3" footer |
| `.Now` | render time (`time.Time`) |

`{{ plus a b }}` is available for small counters.

Keep everything inside `-width`x`-height`: the screenshot *is* the viewport, so
whatever falls past the fold is silently lost. Use `white-space: nowrap` plus
`text-overflow: ellipsis` on rows, or one long title will push a list off the
panel.

`.Now` is the render time, but renders are cached — a clock would freeze at the
first screenshot. Pass `-render-every <seconds>` (usually the same value as
`-interval`) to re-render once per bucket; the page then updates at most that
often, and the device re-downloads at most once per bucket.

A data file that does not exist yet renders `{}` instead of failing the wake, so
a fresh checkout shows an empty state rather than a broken screen.

## How rendering works

Each page caches its last PNG keyed by a hash of template + data + panel size.
When the inputs are unchanged the cached PNG is served as-is; when they change,
the page is screenshotted again and re-encoded. The result is a 1-bit grayscale
PNG (color type 0, built by hand — Go's encoder only emits 8-bit) after
Floyd–Steinberg dithering, which is what e-ink firmware expects and roughly a
quarter of the size.

The filename is `<page>-<hash6>-<unixtime>`: same inputs, same hash; a new
timestamp only when the bytes change. That is the device's cache key, and the
firmware drops any name longer than 31 characters.

## Refreshing data on a timer

`-script` runs a shell script on tape-drive's own timer, independent of the page
interval. It runs once at startup and then every `-script-interval`:

```bash
tape-drive -templates ./templates -data ~/.local/share/screens -pages ./pages.json \
  -script ./refresh.sh -script-interval 5m -interval 2m
```

The device flips pages every 2 minutes while the script refreshes the JSON every
5: the next render picks up whatever the script wrote, because the cache keys on
file contents rather than on time.

- Runs never overlap — a script slower than the interval delays the next tick
  instead of stacking up.
- A failing script is logged with its output and the loop keeps going, so a
  flaky network or a missing command never takes the server down.
- With `-preview`, the script runs once first, so the exported PNGs show fresh
  data.

## Endpoints

| Endpoint | Purpose |
| --- | --- |
| `GET /api/setup` | provisioning: `api_key`, `friendly_id`, `image_url` |
| `GET /api/display` | next page in the playlist + `refresh_rate` |
| `GET /api/display/current` | same payload for the last page served, no advance |
| `GET /screens/{filename}` | the 1-bit PNG |
| `POST /api/log` | device logs, answered with `204` |
| `GET /up` | health check |
| `GET /` | human preview: every page, the data behind it, device reports |

Firmware details worth knowing:

- With `-api-key` unset the server is open on the LAN, but setup still returns a
  stable per-device key (`local-<last 6 of the MAC>`) so the firmware has
  something to store and send back.
- A wrong `Access-Token` gets `202 {"status":202,"message":"Device is not
  registered yet."}` — never `reset_firmware`, which makes the firmware wipe its
  WiFi credentials and API key.
- `refresh_rate` must be a number: a string parses as 0 and the device wakes in a
  loop until the battery dies.
- The image is served as `image/png`; the firmware picks its decoder from
  `Content-Type`.

## Development

```bash
make test     # go test ./... (race, no network, no Chrome)
make vet
make fmt
```

CI runs `gofmt`, `go vet`, `go build` and `go test -race` on every push and pull
request to `main`. The rendering path needs a browser, so it is exercised with
`-render` rather than in unit tests — the tests cover the JSON decoding, the
manifest loader, the cache key and the filename rules.

## License

AGPL-3.0 — see [LICENSE](LICENSE).
