# IRCapybara

IRCapybara is a small, opinionated single-server IRC implementation in Go. It uses strict regexp-first parsing, UTF-8 text with ASCII identifiers, predefined/operator-created channels, RAM-first runtime state, bounded RAM history, and SQLite only for durable configuration/identity/security data.

See `SPEC.md` for protocol behavior and acceptance criteria.

## Build

The only direct non-standard-library dependency is `github.com/glebarez/go-sqlite`, used directly through `database/sql` with `CGO_ENABLED=0`.

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

The production Dockerfile is multi-stage. Its **runtime** image is `gcr.io/distroless/static-debian12:nonroot`; Go exists only in the build stage. `Dockerfile.debug` uses `golang:1.27-bookworm`.

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

For compatibility with clients that omit the trailing-parameter colon when the real name is one word, `USER alice 0 * Alice` is also accepted. The colon remains required when the real name contains spaces. `CAP LS` suspends registration until `CAP END`, as expected by modern IRC clients.

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

Use it on a later connection:

```text
IDENTIFY alice a-long-password
NICK alice
USER alice 0 * :Alice
```

## Channels

Channels are never implicitly created by `JOIN`. Operators add them:

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

## Basic server queries

```text
TIME
UPTIME
```

Many IRC clients expose these as `/time` and `/quote UPTIME` or equivalent.

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
- `IRCAPYBARA_INITIAL_CHANNELS` — e.g. `#general|General discussion;#dev|Development`; seeded channels are public/history-enabled;
- `IRCAPYBARA_GUARD_LEVEL` — `0`, `1`, `2`, or `3`;
- `IRCAPYBARA_GUARD_ALLOWED_IPS` / `IRCAPYBARA_GUARD_ALLOWED_USERS` — startup guard allowlists;
- `IRCAPYBARA_MAX_CLIENTS`, `IRCAPYBARA_MAX_PER_IP`;
- `IRCAPYBARA_RATE_PER_SECOND`, `IRCAPYBARA_RATE_BURST`.

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

Implement `pluginapi.Plugin`, link the plugin package into the build, and return an instance from `internal/plugins.Builtins()`. This keeps the production binary static and distroless-friendly while preserving extension points for future server-side bots/bridges.

## Prototype limitations

This is still a prototype rather than an audited production IRC daemon. It intentionally has no SASL/services, server network, persistent chat history, DCC/file-transfer implementation, IRCv3 history protocol, dynamic plugins, DNS/host cloaking, or autonomous idle-PING scheduler.
