(() => {
  'use strict';

  // Touch gestures for the phone layout. They are driven by Pointer Events and
  // depend on the touch-action rules in styles.css: the conversation pane is
  // pan-y, so the browser scrolls vertically and hands horizontal movement to
  // this file; sheet headers are touch-action none, so vertical drags on them
  // arrive here instead of scrolling.
  //
  //   swipe right on the conversation pane   -> onSwipeBack(); the pane follows
  //                                            the finger, or the drawer that
  //                                            options.drawer() returns slides in
  //   drag a sheet down by its header        -> onDismissSheet()
  //   press and hold a [data-longpress] node -> onLongPress(node)
  //
  // Only touch pointers are handled, so mouse and pen behave as before.

  const SLOP = 10;
  const LONG_PRESS_MS = 480;

  function install(options) {
    const html = document.documentElement;
    let drag = null;
    let press = null;
    let swallowClickUntil = 0;
    let heldPointer = null;

    // A hold or a drag that this file acted on can still end in a browser
    // "click" when the finger lifts (a hold released just before the system's
    // own long-press timeout counts as a tap). That click would land on
    // whatever is under the finger now, typically the backdrop of the sheet
    // that just opened, and close it again. The click is dispatched right
    // after pointerup, so only that instant is guarded; a deliberate tap a
    // moment later must still work.
    function swallowReleaseClick() {
      swallowClickUntil = performance.now() + 80;
    }

    function onClickCapture(event) {
      if (performance.now() > swallowClickUntil) return;
      swallowClickUntil = 0;
      event.stopPropagation();
      event.preventDefault();
    }

    function cancelPress() {
      if (press) window.clearTimeout(press.timer);
      press = null;
    }

    function startPress(event, node) {
      cancelPress();
      const state = { x: event.clientX, y: event.clientY, id: event.pointerId, timer: 0 };
      state.timer = window.setTimeout(() => {
        if (press !== state) return;
        press = null;
        // A hold that became an action must not also start a drag.
        drag = null;
        heldPointer = state.id;
        options.onLongPress(node);
      }, LONG_PRESS_MS);
      press = state;
    }

    function onDown(event) {
      if (event.pointerType !== 'touch' || !event.isPrimary) return;
      const target = event.target instanceof Element ? event.target : null;
      if (!target || target.closest('.markdown code, .markdown-code-block')) return;

      const holdable = target.closest('[data-longpress]');
      if (holdable && !target.closest('button, a, input, textarea')) startPress(event, holdable);
      else cancelPress();

      if (drag) return;
      const base = { id: event.pointerId, x: event.clientX, y: event.clientY, t: event.timeStamp, active: false, delta: 0, lastX: event.clientX, lastY: event.clientY, lastT: event.timeStamp, velocity: 0 };

      const header = target.closest('.modal-header');
      const card = header && header.closest('.modal-card');
      if (card) {
        if (!target.closest('button, a, input') && options.canDismissSheet()) drag = { ...base, kind: 'sheet', el: card };
        return;
      }
      const pane = target.closest('.workspace');
      if (pane && !target.closest('input, textarea') && options.canSwipeBack()) {
        const drawer = options.drawer && options.drawer();
        drag = { ...base, kind: 'pane', el: drawer || pane, drawer: !!drawer };
      }
    }

    function onMove(event) {
      if (press && event.pointerId === press.id && Math.hypot(event.clientX - press.x, event.clientY - press.y) > SLOP) cancelPress();
      if (!drag || event.pointerId !== drag.id) return;

      const dx = event.clientX - drag.x;
      const dy = event.clientY - drag.y;
      const along = drag.kind === 'pane' ? dx : dy;
      const across = drag.kind === 'pane' ? dy : dx;

      if (!drag.active) {
        if (Math.abs(across) > SLOP && Math.abs(across) >= Math.abs(along)) { drag = null; return; }
        if (along < SLOP || along < Math.abs(across) * 1.5) return;
        drag.active = true;
        cancelPress();
        html.dataset.drag = drag.kind;
        try { drag.el.setPointerCapture(drag.id); } catch (_) {}
      }

      const dt = event.timeStamp - drag.lastT;
      if (dt > 0) {
        const step = drag.kind === 'pane' ? event.clientX - drag.lastX : event.clientY - drag.lastY;
        drag.velocity = drag.velocity * 0.4 + (step / dt) * 0.6;
      }
      drag.lastX = event.clientX;
      drag.lastY = event.clientY;
      drag.lastT = event.timeStamp;
      drag.delta = Math.max(0, along);
      if (drag.drawer) drag.el.style.transform = `translate3d(${Math.min(0, drag.delta - drag.el.offsetWidth)}px, 0, 0)`;
      else drag.el.style.transform = drag.kind === 'pane' ? `translate3d(${drag.delta}px, 0, 0)` : `translate3d(0, ${drag.delta}px, 0)`;
    }

    function finish(event, cancelled) {
      if (press && event.pointerId === press.id) cancelPress();
      if (heldPointer === event.pointerId) {
        heldPointer = null;
        if (!cancelled) swallowReleaseClick();
      }
      if (!drag || event.pointerId !== drag.id) return;
      const done = drag;
      drag = null;
      if (!done.active) return;
      try { done.el.releasePointerCapture(done.id); } catch (_) {}
      if (!cancelled) swallowReleaseClick();

      const extent = done.kind === 'pane' ? done.el.offsetWidth : done.el.offsetHeight;
      const far = done.delta > Math.min(extent * 0.33, 140);
      const flung = done.velocity > 0.45 && done.delta > 36;
      const commit = !cancelled && (far || flung);

      // Dropping the drag flag re-enables the CSS transitions, so releasing the
      // inline transform animates from where the finger left off.
      delete html.dataset.drag;
      if (done.kind === 'pane') {
        done.el.style.transform = '';
        if (commit) options.onSwipeBack();
        return;
      }
      if (commit) {
        // The sheet fades out where it was dropped; the node is discarded.
        options.onDismissSheet();
        return;
      }
      done.el.style.transition = 'transform .18s cubic-bezier(.2,.8,.2,1)';
      done.el.style.transform = '';
      window.setTimeout(() => { done.el.style.transition = ''; }, 220);
    }

    const onUp = event => finish(event, false);
    const onCancel = event => finish(event, true);
    const passive = { passive: true };
    document.addEventListener('pointerdown', onDown, passive);
    document.addEventListener('pointermove', onMove, passive);
    document.addEventListener('pointerup', onUp, passive);
    document.addEventListener('pointercancel', onCancel, passive);
    document.addEventListener('click', onClickCapture, true);

    return () => {
      cancelPress();
      delete html.dataset.drag;
      document.removeEventListener('pointerdown', onDown);
      document.removeEventListener('pointermove', onMove);
      document.removeEventListener('pointerup', onUp);
      document.removeEventListener('pointercancel', onCancel);
      document.removeEventListener('click', onClickCapture, true);
    };
  }

  window.IRCapybaraGestures = { install };
})();
