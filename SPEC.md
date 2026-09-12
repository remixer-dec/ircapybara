# IRCapybara v0.1 prototype specification

## 1. Scope

IRCapybara is a single-server IRC service for a controlled environment, not a general-purpose IRC daemon and not an IRC network implementation. The first version prioritizes a small attack surface, deterministic behavior, bounded inputs, conventional IRC client compatibility on the common path, and a codebase that can later carry IRCapybara-specific extensions.

Out of scope for v0.1: server linking, SASL, services/NickServ/ChanServ, account federation, history, file transfer, media/voice, IRC `+v` voice status, WebSockets, metrics, structured logging, HTTP APIs, arbitrary user-created channels, CTCP parsing, and dynamic shared-object plugins.

## 2. Design rules

1. **Single server.** No server-to-server protocol or distributed state.
2. **Strict input boundary.** Every client line must be valid UTF-8, <=510 bytes excluding CRLF, match the command envelope regexp, and then match a command-specific regexp. Message/topic/realname text is additionally checked against an allowlist regexp that excludes control characters.
3. **ASCII identifiers, UTF-8 text.** Nicknames, usernames, channel names, modes, keys, passwords, and command tokens use tightly bounded ASCII forms. Message bodies, topics, kick/quit reasons, and realnames accept printable UTF-8. ASCII clients therefore work naturally as an UTF-8 subset. No other encoding is supported.
4. **No implicit channel creation.** Channels come from startup seed configuration or `CHANADD` by an IRC operator.
5. **RAM-first hot path.** Connections, memberships, modes, invites, flood state, captcha state, guard state, and chat traffic live in RAM. SQLite is read at startup and written only for durable mutations.
6. **SQLite persistence.** `github.com/glebarez/go-sqlite` is the only direct non-standard-library dependency. `database/sql` is used directly; no ORM and no CGO.
7. **TLS-first.** TLS is enabled by default. A self-signed Ed25519 certificate may be generated automatically. IRCapybara does not require client certificate verification.
8. **Compile-time plugins.** Plugins implement `pkg/pluginapi.Plugin` and are linked into the Go binary through the compile-time registry. No runtime `.so` loading is required.
9. **Bounded behavior.** One channel/target per relevant command and one channel-mode mutation per `MODE` invocation.

## 3. Persistent data

SQLite stores only:

- registered nick identity: normalized nick, display nick, random salt, password-derived hash;
- predefined channels and topics;
- server/channel bans.

Registered-nick passwords use PBKDF2-HMAC-SHA-256 with a random 128-bit salt and 210,000 iterations. Plaintext identity passwords are never stored. Operator/server passwords supplied through environment configuration are converted to in-memory salted hashes at startup and are not written to the database.

A future hardening release may replace PBKDF2 with a memory-hard KDF such as Argon2id if the dependency policy is relaxed.

## 4. IRC command acceptance criteria

### `PASS <password>`

Purpose: optional shared server-entry password, not registered-nick identity authentication.

Acceptance criteria:
- accepted only before IRC registration;
- argument is 1..128 printable non-space ASCII bytes;
- when no server password is configured, a valid `PASS` is harmless;
- when configured, registration cannot complete until `PASS` verifies;
- incorrect password returns numeric `464` and may be retried;
- password is never logged or persisted.

### `NICK <nickname>`

Purpose: select/change IRC nickname.

Acceptance criteria:
- nickname matches the strict ASCII nickname regexp and is <=31 characters;
- nickname is case-insensitively unique among connected clients;
- a nick present in the registered-identity table cannot be selected unless `IDENTIFY` for that nick succeeded on the same connection;
- changing nick after registration broadcasts `NICK` to users sharing a channel;
- changing nick does not change IRC operator status or channel membership.

### `USER <username> <mode> * :<realname>`

Purpose: complete basic IRC registration metadata.

Acceptance criteria:
- accepted only before registration;
- username is strict ASCII, 1..32 characters;
- mode is decimal and ignored in v0.1;
- realname is printable UTF-8, <=80 code points;
- server username bans and guard-mode username rules are checked when `USER` is received;
- registration completes only when valid `NICK`, `USER`, and optional server `PASS` requirements are satisfied.

### `PING [:]<token>` / `PONG [:]<token>`

Purpose: basic connection liveness interoperability.

Acceptance criteria:
- token is bounded safe ASCII;
- valid `PING` gets a server `PONG` carrying the same token;
- valid `PONG` is accepted and otherwise ignored in v0.1;
- no autonomous idle-ping scheduler is required in this prototype.

### `QUIT [:<reason>]`

Purpose: disconnect cleanly.

Acceptance criteria:
- optional reason is printable UTF-8, <=160 code points;
- connection closes and shared-channel peers receive one `QUIT` notification;
- all RAM membership/invite state for the connection is removed.

### `JOIN <#channel> [key]`

Purpose: join a predefined channel.

Acceptance criteria:
- exactly one channel per command;
- unknown channel is not created and returns a server `NOTICE` saying the channel does not exist;
- channel bans, invite-only mode, key, and limit are checked;
- successful join broadcasts `JOIN`, returns topic state and `NAMES`;
- successful join also emits a small IRCapybara extension-awareness `NOTICE`;
- IRC operators may bypass invite-only/key/limit, but not server/channel bans;
- no IRC `+v` voice status exists.

### `PART <#channel> [:<reason>]`

Purpose: leave a channel without disconnecting.

Acceptance criteria:
- client must currently be a member;
- optional reason is printable UTF-8, <=160 code points;
- `PART` is broadcast to all members including the leaving client;
- only RAM membership state changes.

### `PRIVMSG <target> :<text>`

Purpose: normal user/channel conversation.

Acceptance criteria:
- target is exactly one valid nick or predefined channel;
- text is 1..400 printable UTF-8 code points and contains no IRC/terminal control characters;
- channel sender must be a member;
- moderated channels allow only channel operators or IRC operators to send;
- direct messages require a connected registered target nick;
- sender does not receive a server echo in v0.1;
- plugin `BeforeMessage` hooks run before delivery, and plugin output is revalidated before use.

### `NOTICE <target> :<text>`

Purpose: informational message with the IRC convention that it should not provoke automatic error/reply loops.

Acceptance criteria:
- same target/text bounds as `PRIVMSG`;
- delivered similarly to `PRIVMSG`;
- target/member/mode failures are silently dropped rather than answered with IRC errors;
- malformed `NOTICE` arguments are ignored after the strict envelope boundary.

### `TOPIC <#channel> [:<topic>]`

Purpose: inspect or change channel topic metadata.

Acceptance criteria:
- without `:<topic>`, returns current topic or “no topic” numeric;
- with `:<topic>`, topic may be set or cleared;
- topic is printable UTF-8, <=180 code points;
- default channel mode is `+t`, therefore only a channel operator or IRC operator may change it;
- `MODE #channel -t` allows any channel member to change topic;
- successful topic changes are persisted immediately because topic is durable configuration.

### `NAMES <#channel>`

Purpose: list current members.

Acceptance criteria:
- exactly one existing channel;
- returns `353` chunks followed by `366`;
- channel operators are prefixed with `@`;
- output is chunked to remain under the IRC line limit.

### `WHO <#channel|mask>`

Purpose: lightweight visible-user query.

Acceptance criteria:
- channel form returns members of exactly that channel;
- mask form accepts only the bounded wildcard search regexp;
- `*` and `?` are converted to an anchored regular expression after the mask itself has passed validation;
- mask is matched case-insensitively against nick, username, realname, and IP string;
- results use `352`, followed by `315`.

### `WHOIS <nickname>`

Purpose: detailed information about one connected user.

Acceptance criteria:
- exactly one strict nickname;
- returns username, IP-as-host field, realname, channel memberships, and IRC-operator marker when applicable;
- missing user returns `401`;
- response terminates with `318`.

### `LIST`

Purpose: list the predefined channel catalog.

Acceptance criteria:
- takes no parameters in v0.1;
- lists every configured/persisted channel, current member count, and topic;
- unknown/empty channels still appear because channel existence is independent of membership.

### `MODE <#channel> [<mode> [parameter]]`

Purpose: query/change bounded channel state.

Acceptance criteria:
- one channel and at most one mode change per command;
- query returns `324`;
- mutating modes require channel-op or IRC-op authority;
- supported modes are `+/-o`, `+/-b`, `+/-i`, `+/-k`, `+/-l`, `+/-m`, `+/-t`;
- `+o/-o` affects channel membership authority only;
- `+b/-b` accepts only `ip:<addr>`, `cidr:<range>`, or `user:<username>` and is durable in SQLite;
- `+b` without a mask lists bans;
- `+i`, `+m`, `+t`, `-i`, `-m`, `-t`, `-k`, `-l` reject unexpected parameters;
- `+k` requires a bounded ASCII key;
- `+l` requires a positive bounded integer;
- `+v` is deliberately unsupported.

### `KICK <#channel> <nick> [:<reason>]`

Purpose: remove a member from a channel.

Acceptance criteria:
- requester must be channel operator or IRC operator;
- target must currently be in the channel;
- optional reason is printable UTF-8, <=160 code points;
- `KICK` is sent to all current members including the target before its membership is removed.

### `INVITE <nick> <#channel>` — low priority

Purpose: one-time admission into an invite-only channel.

Acceptance criteria:
- implemented but treated as low priority;
- requester must be channel operator or IRC operator;
- target must be connected and not already a member;
- invite exists only in RAM and is consumed by successful `JOIN`;
- no persistence or elaborate invite-notify semantics.

### `OPER <oper-name> <password>`

Purpose: attach server-wide operator authority to the current connection.

Acceptance criteria:
- requester must already be a registered IRC user;
- credentials match the configured operator identity;
- success sets per-connection IRC-op state and emits numeric `381` plus user mode `+o` indication;
- operator privilege survives nick changes and channel joins but disappears on disconnect;
- operator password is never logged or persisted.

### `KILL <nick> [:<reason>]`

Purpose: force-disconnect another client.

Acceptance criteria:
- IRC-operator only;
- exact connected target nick required;
- target receives `KILL`/`ERROR`, is removed from RAM state, and its socket is closed;
- peers receive a resulting `QUIT`.

## 5. IRCapybara custom/support commands

### `REGISTER <password>`

Claims the current nick permanently. Requires a fully registered, captcha-cleared connection. Password is 8..128 printable non-space ASCII characters. A nick can be registered only once. The derived hash/salt is persisted; plaintext is discarded.

### `IDENTIFY <nick> <password>`

Authenticates a registered nick on the current connection. It may be sent before `NICK`; a protected nick is rejected by `NICK` until matching `IDENTIFY` succeeds. This is deliberately much smaller than SASL/services.

### `CHANADD <#channel> :<topic>`

IRC-operator only. Creates a new allowed channel and persists it. This is the only runtime channel-creation path in v0.1.

### `GUARD <0|1|2>`

IRC-operator only. Runtime activation snapshots currently registered clients.

- `0`: off;
- `1`: reconnect allowed when either source IP or IRC username was present in the snapshot;
- `2`: reconnect allowed only from snapshot IPs.

Level 2 can reject at TCP accept time. Level 1 may provisionally accept a new IP until its `USER` username is known. Startup guard mode requires explicit environment allowlists to prevent accidental total lockout.

### `BANADD <SERVER|#channel> <IP|CIDR|USER> <value>`
### `BANDEL <SERVER|#channel> <IP|CIDR|USER> <value>`
### `BANLIST <SERVER|#channel>`

IRC-operator durable ban administration. Server IP/CIDR bans can reject during accept; username bans apply when `USER` is known. Adding a server ban immediately disconnects matching current clients. Channel bans are also manageable through `MODE +b/-b`.

### `CAPTCHA <challenge-id> <answer>`

When captcha is enabled, every newly registered TCP connection receives one simple addition challenge. Until solved, every command except `CAPTCHA`, `PING`, `PONG`, and `QUIT` is silently ignored. The challenge is RAM-only and not intended as strong bot proof; it is a cheap friction control.

### `XMODE PLAIN|EXTENDED`

Per-connection extension preference. `EXTENDED` is advertised on channel join, but v0.1 remains payload-compatible/passthrough because the future rendering format is intentionally undefined. This creates a stable negotiation hook without prematurely defining markup.

## 6. Compatibility shims

`CAP LS [302]` / `CAP END` are accepted so modern clients that probe IRCv3 do not fail registration; v0.1 advertises no IRCv3 capability set. `MOTD` is also provided because many regular clients expect it.

## 7. Protection behavior

- maximum total clients and maximum connections per source IP;
- token-bucket command rate limit per connection;
- input line size bound before parsing;
- strict UTF-8 and regexp parsing;
- server IP, CIDR, and username bans;
- channel IP, CIDR, and username bans;
- guard levels 0/1/2;
- optional per-connection math captcha;
- no terminal/CTCP control bytes in relayed text.

## 8. Plugin entry points

Compile-time Go plugins implement `pkg/pluginapi.Plugin` with hooks for:

- connect;
- disconnect;
- before command;
- before message (may transform or deny);
- join.

Plugins are trusted server code. Any message target/text returned by a plugin is nevertheless revalidated before network delivery. The built-in registry is `internal/plugins.Builtins()`.

## 9. Future-awareness, not v0.1 behavior

The architecture must not prevent later addition of:

- bounded server-side history;
- end-to-end-encryption overlay metadata;
- file transfer;
- negotiated extended rendering;
- server-side bot/agent plugins;
- bridges to other messengers;
- images, GIFs, stickers, and voice-message blobs carried through file-transfer/extended-rendering facilities;
- terminal-friendly binary/data transport;
- a possible SSH overlay for transport/data transfer, explicitly not remote command execution by IRC itself.

No v0.1 code may pretend these formats are already defined.
