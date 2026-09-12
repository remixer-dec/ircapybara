# IRCapybara

IRCapybara is a small, single-server IRC prototype in Go. It is intentionally opinionated: predefined channels, strict regexp parsing, UTF-8 text, no server network, no SASL/services, RAM-first runtime state, and SQLite only for durable identities/channels/bans.

See `SPEC.md` for protocol behavior and acceptance criteria.

## Build

The only direct non-standard-library dependency is `github.com/glebarez/go-sqlite`, used directly through `database/sql` with `CGO_ENABLED=0`.

```sh
go mod download
go test ./...
CGO_ENABLED=0 go build -o ircapybara ./cmd/ircapybara
```

The source archive intentionally does not vendor dependencies. Running `go mod download`/`go mod tidy` in a networked environment will materialize the module checksums.

## Docker

Production:

```sh
docker compose up --build ircapybara
```

Debug/toolchain environment:

```sh
docker compose --profile debug up --build ircapybara-debug
```

The production image uses `gcr.io/distroless/static-debian12:nonroot`. TLS is enabled by default and a self-signed certificate is generated under `/data/tls` if no certificate/key exists.

The example compose file exposes TCP/6697 and uses a persistent named volume for `/data`. Change the operator password before use.

## Minimal client flow

```text
CAP LS 302
NICK alice
USER alice 0 * :Alice
CAP END
JOIN #general
PRIVMSG #general :hello
```

If captcha is enabled, registration produces a `NOTICE` such as:

```text
CAPTCHA REQUIRED deadbeef: solve 7+4; reply exactly: CAPTCHA deadbeef <answer>
```

Then send:

```text
CAPTCHA deadbeef 11
```

If a nick has been protected:

```text
IDENTIFY alice your-password
NICK alice
USER alice 0 * :Alice
```

To protect the currently used nick for future connections:

```text
REGISTER a-long-password
```

## Operator flow

```text
OPER admin change-me-now
CHANADD #ops :Operations
BANADD SERVER IP 192.0.2.10
BANADD SERVER CIDR 198.51.100.0/24
BANADD #general USER noisyuser
GUARD 1
```

Channel bans may also use:

```text
MODE #general +b user:noisyuser
MODE #general +b ip:192.0.2.10
MODE #general +b cidr:198.51.100.0/24
```

## Configuration

Environment variables:

- `IRCAPYBARA_LISTEN_ADDR` — default `0.0.0.0:6697` in Compose;
- `IRCAPYBARA_SERVER_NAME` — default `ircapybara.local`;
- `IRCAPYBARA_DB_PATH` — default `/data/ircapybara.db`;
- `IRCAPYBARA_TLS_ENABLED` — `true|false`, default `true`;
- `IRCAPYBARA_TLS_AUTOGEN` — `true|false`, default `true`;
- `IRCAPYBARA_TLS_CERT_FILE` / `IRCAPYBARA_TLS_KEY_FILE`;
- `IRCAPYBARA_SERVER_PASSWORD` — optional shared `PASS` secret;
- `IRCAPYBARA_OPER_NAME` / `IRCAPYBARA_OPER_PASSWORD`;
- `IRCAPYBARA_CAPTCHA_ENABLED` — default `false`;
- `IRCAPYBARA_INITIAL_CHANNELS` — e.g. `#general|General discussion;#dev|Development`;
- `IRCAPYBARA_GUARD_LEVEL` — `0`, `1`, or `2`;
- `IRCAPYBARA_GUARD_ALLOWED_IPS` and `IRCAPYBARA_GUARD_ALLOWED_USERS` — startup guard allowlists;
- `IRCAPYBARA_MAX_CLIENTS`, `IRCAPYBARA_MAX_PER_IP`;
- `IRCAPYBARA_RATE_PER_SECOND`, `IRCAPYBARA_RATE_BURST`.

All configuration strings are also bounded and validated before use.

## Architecture

```text
cmd/ircapybara        process entry point
internal/config       strict environment configuration
internal/protocol     line/command regexps and UTF-8 policy
internal/server       TCP/TLS loop, IRC state, command handlers
internal/protection   bans + guard state
internal/security     password KDF/verification
internal/storage      direct database/sql SQLite persistence
pkg/pluginapi         public compile-time hook interface
internal/plugins      compile-time plugin registry
```

The server deliberately keeps live IRC state in memory. SQLite is loaded at startup; chat/messages/presence never write to disk. Durable writes occur only for nick registration, channel creation/topic mutation, and ban mutation.

## Plugin model

Implement `pluginapi.Plugin`, add the plugin package to the build, and return an instance from `internal/plugins.Builtins()`. This keeps the production binary static and compatible with the distroless deployment while leaving extension points for future server-side bots/bridges.

## Prototype limitations

This is an initial prototype, not an audited production IRC daemon. In particular: no autonomous idle PING scheduler, no IRCv3 feature set beyond tolerating `CAP`, no DNS/host cloaking, no service accounts/SASL, no history, and no dynamic plugins. Those omissions are deliberate unless/until IRCapybara itself needs them.
