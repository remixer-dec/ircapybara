# IRCapybara

IRCapybara is a small, opinionated single-server IRC implementation in Go. It uses strict regexp-first parsing, UTF-8 text with ASCII identifiers, predefined/operator-created channels, RAM-first runtime state, bounded RAM history, and SQLite only for durable configuration/identity/security data.

See `SPEC.md` for protocol behavior and acceptance criteria.

## License

The server is licensed under the [Business Source License 1.1](LICENSE). The Additional Use Grant permits non-commercial personal production use with fewer than 10,000 monthly active users. Enterprise production use requires a separate commercial license from the licensor while the Business Source License applies.

## Build

Direct Go dependencies are `github.com/glebarez/go-sqlite` for pure-Go SQLite, `github.com/kkdai/youtube/v2` for the optional YouTube plugin, and `github.com/u2takey/ffmpeg-go` as a pure-Go FFmpeg process wrapper. The server still builds with `CGO_ENABLED=0`; FFmpeg itself is an external static executable in Docker rather than a CGO library.

```sh
go mod download
go test ./...
CGO_ENABLED=0 go build -o ircapybara ./cmd/ircapybara
```

The project targets Go 1.27. Source archives do not vendor dependencies.

## Data directory

Outside Docker, defaults are relative to the working directory:

```text
./data/ircapybara.db
./data/tls/server.crt
./data/tls/server.key
```

Docker Compose overrides them to `/data/...` and mounts `/data` as a volume.

## Docker

Production:

```sh
docker compose up --build ircapybara
```

Debug/toolchain environment:

```sh
docker compose --profile debug up --build ircapybara-debug
```

The production Dockerfile is multi-stage. Its **runtime** image is `gcr.io/distroless/static-debian12:nonroot`; Go exists only in the build stage. A separate `mwader/static-ffmpeg:latest` stage contributes only `/ffmpeg` and `/ffprobe`. `Dockerfile.debug` uses `golang:1.27-bookworm` and receives the same static FFmpeg binaries. Compose mounts `/tmp` as tmpfs so YouTube-plugin working files stay ephemeral and RAM-backed in the normal container setup.

TLS is enabled by default. If the configured certificate/key do not exist and autogeneration is enabled, IRCapybara creates a self-signed RSA-2048 certificate for compatibility with older IRC TLS stacks. Configure ordinary IRC clients to use **TLS/SSL** on the configured port (Compose: `6697`) and disable certificate verification when using the generated self-signed certificate. The TLS listener accepts TLS 1.0 through TLS 1.3 and includes RSA/AES compatibility cipher suites for older IRC clients. Older IRCapybara-generated Ed25519 certificates are automatically replaced with RSA certificates when TLS autogeneration is enabled; administrator-supplied certificates are never overwritten. A plaintext IRC client pointed at the TLS port is rejected and the server logs the TLS handshake failure.

## Minimal client flow

```text
CAP LS 302
NICK alice
USER alice 0 * :Alice
CAP END
JOIN #general
PRIVMSG #general :hello
```

For compatibility with clients that omit the trailing-parameter colon when the real name is one word, `USER alice 0 * Alice` is also accepted. The colon remains required when the real name contains spaces. `CAP LS` suspends registration until `CAP END`, as expected by modern IRC clients. The server advertises `server-time` and the IRCapybara-specific `ircapybara/history` capability.

If captcha is enabled, registration produces a `NOTICE` such as:

```text
CAPTCHA REQUIRED deadbeef: solve 7+4; reply exactly: CAPTCHA deadbeef <answer>
```

Then:

```text
CAPTCHA deadbeef 11
```

Protect the current nickname:

```text
REGISTER a-long-password
```

Use it on a later connection. A registered nickname may connect before identification, but the connection remains identity-read-only until the password is accepted:

```text
NICK alice
USER alice 0 * :Alice
IDENTIFY a-long-password
```

The explicit form remains available:

```text
IDENTIFY alice a-long-password
```

While identity-read-only, the client can join/part and use read/query commands (`LIST`, `NAMES`, `WHO`, `WHOIS`, `TOPIC #channel`, `MODE #channel`, `MODE #channel b`, history/time/uptime/help), so it remains visible in rooms. Publishing messages and shared-state mutations are blocked until `IDENTIFY` succeeds.

## Channels

Channels are never implicitly created by `JOIN`. `JOIN` accepts up to 16 comma-separated channels (for example `JOIN #general,#ai,#rss-news`) and optional comma-separated keys. Operators add channels with:

```text
OPER admin change-me-now
CHANADD #ops :Operations
CHANADD #secret PRIVATE :Private room
CHANADD #scratch PRIVATE NOHISTORY :Temporary private room
```

`PRIVATE` channels are unlisted to unrelated users and invite-only by default. `LIST` still exposes them to IRC operators and to users who have participated. Durable prior-participant visibility is recorded only for authenticated registered identities; unregistered nickname participation is session-only.

Invite into a private channel:

```text
INVITE alice #secret
```

Channel policy modes:

```text
MODE #secret +s   # private/unlisted
MODE #secret -s   # public/listed
MODE #secret +H   # enable RAM history
MODE #secret -H   # disable and clear RAM history
```

## History

A history-enabled channel keeps at most 100 recent `PRIVMSG`/`NOTICE` messages in RAM. Nothing in message history is written to SQLite.

```text
HISTORY #general
HISTORY #general 100
```

The default replay limit is 20. A requester must currently be in the channel unless they are an IRC operator. History disappears on restart.

## Extended-mode attachments

IRCapybara has a deliberately small server-relayed attachment protocol for clients that enabled:

```text
XMODE EXTENDED
```

Uploads are text-framed and base64 encoded:

```text
XFER START #general 3 text/plain :a.txt
# server: XFER READY <nick> <fileid> 240 1
XFER CHUNK <fileid> 0 YWJj
XFER END <fileid> ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad
```

Recipients that were present in that conversation when the attachment completed receive `XATTACH` metadata containing the file ID, filename, MIME type, and size. An extended client can then request:

```text
RECV <fileid>
```

The server replays `XFER START`, ordered `XFER CHUNK`, and `XFER END` lines. Transfer chunks remain fixed at 240 raw bytes (except the final chunk), even though ordinary IRCapybara text messages now support up to 32,768 Unicode characters. Files are held only in RAM, at most four transfers occupy the window, default maximum file size is 15 MiB, completed files become eligible for removal one hour after the first successful download, incomplete uploads expire after ten minutes, and never-downloaded completed files expire after 24 hours as a safety bound. Attachments are not placed in channel history.

Non-extended clients receive a plain text fallback telling them an attachment arrived but cannot issue `RECV`. Channel attachment ACLs are snapshotted at completion: joining a channel later does not grant access to an older attachment merely by learning its file ID.

## Basic server queries and help

```text
TIME
UPTIME
HELP
HELP JOIN
HELP XFER
HELP AI
HELP YTDL
```

`HELP` lists the currently available core/plugin help modules. `HELP <module>` returns short syntax rules and examples. Many IRC clients expose `TIME` as `/time` and custom commands through `/quote`, for example `/quote HELP XFER`.

IRCapybara accepts ordinary `PRIVMSG`/`NOTICE` text up to **32,768 Unicode characters**. This is intentionally larger than historical IRC line limits for IRCapybara-aware clients; identifiers and command-specific metadata remain tightly bounded.

## Guard mode

```text
GUARD 0
GUARD 1
GUARD 2
GUARD 3
```

- level 1: snapshotted IP **or** username may reconnect;
- level 2: only snapshotted IPs may reconnect;
- level 3: first removes recent untrusted sessions, then snapshots surviving IPs like level 2 and puts the server into read-only mode.

When entering level 3, a connection younger than 30 minutes is removed unless it is an IRC operator or it is authenticated to a registered identity at least 30 minutes old. Therefore old sessions remain, and established registered users can survive a recent reconnect. Security/operator controls remain usable so an operator can disable guard mode.

At guard level 0, unsupported or malformed command envelopes that remain valid UTF-8 and inside the hard size ceiling are ignored rather than disconnecting the client. They still consume the normal flood budget. Oversized and invalid-UTF-8 input remain hard protocol violations.

## Ban administration

```text
BANADD SERVER IP 192.0.2.10
BANADD SERVER CIDR 198.51.100.0/24
BANADD #general USER noisyuser
BANLIST SERVER
```

Channel bans can also use:

```text
MODE #general +b user:noisyuser
MODE #general +b ip:192.0.2.10
MODE #general +b cidr:198.51.100.0/24
```

## Configuration

Environment variables include:

- `IRCAPYBARA_LISTEN_ADDR` — default `:6697` outside Compose;
- `IRCAPYBARA_SERVER_NAME` — default `ircapybara.local`;
- `IRCAPYBARA_DB_PATH` — default `./data/ircapybara.db`;
- `IRCAPYBARA_TLS_ENABLED` — `true|false`, default `true`;
- `IRCAPYBARA_TLS_AUTOGEN` — `true|false`, default `true`;
- `IRCAPYBARA_TLS_CERT_FILE` — default `./data/tls/server.crt`;
- `IRCAPYBARA_TLS_KEY_FILE` — default `./data/tls/server.key`;
- `IRCAPYBARA_SERVER_PASSWORD` — optional shared IRC `PASS` secret;
- `IRCAPYBARA_OPER_NAME` / `IRCAPYBARA_OPER_PASSWORD`;
- `IRCAPYBARA_CAPTCHA_ENABLED` — default `false`;
- `IRCAPYBARA_IDENTIFY_TRUST_IP` — default `false`; when enabled, a successful `REGISTER`/`IDENTIFY` remembers the `(IP, identity)` pair in RAM and automatically restores that same identity from that IP until restart;
- `IRCAPYBARA_INITIAL_CHANNELS` — e.g. `#general|General discussion;#dev|Development`; seeded channels are public/history-enabled;
- `IRCAPYBARA_GUARD_LEVEL` — `0`, `1`, `2`, or `3`;
- `IRCAPYBARA_GUARD_ALLOWED_IPS` / `IRCAPYBARA_GUARD_ALLOWED_USERS` — startup guard allowlists;
- `IRCAPYBARA_MAX_CLIENTS`, `IRCAPYBARA_MAX_PER_IP`;
- `IRCAPYBARA_RATE_PER_SECOND`, `IRCAPYBARA_RATE_BURST`;
- `IRCAPYBARA_TRANSFER_MAX_MB` — maximum attachment size, default `15`, accepted range `1..64`; transfer window remains fixed at four files;
- `IRCAPYBARA_MIRROR_SOURCES` — optional MirrorPlugin source list;
- `IRCAPYBARA_BRIDGE_TG_BOT_TOKEN` / `IRCAPYBARA_BRIDGE_TG_MODE` — optional Telegram BridgePlugin;
- `IRCAPYBARA_CHAT_ENABLED`, `IRCAPYBARA_CHAT_ENDPOINT`, `IRCAPYBARA_CHAT_API_KEY`, `IRCAPYBARA_CHAT_MODEL`, `IRCAPYBARA_CHAT_REASONING_EFFORT`, `IRCAPYBARA_CHAT_CHANNEL`, `IRCAPYBARA_CHAT_PRIVATE`, `IRCAPYBARA_CHAT_HISTORY_ENABLED`, `IRCAPYBARA_CHAT_NICK`, `IRCAPYBARA_CHAT_SYSTEM_PROMPT` — optional ChatCompletions plugin;
- `IRCAPYBARA_YTDL_ENABLED`, `IRCAPYBARA_YTDL_MODE`, `IRCAPYBARA_YTDL_CHANNEL`, `IRCAPYBARA_YTDL_PRIVATE`, `IRCAPYBARA_YTDL_NICK`, `IRCAPYBARA_YTDL_FFMPEG_PATH`, `IRCAPYBARA_YTDL_FFMPEG_PRESET`, `IRCAPYBARA_YTDL_FILENAME_TEMPLATE` — optional YouTubeFFmpeg plugin.

Configuration strings are bounded and validated. Database/TLS paths must be safe absolute paths or safe `./relative` paths; parent traversal is rejected.

## Architecture

```text
cmd/ircapybara        process entry point
internal/config       strict environment configuration
internal/protocol     command regexps and UTF-8 policy
internal/model        in-memory state structures
internal/server       TCP/TLS loop, IRC state, handlers and policy
internal/protection   bans + guard state
internal/security     password KDF/verification
internal/storage      direct database/sql SQLite persistence
pkg/pluginapi         public compile-time hook interface
internal/plugins      compile-time plugin registry
```

SQLite is loaded at startup. Ordinary messages/presence never write to disk. Durable writes are restricted to identity registration, channel/topic/policy changes, first durable private-channel participation, and ban changes.

## Plugin model

Plugins are linked at compile time and configured through environment variables. They use a narrow `pluginapi.Host` surface rather than direct socket/database access. Plugin-created channels are RAM-only; plugins cannot silently persist arbitrary state in SQLite. Live plugin messages also respect guard level 3 read-only mode. The host additionally exposes a bounded server-side attachment send primitive so a plugin can publish a file without impersonating a client-side upload.

### MirrorPlugin

`MirrorPlugin` supports two sources: RSS/Atom over HTTPS and Telegram public-channel web previews (`https://t.me/s/<channel>`). Sources are configured in one semicolon-separated variable:

```sh
IRCAPYBARA_MIRROR_SOURCES='rss|#rss-news|https://example.com/feed.xml|1h;tg|#rss-telegram|telegram|24h'
```

Each entry is:

```text
rss|#channel|https://feed-url|<1h-or-24h>
```

or, concretely, one interval token at the end:

```text
rss|#rss-news|https://example.com/feed.xml|1h
tg|#rss-telegram|telegram|24h
```

The actual parser accepts exactly `kind|#channel|source|1h` or `kind|#channel|source|24h`; entries are separated by `;`. Mirror channels are forced history-free in the IRC core, so normal discussion remains possible but discussion messages are not retained. The plugin itself keeps at most 100 latest source posts in RAM.

On initial fetch the existing source backlog is learned but not broadcast as new. Later unseen IDs/hashes are broadcast. On `JOIN`, the latest source posts are sent only to that joining client. A client that negotiated:

```text
CAP REQ :server-time ircapybara/history
```

gets tagged synthetic history (`ircapybara-history=1`, plus `time=` when `server-time` was requested). A legacy client gets ordinary `PRIVMSG` replay only to that client.

RSS URLs must be HTTPS and the fetcher rejects private/loopback/link-local destinations to avoid turning feed configuration into an SSRF path. When RSS `pubDate`/Atom time parses successfully, posts are prefixed with compact `[MM-DD HH:MM]` time. Telegram preview timestamps receive the same prefix when available. Telegram mirror configuration accepts only the public channel username, constructs the `/s/` URL itself, and uses that channel username as the IRC sender nick when representable.

### BridgePlugin / Telegram

Enable with:

```sh
IRCAPYBARA_BRIDGE_TG_BOT_TOKEN='123456789:...'
IRCAPYBARA_BRIDGE_TG_MODE='channel'   # or dm
```

Bridge ownership requires an authenticated registered IRC identity. Create a binding with the raw IRC command:

```text
BRIDGE tg:<chat_id> <name>
```

In clients that do not define `/bridge` themselves, use the client's raw-command form, commonly:

```text
/quote BRIDGE tg:-1001234567890 family
```

`channel` mode creates a RAM-only private invite-only channel such as `#tg-family`, automatically joins the owning identity, and suppresses IRC history. `dm` mode exposes a virtual sender/target named `tg-family`. One Telegram `chat_id` can belong to only one IRC identity, and bridge mappings are intentionally RAM-only in this iteration.

The bot uses Telegram Bot API long polling. Outgoing IRC messages go through one bounded queue; incoming Telegram messages are injected directly through the host API rather than re-entering the outgoing IRC hook, preventing bridge loops. Text is mirrored directly. Images, video, stickers, GIF/animation, and voice are represented as `[image]`, `[video]`, `[sticker]`, `[gif]`, and `[voice]` for now.

For an inbound Telegram dialog with no mapping: one eligible IRC identity is selected automatically; with 2–9 connected users the bot sends inline buttons for currently authenticated identities; with 10 or more connected users (and any other ambiguous/no-user case) it says it is waiting for IRC-side dialog initiation. Callback selection claims the chat for exactly one identity. At most 100 unmatched latest messages are retained in RAM while waiting.

### YouTubeFFmpegPlugin

Enable with:

```sh
IRCAPYBARA_YTDL_ENABLED=true
IRCAPYBARA_YTDL_MODE=channel       # channel | dm | both
IRCAPYBARA_YTDL_CHANNEL=#yt-dl
IRCAPYBARA_YTDL_PRIVATE=true
IRCAPYBARA_YTDL_NICK=yt-dl
IRCAPYBARA_YTDL_FFMPEG_PATH=ffmpeg
IRCAPYBARA_YTDL_FFMPEG_PRESET='-c:v copy -c:a copy -movflags +faststart -f mp4'
IRCAPYBARA_YTDL_FILENAME_TEMPLATE='{title}-{id}.{ext}'
```

In channel mode, send one canonical YouTube URL to `#yt-dl`; in DM mode, send it to the virtual target `yt-dl`. The request is intercepted and progress is sent only to the requester. The client must be in `XMODE EXTENDED` because the result is delivered through the attachment protocol.

There is exactly one worker. Other requests enter an eight-item bounded queue. A non-operator may have only one active-or-queued job; operators may queue more, still subject to the global queue. The plugin selects a combined MP4 stream below the server attachment budget, bounds the download, runs FFmpeg with a four-minute timeout, a local-file/pipe protocol whitelist, and an output-size limit, reads the final file only after it is known to fit, then publishes it using the server-side attachment API. Docker uses `/tmp` tmpfs for the intermediate input/output files.

The FFmpeg preset is trusted administrator configuration, not IRC-user input. It accepts ordinary FFmpeg argv such as `-vn -c:a libopus -b:a 6k -ac 1 -ar 16000 -application voip -frame_duration 60 -compression_level 10`; quoting/backslash escaping is tokenized in Go and **no shell is invoked**. The old unrestricted `key=value;...` form is accepted for backward compatibility. IRCapybara still owns the managed input/output path, timeout, and final `-fs` output-size ceiling. The filename template supports only `{title}`, `{id}`, and `{ext}`; user-derived values are sanitized and bounded in Go.

The plugin currently accepts text URLs only, downloads video content only when a compatible stream fits the configured server limit, and is intended for media the operator/user is authorized to retrieve.

### ChatCompletions

Enable an OpenAI-Chat-Completions-compatible endpoint with:

```sh
IRCAPYBARA_CHAT_ENABLED=true
IRCAPYBARA_CHAT_ENDPOINT='https://api.openai.com/v1/chat/completions'
IRCAPYBARA_CHAT_API_KEY='...'
IRCAPYBARA_CHAT_MODEL='your-model'
IRCAPYBARA_CHAT_REASONING_EFFORT='low'
IRCAPYBARA_CHAT_CHANNEL='#ai'
IRCAPYBARA_CHAT_PRIVATE=false
IRCAPYBARA_CHAT_HISTORY_ENABLED=true
IRCAPYBARA_CHAT_NICK='chat-ai'
IRCAPYBARA_CHAT_SYSTEM_PROMPT='You are a concise IRC assistant.'
```

The endpoint receives only `model`, `messages`, and optional `reasoning_effort`; no tools/functions are supplied. The API key may be empty for a compatible endpoint that does not require bearer auth. `IRCAPYBARA_CHAT_PRIVATE=true` makes the channel unlisted, but it remains joinable by users who know its name.

All conversations are private per user even when several people are members of the same `#ai` channel: user prompts are intercepted before normal IRC broadcast/history and model replies are delivered only to the requester. With `IRCAPYBARA_CHAT_HISTORY_ENABLED=true` (default), authenticated identities keep a private RAM context/history across reconnects for the lifetime of the server process; unregistered sessions are discarded at disconnect. The newest **20 user+assistant messages** are retained and replayed only to that user on join (synthetic history for capable clients, ordinary private-to-that-client messages otherwise). With the env flag set to `false`, requests are stateless. One request may be in flight per user and four globally.

Reset only your own AI context/history with `AIRESET` (commonly `/quote AIRESET`), or by sending the exact message `!reset`/`/reset` to the AI channel. If a request is already in flight, reset invalidates it and its eventual answer is discarded. The synthetic responder nick is derived from the Chat Completions response `model` field after conversion to an IRC-safe nickname; `IRCAPYBARA_CHAT_NICK` is now the fallback when the response omits or returns an unusable model name.

To add another compile-time plugin, implement `pluginapi.Plugin` and register it in `internal/plugins.Builtins()`. Plugins may optionally implement `pluginapi.CommandHandler` and/or `pluginapi.HelpProvider`.

## Prototype limitations

This is still a prototype rather than an audited production IRC daemon. It intentionally has no SASL/services, server network, persistent chat history, CTCP/DCC file transfer, IRCv3 history protocol, dynamic plugins, DNS/host cloaking, or autonomous idle-PING scheduler.
