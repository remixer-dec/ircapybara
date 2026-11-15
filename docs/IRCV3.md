# IRCv3 support: proposal

This is a plan for bringing IRCapybara (server and the shared desktop/mobile client core) in line with the [IRCv3 specifications](https://ircv3.net/irc/). It covers where the code stands today, which specifications are worth adopting and which are not, and the order to do the work in.

Spec status below is taken from ircv3.net as of November 2026. The site labels specifications only as stable (unmarked), work-in-progress / `[draft]`, or deprecated. Draft capabilities and tags carry a `draft/` prefix on the wire, and several client tags were recently promoted out of draft (`+reply`, `+typing`, `+channel-context`), so confirm the exact wire name against the spec page when implementing each item.

## Summary

- IRCapybara already speaks a sliver of IRCv3: `CAP LS 302`, `server-time`, and `UTF8ONLY`. Everything else in the list is missing on at least one side.
- The largest blocker is on the server: inbound lines that start with `@tags` or `:prefix` are rejected by the command envelope, so no client-to-server tag can work until the parser changes. That is the first piece of work, and most later phases depend on it.
- Three IRCapybara-specific mechanisms have a direct IRCv3 replacement and should migrate: `IDENTIFY` (to SASL), `HISTORY` plus `ircapybara/history` (to `draft/chathistory`), and the client's text matching for identity and read-only state (to `standard-replies`).
- One custom command collides with a draft spec: IRCapybara's `REGISTER <password>` has a different syntax from `draft/account-registration`'s `REGISTER <account> <email> <password>`.
- Now that there is a phone client next to the desktop one, the features that pay off most are the ones that make a reconnect lossless and keep two devices in step: `msgid`, `echo-message`, `draft/chathistory`, and `draft/read-marker`.
- Recommended order: foundations, then message identity, accounts, presence, history, rich messaging, and transport. Phases 0 to 2 are the minimum to call the server "IRCv3-capable" for third-party clients.

## Where the code is today

### Server (`server/`)

| Area | Current behaviour | Gap |
| --- | --- | --- |
| Line parsing | `envelopeRE` in `internal/protocol/parser.go` accepts only `COMMAND args`. A leading `@tags` or `:source` is a parse failure. | No client-to-server tags, so no `label`, `+typing`, `+reply`, `TAGMSG`, or client batches. |
| CAP | `LS [302]`, `LIST`, `REQ`, `END`. Advertises exactly `server-time` and `ircapybara/history`. Any unknown capability in a `REQ` is a `NAK`. | No capability values, no `CAP NEW`/`DEL` (`cap-notify`), no multi-line `LS`, no `-cap` removal handling worth the name. |
| Outbound tags | Built by hand in one place (`plugin_host.go`) for history replay: `@ircapybara-history=1;time=...`. | No shared "tags for this recipient" step; `server-time` is not applied to ordinary live messages uniformly. |
| ISUPPORT (005) | `UTF8ONLY CHANTYPES=# PREFIX=(o)@ CHANMODES=b,k,l,imtsH IRCAPYBARA=1 IRCAPYBARA-XFER=1` | Missing `NETWORK`, `CASEMAPPING`, `NICKLEN`, `CHANNELLEN`, `TOPICLEN`, `MONITOR`, `WHOX`, `BOT`, `CLIENTTAGDENY`, and a line-length token. |
| Accounts | Custom `REGISTER <password>` and `IDENTIFY [<nick>] <password>`, salted iterated HMAC-SHA-256 hashes in SQLite, optional IP trust. SASL is explicitly out of scope in `SPEC.md`. | No `sasl`, `account-notify`, `account-tag`, `extended-join`, or 900-908 numerics. |
| History | RAM only, 100 messages per channel (`history.go`), fetched with the custom `HISTORY <#channel> [limit]`. No message IDs. | No `msgid`, `batch`, `draft/chathistory`, or persistence. |
| Presence | `NAMES`, `WHO`, `WHOIS`. No `AWAY`. | No `away-notify`, `multi-prefix` (trivial: only `@` exists), `userhost-in-names`, `WHOX`, `MONITOR`, `chghost`, `setname`. |
| Errors | Numerics and free-text `NOTICE`s. | No `FAIL`/`WARN`/`NOTE` (`standard-replies`). |
| Echo | The sender never receives its own `PRIVMSG`. | No `echo-message`. |
| Transport | TLS only, on one port. | Already compatible with STS; no WebSocket listener. |
| Line length | 32,768 code points of text, about 131 KiB per line. | Far above IRCv3's 8191-byte tag budget plus 512-byte body. Needs to be advertised, and reconciled with `draft/multiline`. |

### Client core (`client/internal/`)

| Area | Current behaviour | Gap |
| --- | --- | --- |
| Parsing | `protocol.Parse` handles tags (with unescaping), source, and parameters generically. | Fine as is. No serializer for outgoing tags. |
| CAP | Sends `CAP LS 302`, collects multi-line `LS`, requests `server-time` and `ircapybara/history`, ends on `ACK` or `NAK`, with a 5 s fallback. | Capability values are discarded. `NEW`/`DEL` are ignored. No record of which capabilities are actually enabled. One `REQ` for everything, so a single `NAK` loses all of them. |
| Messages | `PRIVMSG` and `NOTICE` only. IDs are a local counter (`m-1`, `m-2`). Own messages are appended locally on send. | No `msgid`, `TAGMSG`, `BATCH`, or echo reconciliation. Replay is deduplicated by sender + time + text. |
| Identity | `IDENTIFY` after registration, then `applyServerPolicyFromText` guesses success, failure, and read-only state from English substrings. | Brittle and server-wording dependent. SASL numerics and `FAIL` codes remove the guessing. |
| Members | `NAMES` with `@` as the only tracked prefix; `JOIN`/`PART`/`QUIT`/`NICK`/`KICK` applied. | No account, away, or host tracking. |
| Synthetic senders | A message is "service" if the source user is `plugin` or the host is `server`. | A convention only IRCapybara's server follows; the `bot` tag is the standard signal. |

### Frontend (`client/frontend/dist/`)

Plain text with linkified URLs, per-conversation unread counters kept in the page, a "load recent history" button, and (new) long-press message actions. Nothing renders replies, reactions, typing, edits/redactions, or away state yet.

## What to adopt

"Side" is where the work lands: S = server, C = client core, U = frontend. Phase refers to the plan in the next section.

### Stable specifications

| Specification | Wire name | Decision | Side | Phase | Notes |
| --- | --- | --- | --- | --- | --- |
| Capability negotiation 3.2 | `CAP`, `LS 302` | Complete it | S C | 0 | Values, multi-line `LS`, `LIST`, per-capability `REQ` outcome. |
| cap-notify | `cap-notify` | Adopt | S C | 0 | Implicit with `LS 302`. Needed once plugins or SASL mechanisms can change at runtime. |
| Message tags | `message-tags`, `TAGMSG`, `CLIENTTAGDENY` | Adopt | S C | 0 | The parser change. Everything in phases 1, 4, and 5 depends on it. |
| standard-replies | `standard-replies`, `FAIL`/`WARN`/`NOTE` | Adopt | S C U | 0 | Replaces text matching in the client. |
| server-time | `server-time`, `time` | Finish | S | 0 | Apply to every message, not only replay. |
| Message IDs | `msgid` | Adopt | S C | 1 | No capability; sent to clients with `message-tags`. |
| echo-message | `echo-message` | Adopt | S C | 1 | Client stops appending its own messages locally. |
| batch | `batch` | Adopt | S C | 1 | Required by chathistory, labeled-response, multiline. |
| labeled-response | `labeled-response`, `label` | Adopt | S C | 1 | Lets the core match replies to requests instead of using timers. |
| SASL 3.1 / 3.2 | `sasl`, `AUTHENTICATE`, 900-908 | Adopt | S C U | 2 | `PLAIN` first, then `EXTERNAL` and `SCRAM-SHA-256`. |
| account-notify | `account-notify`, `ACCOUNT` | Adopt | S C | 2 | |
| account-tag | `account-tag`, `account` | Adopt | S C | 2 | |
| extended-join | `extended-join` | Adopt | S C | 2 | |
| away-notify | `away-notify`, `AWAY` | Adopt | S C U | 3 | Requires implementing `AWAY` first. |
| multi-prefix | `multi-prefix` | Adopt | S C | 3 | Trivial today (one prefix); do it so adding voice/halfop later is not a client break. |
| userhost-in-names | `userhost-in-names` | Adopt | S C | 3 | |
| WHOX | `WHOX` token, 354 | Adopt | S C | 3 | The standard way to learn accounts and away state for a whole channel. |
| MONITOR | `MONITOR` token, 730-734 | Adopt | S C U | 3 | Online state for direct-message partners. |
| extended-monitor | `extended-monitor` | Adopt | S C | 3 | Away/account/host changes for monitored nicks. |
| invite-notify | `invite-notify` | Adopt, low priority | S C | 3 | `INVITE` is already marked low priority in `SPEC.md`. |
| chghost | `chghost`, `CHGHOST` | Adopt, low priority | S C | 3 | Only matters if hosts can change (cloaking after login). |
| setname | `setname`, `SETNAME` | Adopt | S C U | 3 | The client already has a "display name" field that maps to realname. |
| no-implicit-names | `no-implicit-names` | Adopt | S C | 3 | Saves a `NAMES` burst per join on a phone; the member list is fetched on demand. |
| Bot mode | `BOT` token, `bot` tag, 335 | Adopt | S C U | 3 | Marks plugin senders. Replaces the `!plugin@server` heuristic. |
| account-extban | `ACCOUNTEXTBAN` token | Adapt | S | 3 | Bans are IRCapybara's own `BANADD` with a `USER` kind; expose the equivalent rather than classic extbans. |
| `+typing` | client tag | Adopt | S C U | 5 | Server only relays. |
| `+reply` | client tag | Adopt | S C U | 5 | Needs `msgid`. |
| `+channel-context` | client tag | Adopt | S C U | 5 | Useful for plugin replies sent privately about a channel. |
| STS | `sts` | Adopt | S C | 6 | The server is TLS-only already; advertising `sts=duration=...` costs little. The client must persist and honour the policy. |
| UTF8ONLY | `UTF8ONLY` token | Done | S | - | Already advertised. Add `FAIL * INVALID_UTF8` instead of dropping the line silently. |
| WebSocket | subprotocols `text.ircv3.net`, `binary.ircv3.net` | Adopt later | S C | 6 | Enables a browser build of the same frontend and a fallback where raw TLS ports are blocked. |
| WebIRC | `WEBIRC` | Only if needed | S | 6 | Required once a WebSocket gateway or reverse proxy sits in front, so bans and IP trust see the real address. |

### Draft specifications

Drafts can change. Implement them behind their `draft/` names, keep each one isolated behind its capability, and expect to follow renames.

| Specification | Wire name | Decision | Side | Phase | Notes |
| --- | --- | --- | --- | --- | --- |
| chathistory | `draft/chathistory`, `CHATHISTORY`, `chathistory` batch, `MSGREFTYPES` | Adopt | S C U | 4 | Replaces `HISTORY` and `ircapybara/history`. |
| event-playback | `draft/event-playback` | Adopt with chathistory | S C | 4 | Replays joins, parts, topic changes. |
| read-marker | `draft/read-marker`, `MARKREAD` | Adopt | S C U | 4 | Shared read position between desktop and phone. |
| account-registration | `draft/account-registration`, `REGISTER`, `VERIFY` | Adopt, resolve the collision | S C U | 2 | See "REGISTER" below. |
| message-redaction | `draft/message-redaction`, `REDACT` | Adopt | S C U | 5 | Needs `msgid` and stored history. |
| react | `+draft/react`, `+draft/unreact` | Adopt | S C U | 5 | Needs `msgid`. |
| multiline | `draft/multiline`, `draft/multiline-concat` | Adopt | S C U | 5 | Today a message cannot contain a newline at all. |
| client-initiated batch | (used by multiline) | Adopt as part of multiline | S C | 5 | |
| pre-away | `draft/pre-away` | Adopt | S C | 3 | A phone that reconnects in the background should not flip the user to "present". |
| extended-isupport | `draft/extended-isupport`, `ISUPPORT` | Optional | S C | 6 | Lets the client learn limits before registration. |
| metadata-2 | `draft/metadata-2`, `METADATA` | Later | S C U | 6 | Avatars, display names, status. The frontend's avatar and display-name concepts map onto its registered keys. |
| network icon | `ICON` token | Optional | S U | 6 | Cosmetic; one ISUPPORT token. |
| channel-rename | `draft/channel-rename`, `RENAME` | Optional | S C | 6 | Channels are created with the oper-only `CHANADD`; rename fits the same model. |
| oper-tag | `draft/oper-tag` | Skip for now | - | - | Little value on a single small server. |

### Not adopting

| Specification | Reason |
| --- | --- |
| STARTTLS (`tls`) | Deprecated by IRCv3, and IRCapybara has no plaintext listener to upgrade. |
| `netsplit` / `netjoin` batches | Single server by design; there are no splits. |
| Web push | Not an IRCv3 specification at present (only vendor extensions exist). See "Mobile" below. |

## Plan

Each phase leaves the server usable by both the IRCapybara client and ordinary IRC clients. Sizes are rough: S is a day or two, M about a week, L more than that.

### Phase 0: foundations (M)

Nothing here is visible to users, and nearly everything later depends on it.

Server:

1. Tag-aware inbound parser. Accept `['@' tags SPACE] [':' source SPACE] command params`. Parse and unescape tags, ignore a client-supplied source, and keep the existing per-command regular expressions for the remainder. Enforce the limits from the message-tags spec (4094 bytes of client tag data, tag key grammar, client-only tags start with `+`) and reject violations with `417` (`ERR_INPUTTOOLONG`). Keep the existing strictness everywhere else.
2. Capability registry. One table of `{name, value function, enabled-by-default, depends-on}` instead of the string constant and two loops in `commands.go`. `CAP LS 302` prints values and wraps with the `*` continuation; plain `CAP LS` prints names only. `REQ` supports `-name`, validates dependencies, and stays atomic.
3. `cap-notify`: `CAP NEW` / `CAP DEL` to clients that negotiated 302 or requested it, driven by registry changes.
4. One outbound path that decides tags per recipient. Today each call site formats its own line. Introduce a message value `{tags, source, command, params}` and a `sendTo(client, msg)` that drops tags the recipient did not negotiate (`time` needs `server-time`, `account` needs `account-tag`, client-only tags and `msgid` need `message-tags`, and so on). Every later phase adds tags through this path.
5. `server-time` on all messages through that path.
6. `standard-replies`: helpers for `FAIL`, `WARN`, `NOTE` with machine-readable codes. Define IRCapybara codes for the states the client currently guesses: `FAIL PRIVMSG IDENTIFY_REQUIRED`, `FAIL * GUARD_READ_ONLY`, `FAIL IDENTIFY INVALID_CREDENTIALS`, `FAIL XFER EXPIRED`, `FAIL XFER UNAUTHORIZED`. Send the existing numerics/notices as well for clients without the capability.
7. Accurate ISUPPORT: add `NETWORK`, `CASEMAPPING=ascii`, `NICKLEN=31`, `CHANNELLEN=49`, `TOPICLEN`, `MODES`, and a `LINELEN` token for the non-standard line limit.
8. Rename the vendor capability. IRCv3 vendor names are domain-based (`example.org/name`); `ircapybara/history` is not. Pick a domain and move vendor capabilities and tags under it, keeping the old names as aliases for one release.

Client core:

1. Capability state: offered (with values), requested, enabled. React to `ACK`, `NAK`, `NEW`, `DEL`, `LIST`. Request capabilities in dependency-ordered groups so one unsupported name cannot veto the rest.
2. A serializer for outgoing messages with tags (escaping is the inverse of the existing `unescapeTag`).
3. Handle `FAIL`/`WARN`/`NOTE`. Route IRCapybara's codes to the session state machine and keep `applyServerPolicyFromText` only as a fallback for servers without `standard-replies`.
4. Expose enabled capabilities in the session snapshot, so the frontend can show or hide features without knowing about the protocol.

Tests: extend `registration_test.go` and the core's fake-server tests for tagged input, capability dependency errors, and tag filtering per recipient. Start running [irctest](https://github.com/progval/irctest) against the server here and keep it in CI from this phase on; it has ready-made cases for almost every specification in this document.

### Phase 1: message identity (M)

Server:

1. `msgid`: assign an ID to every `PRIVMSG`, `NOTICE`, and `TAGMSG` at acceptance. Use a sortable, opaque value (a ULID fits) so history queries can order and page by it. Plugin-originated and attachment (`XATTACH`) messages get one too.
2. `echo-message`: deliver the sender's own message back through the same path, with the same `msgid` and `time`.
3. `batch`: `BATCH +ref type params` / `BATCH -ref` framing with the `batch` tag, used first for history replay.
4. `labeled-response`: copy the `label` tag onto a single-line reply, wrap multi-line replies in a `labeled-response` batch, and send `ACK` when a command produces no output. Apply it in the central dispatcher, not per command.

Client core:

1. `conversation.Message` gains `MsgID`; the store indexes messages by it. The local `m-N` counter stays only for messages without an ID (status lines, old servers).
2. With `echo-message`, stop appending on send. Show the outgoing message as pending, keyed by a `label`, and resolve it when the echo arrives. This removes the current duplicate risk on replay and gives a real "sent" state, which matters on a flaky mobile connection.
3. Batches: collect lines by reference and hand complete batches to a handler chosen by type. Unknown batch types are processed as individual messages, as the spec requires.
4. Use labels for `NAMES`, `LIST`, and history requests in place of the `historyPending` timer and its 1.5 s window.

Frontend: a pending/failed state for outgoing messages, and a retry action on failure.

### Phase 2: accounts (M to L)

Server:

1. `sasl` with `PLAIN`, backed by the existing identity table and password hashing. Advertise `sasl=PLAIN` (the value is the mechanism list). Numerics 900, 903, 904, 905, 906, 907, 908. Support re-authentication after registration (SASL 3.2). Apply the same rate limiting and IP rules as `IDENTIFY`.
2. `EXTERNAL` (TLS client certificate fingerprints stored per identity), then `SCRAM-SHA-256` (needs a SCRAM verifier stored alongside the current hash, created at the next successful login).
3. Decide how SASL interacts with the captcha and guard. Proposed rule: capability negotiation and SASL are allowed before the captcha, a successful SASL login for a known identity skips the captcha, and guard levels that block unauthenticated users let authenticated ones through.
4. `account-notify`, `account-tag`, `extended-join`, and the `a` field for WHOX. The account name is the identity's display nick.
5. Keep `IDENTIFY` as a thin wrapper that produces the same numerics and `ACCOUNT` notifications, so existing clients and habits keep working.
6. `draft/account-registration` and the `REGISTER` collision (below).

Client core and frontend:

1. Authenticate with SASL during capability negotiation when the user supplied an identity password; fall back to `IDENTIFY` when `sasl` is not offered.
2. Identity state comes from 900/903/904 and `ACCOUNT`, not from text. The "Identify" dialog stays, and calls SASL re-authentication when available.
3. Member entries carry an account name; show "logged in as" in the member list and use it to tell a registered user from someone merely using the nick.
4. A registration flow in the connect dialog when the server offers `draft/account-registration`.

#### REGISTER

IRCapybara's `REGISTER <password>` registers the current nick. The draft's form is `REGISTER <account> <email> <password>`, where `*` stands for "my current nick" and "no email". Options:

1. Switch to the draft syntax and accept the old one-argument form as a deprecated alias, distinguishable by argument count. Advertise `draft/account-registration=custom-account-name` only if arbitrary account names are allowed; otherwise fail with `ACCOUNT_NAME_MUST_BE_NICK`. No email verification, so `VERIFY` is never required.
2. Keep the custom command and never advertise the capability.

Option 1 is recommended. The alias costs a few lines and third-party clients gain a registration UI for free.

### Phase 3: presence and membership (M)

Server:

1. `AWAY` (with `301`, `305`, `306`), then `away-notify` and `draft/pre-away`.
2. `multi-prefix` and `userhost-in-names` in `NAMES` and `WHO` replies.
3. `WHOX`: `WHO <mask> %<fields>[,<token>]` with reply 354. Restrict to the fields that exist (`t`, `c`, `u`, `h`, `n`, `f`, `a`, `r`).
4. `MONITOR` with a per-client list limit advertised in ISUPPORT, then `extended-monitor`. Private-channel visibility rules must not leak through it: monitoring reveals only online state, which `WHOIS` already does.
5. `no-implicit-names`, `setname`, `invite-notify`, `chghost`.
6. Bot mode: user mode `+B`, `BOT=B` in ISUPPORT, the `bot` tag on messages, and 335 in `WHOIS`. Set it for every plugin-backed sender (mirrors, bridges, the AI responder, yt-dl).

Client core and frontend:

1. Member state: prefixes, account, away flag and message, host. Populate with one WHOX query on join instead of `NAMES`, when available.
2. Direct-message list shows online/away, driven by `MONITOR`.
3. "Service" badge from the `bot` tag, with the current heuristic as fallback.
4. An away toggle. On mobile, send `AWAY` when the app goes to the background and clear it on return, and use `draft/pre-away` on background reconnects.

### Phase 4: history and read state (L)

This is the phase with the most user-visible effect, particularly for the phone, and it needs a storage decision first.

Server:

1. Persistent message store. `SPEC.md` keeps history in RAM on purpose (low write volume). `draft/chathistory` works against RAM too, but then a restart or a long absence loses everything, which defeats the point on mobile. Proposed: an optional SQLite table `messages(target_norm, msgid, time, source, account, command, text, tags)` with a retention limit per channel (count and age), batched writes, and the existing per-channel `+H` flag deciding whether a channel is stored at all. RAM stays the default; persistence is opt-in by configuration.
2. Direct-message history, stored per account pair and only for authenticated users. This is new behaviour, so it should be an explicit setting.
3. `CHATHISTORY` subcommands `LATEST`, `BEFORE`, `AFTER`, `AROUND`, `BETWEEN`, and `TARGETS`, with `timestamp=` and `msgid=` references, the `chathistory` batch, and the capability value advertising the page limit. Reuse the membership and private-channel checks of the live path.
4. `draft/event-playback` for joins, parts, kicks, and topic changes (requires storing those events).
5. `draft/read-marker`: `MARKREAD <target> [timestamp=...]`, stored per account and target, sent to all of that account's sessions on change and on join.
6. Plugins: replace the hand-built `@ircapybara-history=1` replay in `plugin_host.go` with entries written to the same store, so plugin backlogs are served by `CHATHISTORY` like everything else. Keep the old tag and the `HISTORY` command for one release.

Client core:

1. On connect and on rejoin, request `CHATHISTORY LATEST <target> msgid=<last seen>` to fill exactly the gap, instead of taking whatever the server replays.
2. Page backwards with `BEFORE` when the user scrolls up.
3. `CHATHISTORY TARGETS` after a reconnect to discover direct messages received while away.
4. Deduplicate strictly by `msgid`. Remove `DedupeKey` and `sameHistoricalMessage`.
5. Read markers: send `MARKREAD` when a conversation is read, apply incoming ones.
6. A small local cache (the last N messages per conversation, on disk) so the phone shows content immediately on launch and asks the server only for the difference.

Frontend:

1. Unread counts move out of page-local state and into the core, derived from the read marker. This also fixes a current limitation of the mobile shell, where unread counts and notification decisions are lost if the page is reloaded.
2. Scroll-up pagination replaces the "load recent history" button. A "new messages" divider at the read marker.

### Phase 5: rich messaging (M to L)

All of this needs phases 0 and 1; redaction also needs phase 4.

Server:

1. Relay client-only tags on `PRIVMSG`, `NOTICE`, and `TAGMSG` to recipients with `message-tags`. Advertise `CLIENTTAGDENY` so an operator can block specific tags.
2. Rate-limit `TAGMSG` separately from messages: typing notifications are frequent and must not eat the message token bucket, but must not be free either.
3. `draft/message-redaction`: `REDACT <target> <msgid> [reason]`, allowed for the author within a time window and for channel operators. Redaction removes the text from the store and is replayed through history.
4. `draft/multiline`: client-initiated batch of `PRIVMSG` lines delivered as one message, with `max-bytes` and `max-lines` in the capability value. For recipients without the capability, deliver the lines individually as the spec describes. Decide the relation to the 32,768 code point limit: proposed `max-bytes` equal to today's limit, so a multiline message can be as long as a single line can be now.

Client core and frontend:

1. Replies: store `+reply`, render the quoted parent, jump to it. "Reply" joins the long-press message actions next to "Mention".
2. Reactions: aggregate `+draft/react` / `+draft/unreact` per message; a reaction picker in the message actions.
3. Typing: send `+typing=active|paused|done` from the composer with the throttling the spec asks for (at most one every 3 s); show "... is typing" under the message list; expire after 6 s (active) or 30 s (paused).
4. Redaction: "Delete" in the message actions for own messages; render a tombstone.
5. Multiline: the composer allows Shift+Enter (desktop) and a newline key (mobile); messages render with line breaks. Remove the "messages cannot contain raw newlines" rule when the capability is on.

### Phase 6: transport and extras (S each, independent)

1. STS. Server: advertise `sts=duration=<seconds>` (and `port=` if a plaintext port is ever added). Client: persist the policy per host and refuse the "allow untrusted certificates" option for hosts with an active policy, except when a pin is configured. Note the interaction with the current default, which allows self-signed certificates: a server must present a valid certificate before it can sensibly set a policy.
2. WebSocket listener with the `text.ircv3.net` subprotocol, plus `WEBIRC` if a proxy terminates it. The client core's `transport` package gets a second implementation behind the same interface. This is also the route to a pure browser build of the frontend.
3. `draft/extended-isupport`, `ICON`, `draft/channel-rename`.
4. `draft/metadata-2` for `avatar`, `display-name`, `status`, `pronouns`, `color`, `bot`, with subscriptions and sync limits. The frontend's hash-based capybara avatars remain the fallback when no avatar is set.

## IRCapybara extensions next to IRCv3

| Extension | Recommendation |
| --- | --- |
| `XMODE EXTENDED` | It is capability negotiation by another name. Replace with a vendor capability (`<domain>/xfer`, value = protocol version) requested during `CAP`, and keep `XMODE` as an alias. That also removes the `IRCAPYBARA-XFER` ISUPPORT round trip. |
| `XFER` / `RECV` / `XATTACH` | Keep. IRCv3 has no file transfer. Once `msgid` exists, send attachment announcements as a `PRIVMSG` carrying vendor tags (file ID, size, MIME type) so they are stored, replayed, replied to, and redacted like any other message, and third-party clients see at least a text line. |
| `CAPTCHA` | Keep. Report "required" and "failed" with `standard-replies` codes so the client does not parse wording. |
| `GUARD`, read-only sessions | Keep. Signal state with `FAIL`/`NOTE` codes (phase 0). |
| `HISTORY`, `ircapybara/history`, `ircapybara-history` tag | Replace with `draft/chathistory` (phase 4), keep as aliases for one release. |
| `IDENTIFY`, `REGISTER` | Wrap around SASL and `draft/account-registration` (phase 2). |
| `HELP`, `AIRESET`, `CHANADD`, `BAN*`, `UPTIME` | Keep; outside IRCv3's scope. Make their replies label-aware via the central dispatcher. |
| 32,768 code point lines | Keep, advertise through ISUPPORT, and align `draft/multiline`'s `max-bytes` with it. |

## Mobile

The phone client changes the priorities more than any other factor:

- Connections drop constantly. `msgid` plus `CHATHISTORY LATEST ... msgid=` makes a reconnect fetch exactly what was missed. Today a reconnect resets the conversation list and shows whatever the server chooses to replay.
- Two devices on one account need a shared read position (`draft/read-marker`) and a consistent view of sent messages (`echo-message`).
- Background delivery. IRCv3 has no push specification, so there are two choices: keep the socket open with the Android foreground service (what the wrapper does now), or add a vendor web-push extension later, modelled on the existing ones in the bouncer ecosystem, with the server sending a push when a direct message or mention arrives for an account with no live session. The first is free and costs battery; the second needs server-side storage of push subscriptions and a push provider. Recommended: stay with the foreground service until phase 4 is done, then reconsider, since push only helps once missed messages can be fetched reliably.
- Multiple sessions per account. SASL makes it natural for the phone and the desktop to be logged in at once, but IRC allows one connection per nick. Either the second session takes a different nick, or the server learns to attach several connections to one nick (bouncer-style). The latter is a significant server change and is not part of this plan; it is the main open design question below.

## Verification

- Run irctest against the server in CI from phase 0, enabling its test groups as each specification lands.
- Extend the client core's fake-server tests (`client/internal/core/app_test.go`) per phase: tagged input, batches, SASL exchanges, history paging, echo reconciliation.
- Check interoperability with clients that exercise these specifications heavily: Halloy and WeeChat on desktop, goguma on Android, gamja in the browser (once WebSocket exists). They should connect, authenticate with SASL, and page history without IRCapybara-specific knowledge.
- The protocol log in the client already shows the raw exchange; extend its redaction to `AUTHENTICATE` payloads before SASL ships.

## Decisions needed

1. A domain for vendor capability and tag names.
2. Whether message history may be persisted to disk, with what retention, and whether direct messages are included.
3. `REGISTER`: adopt the draft syntax with the old form as an alias (recommended), or keep the custom command.
4. Whether SASL login should bypass the captcha.
5. Several simultaneous sessions per account: separate nicks, or multi-attach on the server.
6. Whether a WebSocket listener, and with it a browser client, is wanted at all.
