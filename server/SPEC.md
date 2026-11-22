# IRCapybara v0.4 prototype specification

## 1. Scope

IRCapybara is a small, single-server IRC service built for a controlled use case. It is not intended to be a general-purpose IRC daemon or an IRC network implementation. The design prioritizes deterministic behavior, strict input validation, a small attack surface, ordinary IRC-client compatibility on the common path, low storage write volume, and explicit room for IRCapybara-specific extensions later.

Out of scope for this iteration: server linking, SASL, NickServ/ChanServ-style services, account federation, IRC `+v` voice status, voice calls, WebSockets, metrics, structured logging, HTTP APIs, arbitrary implicit channel creation, CTCP interpretation, E2EE, dynamic shared-object plugins, and persistent chat-history/file storage. A bounded IRCapybara-specific RAM attachment transport is in scope; classic CTCP/DCC is still not implemented.

## 2. Design rules

1. **Single server.** No server-to-server protocol or distributed state.
2. **Strict regexp input boundary.** Every client line must be valid UTF-8, stay within the defensive transport/rune ceilings, match the command-envelope regexp, and then match a command-specific regexp. Ordinary `PRIVMSG`/`NOTICE` text is capped at 32,768 Unicode code points. Free text is additionally checked against an allowlist regexp that rejects IRC/terminal control characters. In guard level 0 only, a line that is valid UTF-8 and within hard size bounds but does not match the supported command envelope is ignored (while still consuming flood budget) rather than disconnecting the client; higher guard levels treat that as a protocol violation.
3. **ASCII identifiers, UTF-8 text.** Nicknames, usernames, channel names, modes, passwords, keys, command tokens, IP/CIDR values, and similar protocol identifiers use bounded ASCII forms. Messages/topics/reasons/realnames accept printable UTF-8. ASCII is therefore supported as a UTF-8 subset. No other text encoding is supported.
4. **No implicit channel creation.** An unknown `JOIN` never creates a channel. Channels are seeded from configuration or created by an IRC operator using `CHANADD`.
5. **RAM-first state.** Connections, memberships, invites, rate-limit state, captcha state, guard state, mode state, and message history live in memory.
6. **On-demand persistence only.** SQLite writes occur only for durable identity registration, channel catalog/policy/topic changes, durable private-channel participation, and bans. Normal chat does not write to SQLite.
7. **CGO-free Go build.** SQLite uses `github.com/glebarez/go-sqlite` directly through `database/sql`. Optional media support uses `github.com/kkdai/youtube/v2` and the pure-Go process wrapper `github.com/u2takey/ffmpeg-go`; no Go package links libav/FFmpeg through CGO. FFmpeg is an external static executable in container builds.
8. **TLS-first.** TLS is enabled by default. The server may generate and use a self-signed RSA-2048 certificate for legacy IRC-client compatibility. Client certificates are not required or verified. The TLS endpoint accepts TLS 1.0 through TLS 1.3; TLS 1.0/1.1 and RSA key-exchange suites exist strictly for legacy IRC-client compatibility.
9. **Compile-time Go plugins.** Plugins implement `pkg/pluginapi.Plugin` and are linked into the binary; they may optionally expose custom commands through `pluginapi.CommandHandler` and help modules through `pluginapi.HelpProvider`. Runtime `.so` loading is deliberately excluded.
10. **Bounded protocol behavior.** One message target per `PRIVMSG`/`NOTICE`, up to 16 comma-separated channels per `JOIN`, one mode change per `MODE`, channel history capped at 100 messages, AI private history/context capped at 20 messages when enabled, four concurrent/in-memory attachment records, bounded attachment size, and bounded server/client/channel counts.
11. **Private-channel identity is zero-trust.** An unregistered nickname is not a durable identity. A private-channel participant is remembered across reconnects only when the participant is authenticated to a registered nick. Unregistered participation is remembered only for that live TCP session.

## 3. Filesystem and deployment

Non-container defaults use `./data`:

- database: `./data/ircapybara.db`;
- generated/default TLS certificate: `./data/tls/server.crt`;
- generated/default TLS key: `./data/tls/server.key`.

Environment variables may override these with either a safe absolute path or a safe `./relative` path. Parent traversal (`..`) is rejected.

Docker Compose explicitly overrides the paths to `/data/...` and mounts `/data` as a writable volume. The production image is a multi-stage build whose runtime stage is exactly `gcr.io/distroless/static-debian12:nonroot`. A separate `mwader/static-ffmpeg:latest` stage contributes only the static `ffmpeg`/`ffprobe` executables. Debug uses the Go 1.27 Debian toolchain image. `/tmp` is mounted as tmpfs in Compose so optional media-plugin working files are ephemeral and RAM-backed.

## 4. Persistent data

SQLite stores only:

- registered nick identity: normalized nick, display nick, random salt, password-derived hash, registration timestamp;
- channel catalog: name, topic, public/private policy, history-enabled policy, creator identity marker;
- registered identities that have previously participated in private channels;
- server/channel bans.

Message history is deliberately **not persisted**. The server keeps at most 100 recent messages per history-enabled channel in RAM and loses them on restart.

Registered-nick passwords use PBKDF2-HMAC-SHA-256 with a random 128-bit salt and 210,000 iterations. Plaintext identity passwords are never persisted. Operator/server passwords supplied via environment are converted to in-memory salted hashes at startup and then removed from the live config copy.

## 5. IRC command acceptance criteria

### `PASS <password>`

Optional shared server-entry password, not registered-nick authentication.

- accepted only before IRC registration;
- argument matches the bounded printable ASCII password regexp;
- when no shared password is configured, a syntactically valid `PASS` is harmless;
- when configured, IRC registration cannot complete until it verifies;
- incorrect password returns `464` and may be retried;
- plaintext is never logged or persisted.

### `NICK <nickname>`

Select/change IRC nickname.

- strict ASCII nickname regexp, maximum 31 characters;
- case-insensitively unique among live clients;
- a persistently registered nick may be selected before identification, but the connection is marked identity-read-only until matching `IDENTIFY` succeeds;
- identity-read-only users still complete IRC registration, may join/part channels, remain visible to other members, and may use read/query commands; publishing and shared-state mutation are blocked;
- post-registration changes are broadcast to shared-channel peers;
- nick changes do not remove IRC-op authority or channel memberships.

### `USER <username> <mode> * [:]<realname>`

Complete IRC registration metadata.

- accepted only before registration;
- username is strict ASCII, 1..32 characters;
- mode is decimal and ignored;
- realname is printable UTF-8, maximum 80 code points;
- `:` is required when the realname contains spaces and optional for a single-token realname, matching common IRC client behavior;
- server username bans and guard identity rules are applied;
- registration completes only after valid `NICK`, `USER`, and optional `PASS` requirements, and after `CAP END` when CAP negotiation was started.

### `PING [:]<token>` / `PONG [:]<token>`

Basic liveness interoperability.

- token is bounded safe ASCII;
- `PING` gets `PONG` with the same token;
- valid `PONG` is accepted and otherwise ignored in this iteration.

### `QUIT [:<reason>]`

Clean disconnect.

- optional reason is printable UTF-8, maximum 160 code points;
- connection closes;
- shared-channel peers receive one `QUIT` notification;
- all per-connection memberships, invites, and session-only private-channel participation are removed.

### `JOIN <#channel>[,#channel...] [key[,key...]]`

Join an existing channel.

- one to 16 comma-separated channel names are accepted; optional comma-separated keys map positionally to channels;
- unknown channel is not created and produces a server `NOTICE` saying it does not exist;
- channel bans, invite-only state, key, and user limit are checked;
- private channels created as `PRIVATE` are invite-only by default;
- a successful join broadcasts `JOIN`, then returns topic state and `NAMES`;
- successful participation is remembered in RAM for this connection;
- if the client is authenticated to a registered nick, first participation is persisted once for private-channel listing visibility across reconnects;
- IRC operators may bypass invite-only/key/limit but not bans;
- no IRC `+v` voice mode exists.

### `PART <#channel> [:<reason>]`

Leave a channel without disconnecting.

- client must currently be a member;
- optional reason is printable UTF-8, maximum 160 code points;
- `PART` is broadcast to members including the leaving client;
- prior-participation visibility remains for the session and, for authenticated registered identities, across reconnects.

### `PRIVMSG <target> :<text>`

Normal conversation.

- one strict nick or existing channel target;
- a single-line text is 1..32,768 printable UTF-8 code points and contains no IRC/terminal controls;
- multiline messages are accepted only as a complete negotiated `batch` / `draft/multiline` batch (see §7); reassembled text follows the same character and control-character validation;
- channel sender must be a member;
- moderated channels allow only channel operators or IRC operators;
- direct messages require a connected registered target;
- plugin `BeforeMessage` hooks run before delivery and transformed output is revalidated;
- for a history-enabled channel, the final validated channel message is appended to its RAM history;
- no history write reaches SQLite;
- guard level 3 blocks `PRIVMSG`.

### `NOTICE <target> :<text>`

Informational message using IRC's no-auto-error/no-auto-reply convention.

- same input bounds as `PRIVMSG`;
- delivered similarly to `PRIVMSG`;
- ordinary target/member/mode failures do not generate an IRC error response;
- history-enabled channels retain delivered channel notices in RAM;
- guard level 3 silently drops user `NOTICE` rather than generating a response loop.

### `TOPIC <#channel> [:<topic>]`

Inspect/change channel topic metadata.

- `TOPIC #channel` is a query;
- `TOPIC #channel :text` sets or clears the topic;
- topic is printable UTF-8, maximum 180 code points;
- private-channel topic inspection is limited to current members or IRC operators;
- default `+t` means channel-op or IRC-op is required to change topic;
- `MODE #channel -t` allows a member to change topic;
- successful topic changes are persisted;
- guard level 3 permits topic queries but blocks topic mutation.

### `NAMES <#channel>`

List current members.

- exactly one channel;
- private-channel member enumeration requires current membership or IRC-op authority;
- channel operators are prefixed with `@`;
- output is chunked and terminated by `366`.

### `WHO <#channel|mask>`

Lightweight user query.

- channel form returns current members;
- private-channel form requires current membership or IRC-op authority;
- mask form first passes a bounded regexp and only then becomes an anchored wildcard regexp;
- `*` and `?` are supported;
- mask matches case-insensitively against nick, username, realname, and IP string;
- results use `352`, terminated by `315`.

### `WHOIS <nickname>` / `WHOIS <nickname> <nickname>`

Detailed live-user query.

- accepts one strict nickname or the common duplicate-nickname two-argument client form;
- when two arguments are present, the final nickname token is resolved;
- returns username, IP-as-host, realname, visible channel memberships, and IRC-op marker;
- private channel membership is shown only to the target themself, a current member of that private channel, or an IRC operator;
- missing user returns `401`;
- terminated by `318`.

### `LIST`

List the channel catalog.

- takes no parameters;
- public channels are always listed;
- a private channel is listed only to IRC operators, its creator identity, a current/session participant, or an authenticated registered identity recorded as a prior participant;
- unregistered prior participation is not trusted across reconnects;
- listed rows include member count and topic;
- empty channels remain part of the catalog.

### `MODE <#channel> [<mode> [parameter]]`

Query/change bounded channel state.

- one channel and at most one mode change;
- private-channel mode inspection requires current membership or IRC-op authority;
- mutation requires channel-op or IRC-op authority;
- supported modes: `+/-o`, `+/-b`, `+/-i`, `+/-k`, `+/-l`, `+/-m`, `+/-t`, `+/-s`, `+/-H`;
- `+o/-o`: channel membership authority;
- `+b/-b`: durable IP/CIDR/username channel bans;
- `+b` without a mask lists channel bans; classic client query form `MODE #channel b` is accepted as the same read-only ban-list request;
- `+i`: invite-only;
- `+k`: channel key;
- `+l`: user limit;
- `+m`: moderated;
- `+t`: topic restricted to channel operators;
- `+s`: private/unlisted channel catalog policy; `-s` makes it public again; policy is persisted;
- `+H`: enable RAM history; policy is persisted;
- `-H`: disable history, persist the policy, and immediately discard the current RAM history buffer;
- private-channel creation sets `+i` automatically, but changing `+s/-s` later does not implicitly change `+i`;
- `+v` is unsupported;
- guard level 3 permits only mode query and ban-list query (`+b` without a mask), not mutation.

### `KICK <#channel> <nick> [:<reason>]`

Remove a current channel member.

- requester must be channel operator or IRC operator;
- target must currently belong to the channel;
- optional reason is printable UTF-8, maximum 160 code points;
- event is sent to current members including the target;
- guard level 3 blocks `KICK`.

### `INVITE <nick> <#channel>` — low priority

One-time admission into an invite-only/private channel.

- requester must be channel operator or IRC operator;
- target must be connected and not already a member;
- invite is RAM-only and consumed by successful `JOIN`;
- invited user receives the channel name directly even though it may not be visible in `LIST` yet;
- no persistence or IRCv3 invite-notify semantics;
- guard level 3 blocks new invites.

### `OPER <oper-name> <password>`

Attach server-wide operator authority to the current connection.

- requester must already be an IRC-registered client;
- credentials match configured operator credentials;
- success sets per-connection IRC-op state, returns `381`, and emits user `+o` indication;
- authority survives nick/channel changes but disappears on disconnect;
- password is never logged or persisted.

### `KILL <nick> [:<reason>]`

Force-disconnect another client.

- IRC-op only;
- exact connected target nick;
- target receives `KILL`/`ERROR`, is removed from RAM state, and its socket is closed;
- security administration remains available during guard level 3.

### `TIME`

Return server wall-clock time.

- no arguments;
- requester must be registered;
- responds with numeric `391`;
- timestamp is UTC RFC3339 for deterministic output.

### `UPTIME`

Return process uptime.

- no arguments;
- requester must be registered;
- returns a server `NOTICE` containing elapsed time since this server process started.

## 6. IRCapybara custom/support commands

### `REGISTER <password>`

Claims the currently used nick as a durable identity.

- fully IRC-registered and captcha-cleared connection required;
- password is 8..128 printable non-space ASCII characters;
- nick can be registered only once;
- salt/hash and registration timestamp are persisted;
- plaintext is discarded;
- successful registration authenticates the current connection to that identity;
- guard level 3 blocks new identity registration because it is a durable/user-state mutation.

### `IDENTIFY [<nick>] <password>`

Authenticate a durable nick identity on the current connection.

- `IDENTIFY <password>` authenticates the current nickname;
- `IDENTIFY <nick> <password>` remains available and may be issued before `NICK`;
- selecting a protected nickname before identification is allowed, but the connection is identity-read-only until the matching password succeeds;
- identity-read-only permits connection lifecycle, `JOIN`/`PART`, and read/query commands while blocking `PRIVMSG`, `NOTICE`, topic/mode mutation, kicks/invites, registration, channel creation, bans, and other shared-state mutation;
- successful authentication clears identity-read-only state when it matches the current nick and remains associated with the connection;
- with `IRCAPYBARA_IDENTIFY_TRUST_IP=true`, successful `REGISTER` or `IDENTIFY` remembers only that `(remote IP, identity)` pair in RAM; reconnecting to the same registered nick from that IP auto-identifies until server restart; this never authorizes other identities from the same IP;
- deliberately smaller than SASL/services.

### `CHANADD <#channel> [PUBLIC|PRIVATE [HISTORY|NOHISTORY]] :<topic>`

IRC-op-only channel catalog creation.

Accepted examples:

```text
CHANADD #ops :Operations
CHANADD #secret PRIVATE :Private room
CHANADD #scratch PRIVATE NOHISTORY :Ephemeral private discussion
CHANADD #public PUBLIC NOHISTORY :No retained history
```

Behavior:

- omitted policy means `PUBLIC HISTORY`;
- `PRIVATE` means unlisted/secret catalog behavior and starts invite-only;
- `NOHISTORY` means no channel message buffer;
- channel metadata is persisted;
- implicit creation through `JOIN` remains impossible;
- blocked by guard level 3 read-only mode.

### `HELP [module]`

Return concise syntax and examples for core commands and loaded plugin modules.

- `HELP` lists available help modules;
- `HELP JOIN`, `HELP XFER`, `HELP AI`, `HELP YTDL`, etc. return module-specific lines;
- module names are ASCII and bounded to 32 characters;
- plugin modules are discovered only through the narrow `pluginapi.HelpProvider` interface;
- unknown modules return numeric `524`;
- output uses `704`/`705`/`706`;
- the command is read-only and remains available to identity-read-only users and during guard level 3.

### `HISTORY <#channel> [limit]`

Replay recent RAM history.

- requester must be registered;
- limit defaults to 20 and must be 1..100;
- requester must currently be a member, unless they are an IRC operator;
- history-free channels return a server notice saying history is disabled;
- replay is oldest-to-newest from the selected tail of the buffer;
- legacy clients receive ordinary IRC message lines, so no IRCv3 dependency is required;
- timestamps are retained internally for future extended rendering but are not yet encoded into legacy replay lines;
- history disappears on process restart.

### `GUARD <0|1|2|3>`

IRC-op-only emergency admission control.

- `0`: off;
- `1`: runtime snapshot allows reconnect when either source IP or IRC username was present;
- `2`: runtime snapshot allows reconnect only from snapshotted IPs;
- `3`: first prunes recent untrusted sessions, then snapshots surviving IPs exactly like level 2, and finally enables read-only mode.

Guard level 3 pruning rule:

- IRC operators are retained;
- any connection that is at least 30 minutes old is retained;
- a younger connection is retained if it is authenticated to its current registered identity and that identity itself was registered at least 30 minutes ago;
- remaining younger clients are disconnected **before** the allowlist snapshot is taken;
- this means old sessions survive, and old registered identities survive even after a recent reconnect, while both new anonymous sessions and freshly-created identities are removed.

Guard level 3 read-only policy:

- users may connect when their IP is allowed, authenticate, query state, join/part channels, retrieve history, use `TIME`/`UPTIME`, and disconnect;
- user content/state mutations such as `PRIVMSG`, `NOTICE`, topic changes, mode changes, `KICK`, `INVITE`, `REGISTER`, and `CHANADD` are blocked;
- operator/security escape controls (`OPER`, `GUARD`, `KILL`, `BANADD`, `BANDEL`, `BANLIST`) remain available so the mode can be managed or disabled.

Startup guard levels 2/3 require an explicit IP allowlist. Startup level 1 requires an IP and/or username allowlist.

### `BANADD <SERVER|#channel> <IP|CIDR|USER> <value>`
### `BANDEL <SERVER|#channel> <IP|CIDR|USER> <value>`
### `BANLIST <SERVER|#channel>`

IRC-op durable ban administration.

- server IP/CIDR bans can reject at accept time;
- username bans are enforced once `USER` is known;
- adding a server ban disconnects matching live clients;
- channel bans can also be changed through `MODE +b/-b`;
- security ban administration remains available under guard level 3.

### `CAPTCHA <challenge-id> <answer>`

Optional first-server-join friction control.

- each newly IRC-registered connection receives a small addition problem;
- challenge ID and answer format are regexp constrained;
- until solved, all commands except `CAPTCHA`, `PING`, `PONG`, and `QUIT` are silently ignored;
- challenge state is RAM-only;
- not intended as strong bot-proofing.

### `XMODE PLAIN|EXTENDED`

Per-connection IRCapybara extension preference.

- `PLAIN` means ordinary IRC rendering semantics;
- `EXTENDED` signals that the client wants IRCapybara-specific richer responses where defined;
- current rendering format remains passthrough/reserved;
- `EXTENDED` is also required for the IRCapybara `XFER`/`RECV` attachment protocol;
- the hook exists so future clients can negotiate richer representations without changing ordinary-client behavior.

### `XFER START|CHUNK|END ...`

IRCapybara-specific extended-mode attachment upload. Classic DCC is not used.

Wire form:

```text
XFER START <#channel|nick> <size-bytes> <mime> :<filename>
XFER CHUNK <fileid> <part-number> <base64>
XFER END <fileid> <sha256-hex>
```

Acceptance criteria:

- requester must be fully IRC-registered, writable, and in `XMODE EXTENDED`; identity-read-only and guard-level-3 sessions cannot upload;
- target is exactly one live nick or one existing channel; channel sender must currently be a member;
- declared size is positive and cannot exceed `IRCAPYBARA_TRANSFER_MAX_MB` (default 15 MiB, accepted range 1..64 MiB);
- filename, MIME type, file ID, part number, base64 payload, and checksum all pass dedicated regexps before semantic processing;
- server allocates an unpredictable 96-bit file ID and replies `XFER READY <nick> <fileid> 240 <parts>`; clients do not choose file IDs;
- every non-final decoded chunk is exactly 240 bytes; the final decoded chunk is exactly the remaining declared size;
- part numbers begin at zero and must arrive exactly once in ascending order; duplicates/out-of-order chunks are rejected;
- decoded bytes may never exceed the declared size; malformed base64 is rejected;
- `XFER END` succeeds only after all expected bytes arrived and SHA-256 matches; checksum mismatch discards the upload;
- at most four uploading/completed files occupy the server RAM window at once;
- incomplete uploads expire after ten minutes; downloaded completed files expire one hour after the first successful `RECV`; completed files that are never downloaded expire after 24 hours to prevent permanent RAM pinning;
- disconnecting the uploading connection discards its incomplete uploads; completed files remain until their normal expiry;
- normal IRC command flood limits remain unchanged; only validated `XFER CHUNK` traffic uses a separate bounded transfer token bucket so practical file transfer does not take hours;
- attachments are never written to SQLite and are never inserted into ordinary channel message history.

Recipient behavior:

- when upload completion succeeds, the server snapshots the eligible recipient connections/registered identities; joining the channel later does not grant access to an older attachment;
- recipient must still be in the source channel when downloading a channel attachment; direct attachments are limited to the snapshotted direct recipient;
- IRC-operator status alone does not bypass attachment ACLs;
- extended clients receive `XATTACH <conversation> <fileid> <size> <mime> :<filename>` from the actual sender;
- non-extended recipients receive a normal textual fallback stating that an attachment arrived and requires extended mode.

### `RECV <fileid>`

Download a completed attachment from server RAM.

- requester must be registered and in `XMODE EXTENDED`;
- `RECV` is a read operation and remains available in identity-read-only and guard-level-3 modes;
- file ID must match the strict server file-ID regexp and still exist;
- current conversation membership plus snapshotted recipient ACL must authorize the requester;
- server emits `XFER START`, ordered base64 `XFER CHUNK` lines, then `XFER END <fileid> <sha256>`;
- download chunk size remains fixed at 240 raw bytes; attachment framing intentionally stays small even though ordinary IRCapybara chat lines now support much larger text payloads;
- the one-hour downloaded-file expiry clock starts only after a successful complete replay to a recipient.

## 7. IRCv3 compatibility and multiline messages

`CAP LS [302]`, `CAP LIST`, `CAP REQ`, and `CAP END` are handled. `CAP LS 302` advertises `server-time`, `ircapybara/history`, `batch`, and `draft/multiline=max-bytes=131072,max-lines=512`. A request containing only supported capabilities receives `ACK`; a request containing an unsupported capability receives `NAK`. Starting `CAP LS` suspends IRC registration until `CAP END`; welcome numerics must not be emitted while CAP negotiation is active. `MOTD` is implemented because regular clients commonly expect it.

Clients use multiline only when both `batch` and `draft/multiline` are enabled. A client opens a batch with `BATCH +<id> draft/multiline <target>`, sends tagged `PRIVMSG` or `NOTICE` fragments using `@batch=<id>`, and closes it with `BATCH -<id>`. A fragment tagged `draft/multiline-concat` joins directly to the previous fragment. Otherwise the server inserts one LF between fragments. All fragments must use the same target and command. The server accepts one open client batch per connection, validates and bounds the complete message before dispatching it once through the ordinary message, permission, plugin, and history paths, and expires an unfinished batch after 30 seconds. Limits are 131,072 UTF-8 bytes, 512 fragments, and the ordinary 32,768-code-point message limit. Invalid batches are discarded and reported with `FAIL BATCH`; incomplete batches are cleared on disconnect.

To recipients that negotiated both capabilities, the server frames a multiline chat message as one `draft/multiline` batch. It sends ordinary nonblank message lines to recipients without both capabilities; blank lines cannot be represented in that fallback. The multiline formatter keeps each fragment within a 510-byte envelope, including tags and command metadata. Long fragments are split at UTF-8 rune boundaries and marked with `draft/multiline-concat` so reassembly does not add a line break. The client emits this format when the server advertises usable limits. For older IRCapybara peers, the client retains the private U+2028 line-separator encoding; this compatibility shim is not part of IRCv3.

`ircapybara/history` is intentionally an IRCapybara capability rather than a claim of complete IRCv3 `draft/chathistory` support. It tells plugins that the client accepts synthetic replay messages marked with an `ircapybara-history=1` message tag. If `server-time` is also enabled, those replay messages include a `time=` tag. Clients that do not request the capability receive plugin backlogs as ordinary IRC messages only to that client.

## 8. Protection behavior

- maximum total clients;
- maximum live connections per source IP;
- token-bucket command rate limit per connection;
- separate bounded transfer-chunk token bucket plus strict ordered chunk accounting;
- attachment RAM window capped at four records and per-file size capped by configuration;
- line-size bound before command parsing;
- strict UTF-8 and regexp validation;
- TLS handshake/admission/protocol failures are logged without logging raw IRC command contents or credentials;
- server IP/CIDR/username bans;
- channel IP/CIDR/username bans;
- guard levels 0/1/2/3;
- optional math captcha;
- no terminal/CTCP control bytes in relayed free text;
- registered-identity password hashing with per-identity random salt;
- private-channel durable visibility only for authenticated registered identities.

## 9. Plugin entry points and built-ins

Compile-time plugins implement `pkg/pluginapi.Plugin`; command-owning plugins may additionally implement `pluginapi.CommandHandler`. The host API deliberately exposes only validated operations needed by plugins: RAM-only channel creation, joining an authenticated identity to a plugin-owned channel, bounded synthetic delivery, connection views, user count, the configured attachment byte limit, and targeted server-side attachment publication. Plugins do not receive raw sockets, SQLite handles, or internal maps.

Acceptance criteria common to all built-ins:

- plugin configuration is bounded and regexp validated before startup succeeds;
- IRC-originating plugin commands/messages have already passed the core regexp/UTF-8 parser, and plugin-specific syntax receives another dedicated regexp;
- externally fetched/generated text is stripped of control characters, flattened to a single IRC-safe line, bounded, and then revalidated by the host before delivery;
- plugin-created channels are RAM-only unless an administrator separately creates a durable core channel;
- plugin synthetic live messages are rejected while guard level 3 is active; synthetic history replay and notices remain readable;
- the Go binary remains buildable with `CGO_ENABLED=0`; the media plugin invokes an external static FFmpeg executable rather than linking libav/FFmpeg.

### MirrorPlugin

Configuration: `IRCAPYBARA_MIRROR_SOURCES`, semicolon-separated entries with exact form:

```text
rss|#channel|https://feed|1h
rss|#channel|https://feed|24h
tg|#channel|public_channel_name|1h
tg|#channel|public_channel_name|24h
```

Acceptance criteria:

- RSS/Atom fetches accept HTTPS only, cap response bodies at 2 MiB, reject credentials/fragments and private/loopback/link-local resolved addresses, and follow at most three safe HTTPS redirects;
- Telegram mirror fetches only `https://t.me/s/<validated-public-channel-name>` and parses `data-post`, message text/date, plus simple media fallbacks;
- each source has one polling goroutine and polls only hourly or daily as configured;
- first successful fetch seeds RAM state without broadcasting the existing backlog as new;
- source IDs use RSS GUID/Atom ID when available, otherwise SHA-256 of stable feed fields; Telegram uses `<channel>/<post-id>`;
- last ID plus the latest 100 seen IDs/posts live in RAM only;
- new unseen posts are broadcast oldest-to-newest;
- RSS `pubDate`/Atom date is parsed when possible and rendered as compact `[MM-DD HH:MM]` prefix; Telegram preview dates use the same compact prefix when available;
- Telegram mirror messages use the configured public Telegram channel username as the IRC sender nick when it is IRC-safe, with a bounded `tg-...` fallback otherwise;
- attached mirror channels have core IRC history disabled, so user discussion is permitted but not retained by core history;
- joining clients receive up to 100 latest source posts only to themselves;
- `ircapybara/history` clients receive tagged synthetic history; legacy clients receive ordinary private-to-that-client channel `PRIVMSG` replay.

### BridgePlugin — Telegram provider

Configuration: `IRCAPYBARA_BRIDGE_TG_BOT_TOKEN` and `IRCAPYBARA_BRIDGE_TG_MODE=channel|dm`.

Plugin command:

```text
BRIDGE tg:<chat_id> <name>
```

Acceptance criteria:

- command syntax is regexp constrained; `chat_id` accepts Telegram positive/negative integer IDs and `<name>` is an IRC-safe short identifier;
- requester must be registered and authenticated to a durable IRC identity;
- one Telegram chat ID can be owned by only one IRC identity; one generated bridge target can map to only one chat;
- bindings are RAM-only in this iteration;
- channel mode creates `#tg-<name>` as a private, invite-only, history-free, RAM-only channel owned by the authenticated identity and automatically joins that identity;
- DM mode creates a virtual `tg-<name>` message target without a connected virtual socket;
- outbound bridge traffic is intercepted before normal IRC channel/DM routing and sent through one bounded queue of 64 entries;
- Telegram input arrives through one `getUpdates` long-poll loop; startup discards the pre-process update backlog because there is no durable binding state to route it safely;
- injected Telegram messages use host synthetic delivery directly and therefore never pass back through the outbound hook;
- text is mirrored as text; photo/video/sticker/animation/voice currently render as `[image]`, `[video]`, `[sticker]`, `[gif]`, `[voice]`;
- an unmapped inbound chat auto-binds only when exactly one eligible authenticated IRC identity is online;
- with total connected user count 2–9, Telegram receives inline selection buttons for eligible authenticated identities; callback data is bounded and validated before use;
- at 10+ users, zero users, or otherwise ambiguous/no-eligible-user state, Telegram receives a waiting-for-IRC-initiation message rather than a large keyboard;
- at most 100 unmatched latest chat messages are retained in RAM awaiting selection/initiation.

### YouTubeFFmpegPlugin

Configuration variables:

```text
IRCAPYBARA_YTDL_ENABLED=true|false
IRCAPYBARA_YTDL_MODE=channel|dm|both
IRCAPYBARA_YTDL_CHANNEL=#yt-dl
IRCAPYBARA_YTDL_PRIVATE=true|false
IRCAPYBARA_YTDL_NICK=yt-dl
IRCAPYBARA_YTDL_FFMPEG_PATH=<safe binary name/path>
IRCAPYBARA_YTDL_FFMPEG_PRESET=-c:v copy -c:a copy -movflags +faststart -f mp4
IRCAPYBARA_YTDL_FILENAME_TEMPLATE={title}-{id}.{ext}
```

Acceptance criteria:

- disabled unless explicitly enabled;
- channel mode creates/attaches to a history-free `#yt-dl`-style plugin channel; `PRIVATE=true` makes it unlisted without making it invite-only;
- DM mode intercepts messages to the virtual configured nick without creating a socket-backed IRC user; `both` supports both surfaces;
- requester must have `XMODE EXTENDED`; the channel form also requires actual channel membership;
- request body is exactly one canonical HTTPS `youtube.com`/`youtu.be` URL that matches the plugin regexp; arbitrary hosts, HTTP URLs, and lookalike domains are rejected before the YouTube library sees them;
- requests are intercepted and never broadcast as ordinary IRC chat; progress is delivered only to the requester;
- one worker executes jobs serially; queue capacity is eight; a non-operator may have only one active-or-queued request while operators may enqueue additional work subject to the global queue;
- each job has a twelve-minute context deadline; FFmpeg itself has a four-minute timeout;
- the plugin uses `kkdai/youtube/v2` to obtain metadata/stream data and selects a combined `video/mp4` stream that fits below the server attachment budget, reserving headroom for container overhead; unknown-size streams are bounded while copying;
- source download is hard-limited before temporary-file growth can exceed the budget;
- FFmpeg preset is trusted administrator configuration, never IRC-user input. The primary form is ordinary FFmpeg argv (for example `-vn -c:a libopus -b:a 6k -ac 1 -ar 16000`), tokenized in Go without invoking a shell; single/double quotes and backslash escaping are supported but no shell expansion/substitution/globbing occurs. The earlier unrestricted `key=value;...` syntax remains accepted for compatibility;
- administrator FFmpeg flags are not allowlisted. The managed output extension is derived from a simple `-f` token when possible (otherwise a safe fallback is used); a server-owned final `-fs` size bound is injected after admin options, and the managed input uses a local `file,pipe` protocol whitelist so the normal YouTube source cannot make FFmpeg fetch arbitrary URLs through that input;
- filename template accepts only `{title}`, `{id}`, and `{ext}`; externally sourced title text is sanitized/bounded in Go before publication;
- Docker obtains static `ffmpeg`/`ffprobe` from `mwader/static-ffmpeg:latest` and runs the normal image read-only with a bounded `/tmp` tmpfs; non-Docker operators should point `TMPDIR` at ephemeral/RAM-backed storage if they require the same property;
- final output is read only after its size is verified, then published through `Host.SendAttachmentToClient`; the core attachment store makes its own copy and applies the same four-file/size/ACL/expiry rules;
- only media the user/operator is authorized to retrieve should be requested.

Progress stages include queued position, metadata retrieval, selected stream, 10% download buckets when size is known, FFmpeg start/completion, attachment publication, and terminal failure/success messages.

### ChatCompletions plugin

Configuration variables:

```text
IRCAPYBARA_CHAT_ENABLED=true|false
IRCAPYBARA_CHAT_ENDPOINT=<http-or-https Chat Completions endpoint>
IRCAPYBARA_CHAT_API_KEY=<optional bearer key>
IRCAPYBARA_CHAT_MODEL=<model>
IRCAPYBARA_CHAT_REASONING_EFFORT=<none|minimal|low|medium|high|xhigh|max|empty>
IRCAPYBARA_CHAT_CHANNEL=#ai
IRCAPYBARA_CHAT_PRIVATE=true|false
IRCAPYBARA_CHAT_HISTORY_ENABLED=true|false
IRCAPYBARA_CHAT_NICK=chat-ai   # fallback synthetic nick
IRCAPYBARA_CHAT_SYSTEM_PROMPT=<optional single-line prompt>
```

Acceptance criteria:

- enabling requires a valid endpoint, model, IRC channel and synthetic IRC nick;
- plugin creates/attaches to a history-free channel; `PRIVATE=true` makes it unlisted but not invite-only;
- user must actually be joined to the configured channel before a prompt is accepted;
- prompt is intercepted before normal IRC fan-out/history, so no other member sees it;
- request body contains `model`, bounded conversation `messages`, and optional `reasoning_effort`; it supplies no tool/function definitions;
- Authorization bearer header is sent only when an API key is configured;
- response body is capped at 2 MiB and only the first choice's text/refusal is used;
- the response `model` field is converted to an IRC-safe synthetic responder nick; configured `IRCAPYBARA_CHAT_NICK` is used only as fallback;
- model output preserves paragraph and code-block line breaks, is capped at the server 32,768-code-point message limit, and is delivered only to the originating connection without artificial 390-character chunking;
- `AIRESET` with no arguments resets only the requesting user's context; exact AI-channel messages `!reset` and `/reset` are aliases; reset increments a per-session epoch so an already in-flight stale response is discarded instead of repopulating cleared context;
- `IRCAPYBARA_CHAT_HISTORY_ENABLED=true` (default) keeps private RAM context/history keyed by authenticated identity when present, otherwise by connection ID; unauthenticated session state is removed on disconnect; when disabled, each request is stateless;
- maximum retained context/history is 20 total user/assistant messages; on join it is replayed only to that user (synthetic history for `ircapybara/history` clients, private-to-that-client channel messages otherwise);
- `AIRESET`/`!reset`/`/reset` clears the same retained state and invalidates any in-flight response epoch; one request per user and four global requests may be in flight;
- HTTP work occurs outside the IRC connection read loop.

## 10. Future-awareness, not current behavior

The architecture should not prevent later addition of:

- persistent/IRCv3-compatible history;
- E2EE overlay metadata;
- negotiated extended rendering;
- server-side bots/agents as compile-time plugins;
- bridges to other messengers;
- richer media types layered on the current bounded attachment transport, including images, GIFs, stickers, and voice-message metadata/rendering;
- terminal-friendly data transport;
- a possible SSH overlay for authenticated/encrypted data transfer, explicitly not remote command execution through IRC.

The current attachment wire format is defined above; richer rendering/E2EE/SSH-overlay semantics remain intentionally undefined.
