# s3tui

> [!NOTE]
> This project was build with AI.

A terminal UI for S3 to scratch my own itch. 

## Install

```sh
go install github.com/sj14/s3tui@latest
```

## Usage

```sh
s3tui                       # asks which profile to use
s3tui -config ./my.toml     # a specific config file
s3tui -version
```

## Configuration

TOML, looked up as `-config <path>` → `$S3TUI_CONFIG` (or `$SSS_CONFIG`) →
`~/.config/s3tui/config.toml` → `~/.config/sss/config.toml`.

```toml
[profiles.earth]
endpoint = "https://earth.example.com"
region = "earth"
access_key = "<CHANGE_ME>"
secret_key = "<CHANGE_ME>"

[profiles.mars]
endpoint = "https://mars.example.com"
region = "mars"
path_style = true
read_only = true
bandwidth = "128 MiB"
```

| Field        | Description                                                       |
| ------------ | ----------------------------------------------------------------- |
| `endpoint`   | S3 endpoint URL. Empty for AWS.                                   |
| `region`     | S3 region.                                                        |
| `access_key` | Access key. Empty falls back to the AWS credential chain.         |
| `secret_key` | Secret key.                                                       |
| `path_style` | Use path-style requests (`host/bucket` instead of `bucket.host`). |
| `insecure`   | Skip the TLS verification.                                        |
| `read_only`  | Block every non-read HTTP method.                                 |
| `sni`        | Override the TLS server name.                                     |
| `network`    | Force IPv4/6 with `tcp4` or `tcp6`.                               |
| `bandwidth`  | Limit the bandwidth per second, e.g. `128 MiB`.                   |

s3tui starts in the profile list; `enter` connects, and `esc` on the bucket
list comes back to switch. A config without any profile connects right away, on
the AWS defaults.

Every field has an environment variable (`S3TUI_ENDPOINT`, `S3TUI_REGION`, …,
with `SSS_` as a fallback). Without keys the usual AWS sources are used
(`AWS_*`, `~/.aws/credentials`, SSO, IAM roles). On AWS, buckets outside the
configured region are reached through a region specific client.

## Keys

| Key                | Action                                                                     |
| ------------------ | -------------------------------------------------------------------------- |
| `↑`/`k` `↓`/`j`    | Move the cursor                                                            |
| `pgup`/`pgdn`      | Page; `g`/`G` jump to top/bottom on a document page                        |
| `enter`            | Open the selection                                                         |
| `esc`, `backspace` | Back one level                                                             |
| `u`                | Upload: opens a local file browser for the current prefix                  |
| `l`                | Load, i.e. download the selected object, prefix or version                 |
| `c` / `m`          | Server-side copy / move of the selection                                   |
| `d`                | Delete the selection (object, prefix, version, multipart upload, document) |
| `D`                | Delete **every** version of the selected object or prefix                  |
| `t`                | The transfers of this session                                              |
| `x`                | Transfer view: cancel the selected transfer, or clear a failed row         |
| `e`                | Document page: edit it in `$EDITOR`                                        |
| `/`                | Object view: search on the server. Elsewhere: filter the loaded list       |
| `?`                | Full help, which is also where `ctrl+c` is listed                          |
| `ctrl+c`           | Quit; while a transfer runs it takes a second press within a second        |

## Contributing

`AGENTS.md` describes the layout, the conventions, how the tests fake an S3
endpoint, and what is deliberately still open.
