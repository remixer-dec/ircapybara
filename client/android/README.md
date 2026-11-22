# IRCapybara for Android

A thin Kotlin wrapper around two shared pieces: the Go client core (`../mobile`, compiled with gomobile) and the web frontend (`../frontend/dist`, packaged as assets and shown in a `WebView`). There is no Android-specific UI and no AndroidX dependency.

## Release APK

Pushing a tag builds the Android release APK alongside the desktop clients and server. Download `IRCapybara-client-android.apk` from the GitHub release’s **Assets**.

To publish an installable APK, configure these repository Actions secrets. Use the same signing key across releases so users can install updates over an existing copy.

- `ANDROID_KEYSTORE_BASE64` contains the release keystore encoded in Base64.
- `ANDROID_KEYSTORE_PASSWORD` unlocks the keystore.
- `ANDROID_KEY_ALIAS` selects the signing key.
- `ANDROID_KEY_PASSWORD` is needed only if the key has a different password from the keystore.

With no signing secrets, the workflow publishes `IRCapybara-client-android-unsigned.apk`, which must be signed before installation. See [Android’s command line signing guide](https://developer.android.com/build/building-cmdline#sign_cmdline).

## Build

Prerequisites: Go 1.27+, a JDK (17 or newer), and the Android SDK with an NDK installed and `ANDROID_HOME` set. Gradle 8.13 or newer; the Gradle wrapper is not checked in, so either open the folder in Android Studio or run `gradle wrapper` once.

```bash
./build-core.sh
```

```bash
gradle :app:assembleDebug
```

The first command installs `gomobile` and `gobind` at the versions pinned in `../go.mod` and writes `app/libs/ircapybara.aar`. Re-run it whenever Go code changes. Frontend changes need only the Gradle build.

Minimum Android version is 7.0 (API 24). Update Android System WebView to the newest version available for the device.

## How it fits together

```text
IRCapybaraApp      creates the Go Bridge once per process; watches network changes
MainActivity       WebView + the JS interface (window.IRCapybaraAndroid)
AndroidHost        implements the Go Host interface: pickers, downloads, notifications
ConnectionService  foreground service that exists while a server session does
```

- Page to Go: `bridge.js` calls `IRCapybaraAndroid.invoke(id, method, argsJson)`. The activity runs `Bridge.invoke` on a worker thread and answers with `window.__ircapybara.settle(id, ok, payload)`.
- Go to page: the core emits events through `AndroidHost.emit`, delivered as `window.__ircapybara.dispatch(event, payload)`. State snapshots are coalesced on the Go side so a busy channel cannot flood the web view.
- The page is served from the APK under `https://appassets.androidplatform.net/` by `shouldInterceptRequest`. Every other request from the web view is refused, and navigation away from that origin is handed to the system browser.

## Behaviour that differs from desktop

- Attachments: uploads come from the system document picker and are copied into the app cache for the duration of the transfer. Verified downloads move to `Downloads/IRCapybara` on Android 10 and later. On Android 7 through 9, **Open** asks where to save the file before showing the viewer chooser. No storage permission is needed.
- Certificate pinning: the chosen file is copied into app-private storage, because the core reads it by path.
- Background: while connected, a foreground service keeps the process and the socket alive. Swiping the app out of recents disconnects, because message notifications are decided by the page.
- Back: closes the open sheet, then returns to the chat list, then sends the app to the background (it does not finish the activity, so the page stays loaded).
- The core probes the server when the default network changes or the app returns to the foreground, and drops a connection that does not answer within 15 seconds.

## Known limitations

- Notification policy (which messages notify, unread counts) lives in the page. If Android destroys the activity while the service keeps the connection, messages arrive but do not notify until the app is opened. Moving that policy into the core is planned with read markers (see `../../docs/IRCV3.md`, phase 4).
- No automatic reconnect: passwords are never stored, so a dropped session needs the user to reconnect.
- Release builds have code shrinking off; the gomobile runtime needs keep rules before R8 can be enabled.
- The launcher icon is a placeholder.
