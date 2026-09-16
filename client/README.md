# IRCapybara Desktop Client

A desktop-first IRCapybara client built with Go + Wails and a dependency-light Vue frontend. The UI follows the supplied Slack/Discord/Telegram-inspired direction while keeping the IRC layer completely behind friendly actions.

The frontend does not use npm, a bundler, or a component framework. Vue 3.5.13 is vendored as one local production JS file and everything else is plain HTML/CSS/JS. The Go core uses only the standard library; Wails is the desktop shell dependency.

## Architecture

The protocol boundary is deliberately kept away from rendering and from server implementation details:

```text
main.go / app.go                 Wails shell + UI orchestration
frontend/                        rendering and interaction only
internal/transport/              TLS socket, bounded large-line reader/writer
internal/protocol/               generic IRCv3 parser
internal/session/                connection/auth/captcha state only
internal/conversation/           conversations, messages, topics, member metadata
internal/xfer/                   upload/download framing, SHA-256 verification
internal/persist/                non-secret local settings
```

No server package is imported and no server-internal type is referenced. The wire protocol is the boundary.

## Implemented behavior

- TLS-only connection flow with `CAP LS 302`, optional `PASS`, `NICK`, `USER`, capability request, and `CAP END`.
- Explicit session states for disconnected, connection/TLS, CAP negotiation, IRC registration, captcha, identity read-only, ready, and reserved server-guard read-only state.
- Captcha challenge UI and auto-join gating until registration/challenge completion.
- Persistent nickname identification UI, including the optional explicit nickname form.
- Public/private channel conversation kinds, normal DMs, virtual/plugin DMs, plugin-capable conversation model, and server/status messages.
- Synthetic senders are rendered as services and are never assumed to be WHOIS-able.
- 32,768-code-point message limit without automatic splitting; transport accepts approximately 132 KiB IRC lines.
- IRCv3 tags, prefixes, numerics, trailing parameters, unknown tags, unknown numerics, and unknown commands are parsed conservatively.
- `server-time` and `ircapybara-history=1` are understood; historical plugin messages do not increment frontend unread counters.
- Dynamic `HELP` request UI with 704/705/706 and 524 handling.
- `AIRESET` exposed as a friendly Reset AI Conversation action.
- Attachment upload/download over `XFER`/`XATTACH`, using the server-provided upload chunk size, sequential download parts, declared-size enforcement, filename sanitisation, and SHA-256 verification.
- Downloads only start after an explicit user action and files are never automatically opened or executed.
- Light, dark, and system theme modes.
- No raw IRC command entry in the normal composer.

The code deliberately does not invent future syntax for richer rendering, message IDs, E2EE, reactions, replies, resumable transfers, or future transports. Those can be added behind explicit negotiated capabilities later.

## Build

Prerequisites: Go 1.25+ and Wails v2 tooling for the desktop platform.

```bash
go mod tidy
go install github.com/wailsapp/wails/v2/cmd/wails@v2.15.0
wails build
```

There is no `npm install` step.

For development, `wails dev` can be used. Because the frontend is raw static assets rather than a Vite project, frontend changes are picked up when Wails rebuilds/reloads the asset bundle.

## Checks

Core tests:

```bash
go test ./internal/...
```

Frontend syntax check:

```bash
node --check frontend/app.js
```

The test suite covers tagged/unknown/large IRC parsing, bounded transport lines, queue-overflow behavior, conversation history bounds, filename sanitisation, and attachment hash verification.

## Security notes

All server/plugin/message content is treated as untrusted text. The Vue templates use text interpolation rather than HTML insertion. TLS certificate verification uses the operating-system trust store and there is no insecure-skip option. Passwords are never persisted. Queues and in-memory message history are bounded, attachment filenames are sanitised, and transfer contents must match both declared size and SHA-256 before becoming a completed download.

## Current scope

This is the desktop implementation. The reusable protocol/session/conversation/transfer packages are pure Go and do not depend on Wails; there is no `import "C"` or CGO code in IRCapybara itself. The Wails-specific shell and native dialogs stay at the application boundary (Wails can still have platform-native build requirements on some operating systems). A later gomobile shell can reuse those core packages without importing the desktop renderer.
