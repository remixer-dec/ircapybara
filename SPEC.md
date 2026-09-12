# IRCapybara v0.2 prototype specification

## 1. Scope

IRCapybara is a small, single-server IRC service built for a controlled use case. It is not intended to be a general-purpose IRC daemon or an IRC network implementation. The design prioritizes deterministic behavior, strict input validation, a small attack surface, ordinary IRC-client compatibility on the common path, low storage write volume, and explicit room for IRCapybara-specific extensions later.

Out of scope for this iteration: server linking, SASL, NickServ/ChanServ-style services, account federation, IRC `+v` voice status, voice calls, WebSockets, metrics, structured logging, HTTP APIs, arbitrary implicit channel creation, CTCP interpretation, file transfer, E2EE, dynamic shared-object plugins, and persistent chat-history storage.

## 2. Design rules

1. **Single server.** No server-to-server protocol or distributed state.
2. **Strict regexp input boundary.** Every client line must be valid UTF-8, at most 510 bytes excluding CRLF, match the command-envelope regexp, and then match a command-specific regexp. Free text is additionally checked against an allowlist regexp that rejects IRC/terminal control characters.
3. **ASCII identifiers, UTF-8 text.** Nicknames, usernames, channel names, modes, passwords, keys, command tokens, IP/CIDR values, and similar protocol identifiers use bounded ASCII forms. Messages/topics/reasons/realnames accept printable UTF-8. ASCII is therefore supported as a UTF-8 subset. No other text encoding is supported.
4. **No implicit channel creation.** An unknown `JOIN` never creates a channel. Channels are seeded from configuration or created by an IRC operator using `CHANADD`.
5. **RAM-first state.** Connections, memberships, invites, rate-limit state, captcha state, guard state, mode state, and message history live in memory.
6. **On-demand persistence only.** SQLite writes occur only for durable identity registration, channel catalog/policy/topic changes, durable private-channel participation, and bans. Normal chat does not write to SQLite.
7. **SQLite without CGO/ORM.** `github.com/glebarez/go-sqlite` is the sole direct non-standard-library dependency and is accessed through `database/sql`.
8. **TLS-first.** TLS is enabled by default. The server may generate and use a self-signed RSA-2048 certificate for legacy IRC-client compatibility. Client certificates are not required or verified. The TLS endpoint accepts TLS 1.0 through TLS 1.3; TLS 1.0/1.1 and RSA key-exchange suites exist strictly for legacy IRC-client compatibility.
9. **Compile-time Go plugins.** Plugins implement `pkg/pluginapi.Plugin` and are linked into the binary. Runtime `.so` loading is deliberately excluded.
10. **Bounded protocol behavior.** One target/channel per relevant command, one mode change per `MODE`, channel history capped at 100 messages, and bounded server/client/channel counts.
11. **Private-channel identity is zero-trust.** An unregistered nickname is not a durable identity. A private-channel participant is remembered across reconnects only when the participant is authenticated to a registered nick. Unregistered participation is remembered only for that live TCP session.

## 3. Filesystem and deployment

Non-container defaults use `./data`:

- database: `./data/ircapybara.db`;
- generated/default TLS certificate: `./data/tls/server.crt`;
- generated/default TLS key: `./data/tls/server.key`.

Environment variables may override these with either a safe absolute path or a safe `./relative` path. Parent traversal (`..`) is rejected.

Docker Compose explicitly overrides the paths to `/data/...` and mounts `/data` as a writable volume. The production image is a multi-stage build whose runtime stage is exactly `gcr.io/distroless/static-debian12:nonroot`. Debug uses the Go 1.27 Debian toolchain image.

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

### `JOIN <#channel> [key]`

Join an existing channel.

- exactly one channel per command;
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
- text is 1..400 printable UTF-8 code points and contains no IRC/terminal controls;
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

### `WHOIS <nickname>`

Detailed live-user query.

- one strict nickname;
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
- the hook exists now so future clients can negotiate richer representations without changing ordinary-client behavior.

## 7. Compatibility shims

`CAP LS [302]`, `CAP LIST`, `CAP REQ`, and `CAP END` are handled so modern clients that probe IRCv3 can connect. The server advertises exactly `server-time` and `ircapybara/history` in this iteration. A valid request containing only supported capabilities receives `ACK`; any request containing an unsupported capability receives `NAK`. Starting `CAP LS` suspends IRC registration until `CAP END`; welcome numerics must not be emitted while CAP negotiation is active. `MOTD` is implemented because regular clients commonly expect it.

`ircapybara/history` is intentionally an IRCapybara capability rather than a claim of complete IRCv3 `draft/chathistory` support. It tells plugins that the client accepts synthetic replay messages marked with an `ircapybara-history=1` message tag. If `server-time` is also enabled, those replay messages include a `time=` tag. Clients that do not request the capability receive plugin backlogs as ordinary IRC messages only to that client.

## 8. Protection behavior

- maximum total clients;
- maximum live connections per source IP;
- token-bucket command rate limit per connection;
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

Compile-time plugins implement `pkg/pluginapi.Plugin`; command-owning plugins may additionally implement `pluginapi.CommandHandler`. The host API deliberately exposes only validated operations needed by plugins: RAM-only channel creation, joining an authenticated identity to a plugin-owned channel, bounded synthetic delivery, connection views, and user count. Plugins do not receive raw sockets, SQLite handles, or internal maps.

Acceptance criteria common to all built-ins:

- plugin configuration is bounded and regexp validated before startup succeeds;
- IRC-originating plugin commands/messages have already passed the core regexp/UTF-8 parser, and plugin-specific syntax receives another dedicated regexp;
- externally fetched/generated text is stripped of control characters, flattened to a single IRC-safe line, bounded, and then revalidated by the host before delivery;
- plugin-created channels are RAM-only unless an administrator separately creates a durable core channel;
- plugin synthetic live messages are rejected while guard level 3 is active; synthetic history replay and notices remain readable;
- no plugin adds a non-pure-Go dependency.

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
- model output is converted to bounded single-line IRC-safe chunks and delivered only to the originating connection;
- `AIRESET` with no arguments resets only the requesting user's context; exact AI-channel messages `!reset` and `/reset` are aliases; reset increments a per-session epoch so an already in-flight stale response is discarded instead of repopulating cleared context;
- conversation state is keyed by authenticated identity when present, otherwise by connection ID; unauthenticated session state is removed on disconnect;
- maximum retained context is 24 user/assistant messages; one request per user and four global requests may be in flight;
- HTTP work occurs outside the IRC connection read loop.

## 10. Future-awareness, not current behavior

The architecture should not prevent later addition of:

- persistent/IRCv3-compatible history;
- E2EE overlay metadata;
- file transfer;
- negotiated extended rendering;
- server-side bots/agents as compile-time plugins;
- bridges to other messengers;
- images, GIFs, stickers, and voice-message blobs transported through future file-transfer/extended-rendering facilities;
- terminal-friendly data transport;
- a possible SSH overlay for authenticated/encrypted data transfer, explicitly not remote command execution through IRC.

No current protocol should pretend these future wire formats are already defined.
