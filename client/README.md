# IRCapybara Client

The IRCapybara client for desktop (Go + Wails) and Android (the same Go core through gomobile, in a thin Kotlin web-view wrapper), sharing one dependency-light Vue frontend. The UI follows the supplied Slack/Discord/Telegram-inspired direction while keeping the IRC layer completely behind friendly actions.

The frontend does not use npm, a bundler, or a component framework. Vue 3.5.13 is vendored as one local production JS file and everything else is plain HTML/CSS/JS. The Go core uses only the standard library; Wails is the desktop shell dependency.

## Architecture

The protocol boundary is deliberately kept away from rendering and from server implementation details:

```text
main.go / app.go / desktop_host.go   desktop shell: Wails window, dialogs, notifications
mobile/                              Android binding: gomobile-friendly API over the core
android/                             Android shell: Kotlin web view, pickers, service
frontend/                            rendering and interaction only, shared by both shells
internal/core/                       the client itself: session orchestration and the frontend API
internal/transport/              TLS socket, bounded large-line reader/writer
internal/protocol/               generic IRCv3 parser
internal/session/                connection/auth/captcha state only
internal/conversation/           conversations, messages, topics, member metadata
internal/xfer/                   upload/download framing, SHA-256 verification
internal/persist/                non-secret local settings
```

No server package is imported and no server-internal type is referenced. The wire protocol is the boundary.

### One core, two shells

`internal/core` contains everything that used to live in the Wails `App`: connection lifecycle, capability negotiation, message handling, transfers. It has no UI toolkit dependency. What it needs from the platform is the `core.Host` interface (emit an event to the frontend, pick a file, choose where a download goes, show a notification), and each shell implements that:

- Desktop: `desktop_host.go` implements `core.Host` with the Wails runtime. `App` embeds `*core.App`, so every exported core method is bound as `window.go.main.App.*`.
- Android: `mobile/` wraps the core in a `Bridge` whose API uses only types gomobile can carry. `Bridge.Invoke(method, argsJSON)` dispatches to the same exported core methods by name, so a method added to the core is available to both shells without further glue. The Kotlin `AndroidHost` implements the platform side.

Every exported method on `core.App` is therefore part of the frontend API. Keep helpers unexported.

The frontend reaches Go only through `frontend/dist/bridge.js` (`IRCapybaraNative.invoke`, `.on`, and a few platform services), which detects the shell at load time. `app.js` contains no Wails- or Android-specific calls.

## Implemented behavior

- TLS-only connection flow with `CAP LS 302`, optional `PASS`, `NICK`, `USER`, capability request, and `CAP END`.
- Explicit session states for disconnected, connection/TLS, CAP negotiation, IRC registration, captcha, identity read-only, ready, and reserved server-guard read-only state.
- Captcha challenge UI and auto-join gating until registration/challenge completion.
- Persistent nickname identification UI, including the optional explicit nickname form.
- Public/private channel conversation kinds, normal DMs, virtual/plugin DMs, plugin-capable conversation model, and server/status messages.
- Synthetic senders are rendered as services and are never assumed to be WHOIS-able.
- 32,768-code-point message limit with IRCv3 `batch` / `draft/multiline` support and a private line-separator fallback for older IRCapybara servers.
- A small, dependency-free Markdown renderer with safe links and optional code syntax highlighting; see below for the supported subset.
- IRCv3 tags, prefixes, numerics, trailing parameters, unknown tags, unknown numerics, and unknown commands are parsed conservatively.
- `server-time` and `ircapybara-history=1` are understood; historical plugin messages do not increment frontend unread counters.
- Dynamic `HELP` request UI with 704/705/706 and 524 handling.
- `AIRESET` exposed as a friendly Reset AI Conversation action.
- Attachment upload/download over `XFER`/`XATTACH`, using the server-provided upload chunk size, sequential download parts, declared-size enforcement, filename sanitisation, and SHA-256 verification.
- Downloads only start after an explicit user action and files are never automatically opened or executed.
- Light, dark, and system theme modes.
- No raw IRC command entry in the normal composer.

### Multiline messages

When the server offers both `batch` and `draft/multiline` with usable `max-bytes` and `max-lines` values, the client sends one logical message as a `draft/multiline` batch. Otherwise, it uses the legacy IRCapybara U+2028 line-separator encoding when supported by the peer. Message size is limited to 32,768 Unicode code points. See [the server protocol specification](../server/SPEC.md#7-ircv3-compatibility-and-multiline-messages) for server-side limits and validation.

### Markdown and code blocks

The shared desktop and Android renderer supports paragraphs, headings (`#` through `######`), fenced code blocks, block quotes, flat ordered and unordered lists, emphasis, strong emphasis, backslash escapes, inline code, and links. A single newline within a paragraph is displayed as a line break. Unmatched or unsupported markup remains visible as text. This is a small Markdown subset, not a complete CommonMark implementation; the message Copy action copies the original text, including Markdown markers.

Only absolute HTTP and HTTPS destinations become explicit Markdown links. Prose URLs and recognized nicknames keep the usual link and mention behavior outside code and explicit links. Raw HTML is rendered as text and is never inserted as HTML.

Fenced code highlighting recognizes Go (`go`, `golang`), JavaScript (`js`, `javascript`, `jsx`), TypeScript (`ts`, `typescript`, `tsx`), Python (`py`, `python`), shell (`sh`, `shell`, `bash`, `zsh`), and JSON (`json`). Unknown or unlabeled code remains plain monospace. Highlighting uses CSS Custom Highlights when available and text-only spans otherwise; it is a lightweight lexer, not a full syntax parser. It requires no package or network access.

Markdown parsing is bounded to 65,536 JavaScript code units, 1,024 lines, 4,096 syntax nodes, and 16 nested inline formatting spans. Code highlighting is bounded to 65,536 code units and 4,096 tokens per block. Inputs over the Markdown limits are displayed as plain text.

The code deliberately does not invent future syntax for richer rendering, message IDs, E2EE, reactions, replies, resumable transfers, or future transports. Those can be added behind explicit negotiated capabilities later.

## Android

See [android/README.md](android/README.md) for building the app. In short: `android/build-core.sh` compiles `mobile/` into `android/app/libs/ircapybara.aar`, and the Gradle project packages it with the files in `frontend/dist/`.

Below 720 px the frontend switches to a phone layout: the conversation list and the open conversation are separate full-screen panes with a slide transition. Touch behaviour lives in `frontend/dist/gestures.js` and the `touch-action` rules in `styles.css`:

- swipe right on a conversation, or the system back gesture, returns to the list;
- dialogs are bottom sheets that can be dragged down by their header to dismiss;
- press and hold a message for actions (copy, copy link, mention, message directly); right-click does the same on desktop;
- a jump-to-latest button appears when scrolled up and counts messages that arrive meanwhile;
- opening the keyboard keeps the newest message in view.

## Build (desktop)

Use Go 1.27 or newer and the [Wails v2 platform prerequisites](https://v2.wails.io/docs/gettingstarted/installation/).

```bash
go install github.com/wailsapp/wails/v2/cmd/wails@v2.15.0
wails build
```

On Linux with WebKitGTK 4.1, install the GTK 3 and WebKitGTK 4.1 development packages and run `wails build -tags webkit2_41`. The Windows build uses WebView2. The macOS release supports Intel and Apple silicon and requires macOS 13 or later with Go 1.27.

There is no `npm install` step.

For development, `wails dev` can be used. Because the frontend is raw static assets rather than a Vite project, frontend changes are picked up when Wails rebuilds/reloads the asset bundle.

## Checks

Core tests:

```bash
go test ./internal/...
```

Frontend syntax check:

```bash
node --check frontend/dist/app.js
```

`internal/core` is tested end to end against an in-process TLS IRC server (registration, messaging, remote and local disconnects, the connection probe, and an attachment download published through the host). `mobile` tests the by-name dispatch and the coalescing of state events.

The test suite covers tagged/unknown/large IRC parsing, bounded transport lines, queue-overflow behavior, conversation history bounds, filename sanitisation, and attachment hash verification.

## Security notes

All server/plugin/message content is treated as untrusted text. The Vue templates use text interpolation rather than HTML insertion. TLS certificate verification uses the operating-system trust store and there is no insecure-skip option. Passwords are never persisted. Queues and in-memory message history are bounded, attachment filenames are sanitised, and transfer contents must match both declared size and SHA-256 before becoming a completed download.

## Current scope

The core packages are pure Go; there is no `import "C"` or CGO code in IRCapybara itself. The Wails shell and the Android shell stay at the application boundary (both have platform-native build requirements of their own).

The plan for IRCv3 support, including what it changes in this client, is in [../docs/IRCV3.md](../docs/IRCV3.md).

## License

The IRCapybara client is licensed under [Creative Commons Attribution-NonCommercial-ShareAlike 4.0 International](LICENSE). See the [third-party notices](THIRD_PARTY_NOTICES.md) for the vendored Vue runtime.
