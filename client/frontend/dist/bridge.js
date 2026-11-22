(() => {
  'use strict';

  // One native bridge for every shell. app.js only talks to IRCapybaraNative and
  // never to Wails or the Android interface directly, so the same frontend runs
  // unchanged inside the desktop window and the mobile web view.
  //
  //   invoke(method, ...args) -> Promise   call a Go client-core method
  //   on(event, handler)      -> cancel()  subscribe to a Go client-core event
  //   openExternal(url)                    open a link outside the app
  //   setChrome({ dark })                  match system bars to the theme
  //   copyText(text)          -> Promise   put text on the clipboard
  //   haptic(kind)                         short tactile feedback, if any
  //   onBack(handler)                      handler() -> true when it consumed
  //                                        the platform's back gesture

  let backHandler = null;
  const common = {
    onBack(handler) { backHandler = typeof handler === 'function' ? handler : null; },
  };

  const android = window.IRCapybaraAndroid;

  if (android && typeof android.invoke === 'function') {
    const pending = new Map();
    const listeners = new Map();
    let seq = 0;

    // Entry points for the Kotlin shell, which reaches the page through
    // WebView.evaluateJavascript.
    window.__ircapybara = {
      settle(id, ok, payload) {
        const call = pending.get(id);
        if (!call) return;
        pending.delete(id);
        if (ok) call.resolve(payload);
        else call.reject(typeof payload === 'string' && payload ? payload : 'Something went wrong.');
      },
      dispatch(event, payload) {
        for (const handler of listeners.get(event) || []) {
          try { handler(payload); } catch (err) { console.error(err); }
        }
      },
      back() {
        try { return !!(backHandler && backHandler()); } catch (_) { return false; }
      },
    };

    window.IRCapybaraNative = {
      ...common,
      platform: 'android',
      available: () => true,
      invoke(method, ...args) {
        return new Promise((resolve, reject) => {
          const id = ++seq;
          pending.set(id, { resolve, reject });
          try {
            android.invoke(id, String(method), JSON.stringify(args));
          } catch (err) {
            pending.delete(id);
            reject(err);
          }
        });
      },
      on(event, handler) {
        if (!listeners.has(event)) listeners.set(event, new Set());
        listeners.get(event).add(handler);
        return () => listeners.get(event)?.delete(handler);
      },
      openExternal(url) { android.openExternal(String(url)); },
      setChrome(chrome) {
        if (typeof android.setChrome === 'function') android.setChrome(!!(chrome && chrome.dark));
      },
      copyText(text) {
        // The web clipboard API needs a user gesture the web view does not
        // always attribute to bridge callbacks; the shell has no such limit.
        if (typeof android.copyText === 'function') {
          android.copyText(String(text));
          return Promise.resolve();
        }
        return navigator.clipboard.writeText(String(text));
      },
      haptic(kind) {
        if (typeof android.haptic === 'function') android.haptic(String(kind || 'tap'));
      },
    };
    return;
  }

  const wailsApp = () => (window.go && window.go.main && window.go.main.App ? window.go.main.App : null);

  window.IRCapybaraNative = {
    ...common,
    platform: 'desktop',
    available: () => !!wailsApp(),
    invoke(method, ...args) {
      const api = wailsApp();
      if (!api || typeof api[method] !== 'function') {
        return Promise.reject(new Error('Desktop bridge unavailable. Run the project through Wails.'));
      }
      return api[method](...args);
    },
    on(event, handler) {
      const runtime = window.runtime;
      if (runtime && typeof runtime.EventsOn === 'function') return runtime.EventsOn(event, handler);
      return () => {};
    },
    openExternal(url) {
      if (window.runtime && typeof window.runtime.BrowserOpenURL === 'function') {
        window.runtime.BrowserOpenURL(url);
        return;
      }
      window.open(url, '_blank', 'noopener,noreferrer');
    },
    setChrome() {},
    async copyText(text) {
      const runtime = window.runtime;
      if (typeof runtime?.ClipboardSetText === 'function') {
        if (!await runtime.ClipboardSetText(String(text))) throw new Error('Clipboard unavailable');
        return;
      }
      await navigator.clipboard.writeText(String(text));
    },
    haptic() {},
  };
})();
