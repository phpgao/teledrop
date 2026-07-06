# teledrop

A Telegram bot that downloads every file sent to it, organizes them locally by rule, and optionally uploads to an S3-compatible object store (Tencent COS / MinIO / AWS S3 / ...). Uploading is abstracted behind an `Uploader` interface for easy extension, and an `overwrite` flag controls whether existing remote objects are replaced.

Captions (the text attached to a file) and plain-text messages are saved as `.txt` files next to the binaries, and are uploaded too when upload is enabled.

## Features

- Auto receives and downloads: document / photo (largest picked) / video / audio / voice / video note / animation / sticker
- Local organization rules, switchable: `flat` `by_date` `by_type` `by_chat` `by_chat_date` (default `by_chat_date`)
- Forward isolation: with `separate_forwards`, forwarded files go into a `forwarded/` segment, separate from direct sends
- Text is preserved: a file's caption is saved as a sidecar `.txt`; a text-only message is saved as `text_<id>.txt` with sender/time metadata
- User whitelist: `allowed_users` is non-empty => only those users; empty => everyone
- Pluggable upload backends: `s3` / `local` / `none`, off by default
- Overwrite flag: when `false`, an existing remote object is skipped (returns `ErrExists`)
- Bounded concurrency (default 4); one file failing never blocks the others; errors are surfaced, not swallowed
- **Two-phase reply notification**: a start message (`📥 processing N files...`) and a completion summary (`✅ X/Y success` with per-file size, duration, upload status), both quoting the original message
- **Large file support**: files >20 MB are downloaded with a dedicated HTTP client (30-minute timeout) to handle slow/capped connections gracefully
- **SQLite tracking**: all download attempts (success / fail / skip) are recorded in a local SQLite database at `<base_dir>/.teledrop.db` for structured querying and auditing
- **S3 resilience**: endpoint auto-prefixes `https://`, optional startup health check via `health_check`, and S3 init failures never crash the bot (graceful fallback to no-upload mode)

## Layout (single package, flat)

```
main.go        wiring + flags + signal handling
config.go      config load + ${ENV} expansion + validation
organizer.go   compute local relative dir and remote key from a message
downloader.go  extract every file from a message + download / save text locally
uploader.go    Uploader interface + factory
s3.go          S3/COS uploader
local.go       local mirror uploader
noop.go        disabled-state placeholder
store.go       SQLite persistence layer (processed / seen / failures / downloads)
state.go       dedup + failure-queue + download records (backed by store)
bot.go         poll updates, whitelist check, dispatch handling
```

## Create a Telegram bot (via @BotFather)

teledrop needs a bot token. Create one for free inside Telegram:

1. Open Telegram and search for **@BotFather** (the official bot-management account, marked with a blue verification check).
2. Open the chat and send `/start` to show the menu.
3. Send `/newbot`. BotFather will ask for a display name.
4. Reply with a human-friendly **name** for your bot, e.g. `My File Drop`.
5. Reply with a unique **username** that MUST end with `bot` (case-insensitive), e.g. `my_file_drop_bot`. If it is taken, choose another.
6. BotFather replies with a message containing the HTTP API token, shaped like:
   ```
   123456789:AAEhBOwe...longstring...
   ```
   Copy this token — it is the only credential teledrop needs to receive and download your files.
7. (Recommended) Personalize the bot:
   - `/setdescription` / `/setabouttext` — profile text.
   - `/setuserpic` — avatar.
   - `/setcommands` — register the `start` command so it appears as a suggestion button:
     ```
     start - check that the bot is alive
     ```
8. (Optional) If you want to use the `allowed_users` whitelist, find your own Telegram user ID by talking to **@userinfobot** or **@RawDataBot** and copying the `id` it returns, then add it under `telegram.allowed_users`.

Keep the token secret: anyone who holds it can control your bot. In teledrop it is supplied through the `TELEGRAM_TOKEN` environment variable (see the `${TELEGRAM_TOKEN}` placeholder in `config.yaml`), and is never committed to the repository.

## Receiving updates: long polling

teledrop uses **long polling** (`getUpdates`): it keeps a connection open to Telegram and pulls new
messages as they arrive. There is no callback URL or reverse proxy to configure — just run it.

## Deduplication, retry and download records

teledrop keeps state in a local SQLite database at `<base_dir>/.teledrop.db` so it survives restarts (previously a JSON file; automatically migrated on first run):

- **Processed messages** (`chat_id:message_id`) — a redelivered update right after a restart is skipped, so you never get duplicate downloads from Telegram re-pushing.
- **Seen files** (Telegram `FileUniqueID`) — the same file sent twice is downloaded only once.
- **Failed downloads** — if a download (or text/caption write) fails, it is retried up to 3 times with exponential backoff, then queued. Because a Telegram update is one-shot once acknowledged, the failed item is retried via its permanent `file_id`, not by re-pulling the message.
- **Download records** — every download attempt (success, failure, or skip) is recorded in the `downloads` table with chat ID, sender, file name, size, status, duration, upload status, and timestamp. You can query the database directly, e.g.:
  ```sql
  sqlite3 downloads/.teledrop.db "SELECT name,size,status,datetime(created_at,'unixepoch') FROM downloads ORDER BY created_at DESC LIMIT 20;"
  ```

Send **`/retry`** to replay the failed-download queue on demand. teledrop also replays the queue
automatically at startup (silently). A message is only marked "processed" after *every* part of it
succeeded, so a partial failure stays in the queue and is retried later.

## Large file download (MTProto)

Telegram Bot API refuses `getFile` for files >20 MB with `Bad Request: file is too big`.
teledrop can log into MTProto **as the bot itself** using the same bot token — no separate
phone number, auth code, or 2FA required.

**Setup:**

1. Get `app_id` and `app_hash` at https://my.telegram.org/apps (free, 30 seconds)
2. Fill `download.mtproto` in `config.yaml`:
   ```yaml
   telegram:
     mtproto:
       app_id: 12345
       app_hash: "abc..."
       socks5: "127.0.0.1:1086"
   ```
3. Run the bot — it logs in with the same `telegram.token`, no interactive steps

**How it works:** MTProto shares the bot's identity, so message IDs match Bot API
identically. No peer mapping or content searching needed — just use the message ID
to locate and download the file.

Without MTProto config (`app_id=0`), large files fail the same as before.

## Quick start

```bash
# 1. prepare config
cp config.yaml.example config.yaml   # edit as needed
export TELEGRAM_TOKEN="your bot token"   # from @BotFather
# if enabling s3 upload:
export COS_ACCESS_KEY="..."
export COS_SECRET_KEY="..."

# 2. run
go run . -config config.yaml
```

Send `/start` to the bot to confirm it is alive, then send any file.

## Organization example (default by_chat_date + separate_forwards)

```
downloads/
├── alice/                    # chat dir (username preferred, else title / chat id)
│   └── 2026/07/06/
│       ├── photo_xxx.jpg
│       ├── photo_xxx.txt      # caption sidecar (if a caption was sent)
│       └── doc_xxx.pdf
└── forwarded/                # forwarded-file isolation segment
    └── some_channel/
        └── 2026/07/06/
            └── video_yyy.mp4
```

The remote key mirrors the local relative path, using `/` as separator, so the structure is replicated in COS.

## Extending with a new upload backend

Implement the `Uploader` interface and add a branch in `NewUploader`:

```go
type Uploader interface {
    Upload(ctx context.Context, src, key string, overwrite bool) error
}
```

## Config reference

| Field | Meaning |
|-------|---------|
| `telegram.token` | bot token (from @BotFather), supports `${ENV}` |
| `telegram.allowed_users` | whitelist of user IDs; empty = unrestricted |
| `download.base_dir` | local download root |
| `download.organize` | organization rule |
| `download.separate_forwards` | isolate forwarded files into `forwarded/` |
| `telegram.mtproto.app_id` | Telegram API ID for large file download (optional, 0=off) |
| `telegram.mtproto.app_hash` | Telegram API hash |
| `telegram.mtproto.socks5` | SOCKS5 proxy address for MTProto connections (e.g. `127.0.0.1:1086`) |
| `download.mtproto.phone` | phone number for MTProto auth |
| `upload.enabled` | whether to upload (default false) |
| `upload.overwrite` | overwrite remote object when it already exists |
| `upload.type` | `s3` / `local` / `none` |
| `upload.s3.*` | S3-compatible config; fill `endpoint` for COS/MinIO. `health_check` (bool, optional) pings the bucket at startup |
| `upload.local.mirror_dir` | mirror directory when `type=local` |
