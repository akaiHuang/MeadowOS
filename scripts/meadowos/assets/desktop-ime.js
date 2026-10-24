// Meadow desktop text ingress. The vendor touch-keyboard path is unchanged.
import Keyboard from '../core/input/keyboard.js';
import keysyms from '../core/input/keysymdef.js';
import { getKeysym } from '../core/input/util.js';
import { isTouchDevice, isMac } from '../core/util/browser.js';
import { createNativePreedit } from './desktop-ime-native.js';

export function commitText(rfb, text) {
    for (const character of text.replace(/\r\n/g, '\n')) {
        const point = character.codePointAt(0);
        rfb.sendKey(point === 10 || point === 13 ? 0xff0d :
                    point === 9 ? 0xff09 : keysyms.lookup(point));
    }
}

export function installDesktopIME(rfb, container) {
    if (isTouchDevice || !container) return;
    const controller = new AbortController();
    const options = { signal: controller.signal };
    const input = document.createElement('textarea');
    input.setAttribute('aria-label', '遠端桌面文字輸入');
    input.setAttribute('autocomplete', 'off');
    input.setAttribute('autocorrect', 'off');
    input.setAttribute('autocapitalize', 'off');
    input.spellcheck = false;
    input.tabIndex = -1;
    // This is also the host IME's real preedit/candidate anchor. Keep it
    // unobtrusive between edits, but paint it while the host is composing.
    input.rows = 1;
    input.wrap = 'off';
    input.style.cssText = 'position:fixed;box-sizing:border-box;width:1px;height:32px;left:20px;top:30px;opacity:0;pointer-events:none;padding:0;border:0;border-radius:0;outline:0;resize:none;overflow:hidden;z-index:2147483646;font:400 16px/22px system-ui,-apple-system,sans-serif;color:#183b34;caret-color:#11875e;background:transparent;box-shadow:none;';
    document.body.appendChild(input);
    let anchor = { x: 20, y: 30, canvas: null };
    let composing = false;
    let nativePreedit;
    const positionInput = () => {
        let { x, y } = anchor;
        if (anchor.canvas && anchor.canvas.isConnected) {
            const bounds = anchor.canvas.getBoundingClientRect();
            x = bounds.left + anchor.u * bounds.width;
            y = bounds.top + anchor.v * bounds.height;
        }
        const caret = nativePreedit?.geometry();
        if (caret) { x = caret.x; y = caret.y + caret.height / 2; }
        const width = composing ? Math.min(320, Math.max(1, innerWidth - 16)) : 1;
        const height = Math.min(32, Math.max(1, innerHeight - 16));
        input.style.width = width + 'px';
        input.style.height = height + 'px';
        input.style.left = Math.max(0, Math.min(innerWidth - width - 8, x)) + 'px';
        input.style.top = Math.max(0, Math.min(innerHeight - height - 8, y - height / 2)) + 'px';
    };
    const showPreedit = () => {
        input.style.opacity = nativePreedit?.presenting ? '0' : '1';
        positionInput();
    };
    const hidePreedit = () => {
        input.style.opacity = '0';
        positionInput();
    };
    nativePreedit = createNativePreedit(input, container, () => {
        if (composing) showPreedit();
        else positionInput();
    });
    window.addEventListener('resize', positionInput, options);
    input.addEventListener('blur', hidePreedit, options);
    window.addEventListener('blur', hidePreedit, options);
    const keyboard = new Keyboard(input);
    // A browser/remote input backend may deliver a chord as one key event
    // with modifier flags, without separate modifier-down events. Reconcile
    // those flags before the vendor handler; otherwise Ctrl+A becomes "a".
    const controls = new Set();
    const hostTextKeys = new Set();
    const compositionKeys = new Map();
    const submissionKeys = new Map();
    const remoteKeys = new Set();
    let syntheticControl = false;
    const macControlKeys = new Set(['a', 'c', 'x', 'v', 'z', 'y', 'f', 's', 'o', 'p', 'n',
        'enter', '+', '=', '-', '0']);
    const commandReleases = new Map();
    const physicalMeta = new Map();
    let metaUsed = false;
    let clipboardKeyAction = null;
    let clipboardKeyTimer = null;
    const clearClipboardKeyAction = () => {
        if (clipboardKeyTimer !== null) clearTimeout(clipboardKeyTimer);
        clipboardKeyAction = null;
        clipboardKeyTimer = null;
    };
    const noteClipboardKeyAction = action => {
        clearClipboardKeyAction();
        clipboardKeyAction = action;
        // A keyboard default can dispatch its ClipboardEvent in this same
        // interaction task. Never retain suppression across later menu edits
        // (macOS may omit the letter's keyup while Command is held).
        clipboardKeyTimer = setTimeout(clearClipboardKeyAction, 0);
    };
    const clearCommands = () => {
        commandReleases.clear();
        physicalMeta.clear();
        metaUsed = false;
    };
    const releaseSynthetic = () => {
        if (!syntheticControl) return;
        rfb.sendKey(0xffe3, 'ControlLeft', false);
        controls.delete('ControlLeft');
        syntheticControl = false;
    };
    keyboard.onkeyevent = (keysym, code, down, ...locks) => {
        if (down) remoteKeys.add(code);
        else remoteKeys.delete(code);
        if (code === 'ControlLeft' || code === 'ControlRight') {
            const alreadyDown = down && controls.has(code);
            if (down) controls.add(code);
            else controls.delete(code);
            if (code === 'ControlLeft') syntheticControl = false;
            // Windows AltGr detection may defer the real Control-down until
            // after flag reconciliation has already supplied the same key.
            if (alreadyDown) return;
        }
        if (down && controls.size > 0 && !remoteKeys.has('AltLeft') &&
                !remoteKeys.has('AltRight') && !remoteKeys.has('MetaLeft') &&
                !remoteKeys.has('MetaRight')) {
            if (code === 'KeyC' && (keysym === 99 || keysym === 67)) noteClipboardKeyAction('copy');
            if (code === 'KeyX' && (keysym === 120 || keysym === 88)) noteClipboardKeyAction('cut');
        }
        rfb.sendKey(keysym, code, down, ...locks);
    };
    const releaseVendorKeys = () => {
        keyboard.ungrab();
        releaseSynthetic();
        if (!disposed && !composing) keyboard.grab();
    };
    const chord = (event, control) => {
        const keysym = getKeysym(event);
        if (!keysym) return false;
        const owned = [];
        const press = (symbol, code) => {
            owned.push([symbol, code]);
            rfb.sendKey(symbol, code, true);
        };
        try {
            if (control) {
                if (controls.size === 0) press(0xffe3, 'ControlLeft');
            } else {
                const [code, symbol] = physicalMeta.entries().next().value || ['MetaLeft', 0xffeb];
                press(symbol, code);
            }
            // noVNC's macOS workaround can release Shift immediately. The
            // actual chord still needs its event's Shift state (redo/zoom).
            if (event.shiftKey && !remoteKeys.has('ShiftLeft') && !remoteKeys.has('ShiftRight')) {
                press(0xffe1, 'ShiftLeft');
            }
            press(keysym, event.code);
        } finally {
            for (const [symbol, code] of owned.reverse()) rfb.sendKey(symbol, code, false);
        }
        return true;
    };
    const routeMacCommand = event => {
        if (!isMac()) return false;
        if (event.type === 'keyup' && commandReleases.has(event.code)) {
            const hostDefault = commandReleases.get(event.code);
            commandReleases.delete(event.code);
            event.stopImmediatePropagation();
            if (!hostDefault) event.preventDefault();
            return true;
        }
        if (event.code === 'MetaLeft' || event.code === 'MetaRight') {
            if (event.type === 'keydown') {
                if (physicalMeta.size === 0) metaUsed = false;
                physicalMeta.set(event.code, event.code === 'MetaRight' ? 0xffec : 0xffeb);
            } else {
                const symbol = physicalMeta.get(event.code);
                physicalMeta.delete(event.code);
                if (symbol && physicalMeta.size === 0 && !metaUsed) {
                    rfb.sendKey(symbol, event.code, true);
                    rfb.sendKey(symbol, event.code, false);
                }
            }
            // Defer a physical Command until the next key identifies whether
            // it is native editing, a host action, or an OS Super shortcut.
            event.stopImmediatePropagation();
            return true;
        }
        if (!event.metaKey || event.type !== 'keydown') return false;
        const key = (event.key || '').toLowerCase();
        if (['shift', 'control', 'alt', 'altgraph', 'capslock'].includes(key)) return false;
        metaUsed = true;
        const hostReload = key === 'r' && !event.ctrlKey && !event.altKey;
        if (hostReload) {
            releaseVendorKeys();
            commandReleases.set(event.code, true);
            event.stopImmediatePropagation();
            return true; // Browser's Command+R / Command+Shift+R default.
        }
        const control = !event.ctrlKey && !event.altKey && macControlKeys.has(key);
        if (!chord(event, control)) return false;
        if (control && key === 'c') noteClipboardKeyAction('copy');
        if (control && key === 'x') noteClipboardKeyAction('cut');
        commandReleases.set(event.code, false);
        event.stopImmediatePropagation();
        event.preventDefault();
        return true;
    };
    const reconcileControl = event => {
        // A real IME begins with an ordinary printable keydown on some
        // browsers (not yet isComposing/229). Let the textarea's native edit
        // run: preventing that default would stop compositionstart itself.
        // Only input/compositionend may transmit text; the vendor keyboard
        // still handles navigation and explicit Ctrl/Command shortcuts.
        const key = event.key || '';
        const nativeText = !remoteKeys.has(event.code) && !event.ctrlKey && !event.metaKey &&
            ((!event.altKey && Array.from(key).length === 1) || key === 'Dead' ||
             (event.getModifierState?.('AltGraph') && Array.from(key).length === 1));
        const ime = composing || event.isComposing || event.keyCode === 229 || key === 'Process';
        if (event.type === 'keyup' &&
                ['MetaLeft', 'MetaRight', 'ControlLeft', 'ControlRight'].includes(event.code)) {
            for (const held of compositionKeys.values()) held.released = true;
        }
        let compositionOwned = compositionKeys.has(event.code);
        if (!ime && event.type === 'keydown' && !event.repeat &&
                compositionKeys.get(event.code)?.released) {
            compositionKeys.delete(event.code);
            compositionOwned = false;
        }
        if (event.type === 'keyup') submissionKeys.delete(event.code);
        for (const held of submissionKeys.values()) {
            if (event[held.modifier] === false) held.released = true;
        }
        // macOS may omit the letter keyup while Command is held. A later
        // press of that physical key starts a fresh owner; never consume its
        // release with an earlier Command chord's stale bookkeeping.
        if (!ime && event.type === 'keydown') commandReleases.delete(event.code);
        if (!ime && event.type === 'keyup' && commandReleases.has(event.code) && routeMacCommand(event)) return;
        const hostRelease = event.type === 'keyup' && hostTextKeys.has(event.code);
        if (hostRelease) commandReleases.delete(event.code);
        if (ime || compositionOwned || nativeText || hostRelease) {
            if (ime && event.type === 'keydown') compositionKeys.set(event.code, { released: false });
            if (event.type === 'keyup') compositionKeys.delete(event.code);
            if (event.type === 'keydown') hostTextKeys.add(event.code);
            else hostTextKeys.delete(event.code);
            // Active composition and ordinary text keep the browser's native
            // editing, preedit, selection and candidate-window default.
            event.stopImmediatePropagation();
            // Confirming a candidate can end composition while Enter is still
            // physically held. Its later repeats must not create a host newline
            // or become a guest submission before that same key is released.
            if (compositionOwned && !ime && event.type === 'keydown') event.preventDefault();
            if (event.ctrlKey === false) releaseSynthetic();
            return;
        }
        const submission = key === 'Enter' && !event.altKey &&
            ((event.ctrlKey && !event.metaKey) ||
             (isMac() && event.metaKey && !event.ctrlKey));
        if (event.type === 'keydown') {
            const held = submissionKeys.get(event.code);
            // macOS can omit Enter-up while Command is held. Once that
            // modifier ends, a fresh (non-repeat) down is a new physical press.
            // Actual held repeats remain owned even after modifier release.
            if (held?.released && !event.repeat) submissionKeys.delete(event.code);
            if (submissionKeys.has(event.code) || (submission && event.repeat)) {
                event.stopImmediatePropagation();
                event.preventDefault();
                return;
            }
            if (submission) submissionKeys.set(event.code,
                { modifier: event.ctrlKey ? 'ctrlKey' : 'metaKey', released: false });
        }
        // A held text key may become a shortcut after Ctrl/Command is pressed.
        // Transfer that new keydown and its eventual release back to noVNC.
        if (event.type === 'keydown') hostTextKeys.delete(event.code);
        // Finish the previous text commit before forwarding a new shortcut.
        // In particular it must not be sent while a new Ctrl is held down.
        if (event.type === 'keydown' && pending !== null) {
            flushPending();
            input.value = '';
        }
        if (event.ctrlKey === false) releaseSynthetic();
        else if (event.ctrlKey === true && controls.size === 0 &&
                event.code !== 'ControlLeft' && event.code !== 'ControlRight') {
            rfb.sendKey(0xffe3, 'ControlLeft', true);
            controls.add('ControlLeft');
            syntheticControl = true;
        }
        routeMacCommand(event);
    };
    input.addEventListener('keydown', reconcileControl, { ...options, capture: true });
    input.addEventListener('keyup', reconcileControl, { ...options, capture: true });
    const clearHostKeys = () => {
        hostTextKeys.clear();
        compositionKeys.clear();
        submissionKeys.clear();
        clearClipboardKeyAction();
        clearCommands();
        releaseVendorKeys();
    };
    input.addEventListener('blur', clearHostKeys, options);
    window.addEventListener('blur', clearHostKeys, options);
    input.addEventListener('blur', releaseSynthetic, options);
    window.addEventListener('blur', releaseSynthetic, options);
    keyboard.grab();
    let pending = null;
    let timer = null;
    let disposed = false;
    let lastComposition = null;
    const flushPending = () => {
        if (timer !== null) clearTimeout(timer);
        if (!disposed && pending) commitText(rfb, pending);
        lastComposition = pending;
        pending = null;
        timer = null;
    };
    const clipboardEdit = event => {
        // Some browser/native edit menus emit only Copy/Cut, without any
        // keydown. The bridge textarea is empty between text commits; its
        // selection belongs to the guest, so route that explicit edit there.
        // During host composition its preedit and clipboard remain host-owned.
        if (disposed || composing || document.activeElement !== input) return;
        event.preventDefault();
        if (clipboardKeyAction === event.type) return;
        releaseVendorKeys();
        if (pending !== null) {
            flushPending();
            input.value = '';
        }
        if (physicalMeta.size > 0) metaUsed = true;
        const key = event.type === 'copy' ? 'c' : 'x';
        chord({ key, code: key === 'c' ? 'KeyC' : 'KeyX', shiftKey: false }, true);
    };
    input.addEventListener('copy', clipboardEdit, options);
    input.addEventListener('cut', clipboardEdit, options);
    input.addEventListener('compositionstart', () => {
        flushPending();
        lastComposition = null;
        composing = true;
        for (const code of hostTextKeys) compositionKeys.set(code, { released: false });
        keyboard.ungrab(); // release held modifiers; preedit belongs to host IME
        releaseSynthetic();
        clearClipboardKeyAction();
        clearCommands();
        nativePreedit.begin();
        showPreedit();
    }, options);
    input.addEventListener('compositionend', event => {
        composing = false;
        nativePreedit.end();
        hidePreedit();
        pending = typeof event.data === 'string' ? event.data : input.value;
        keyboard.grab();
        // Safari and Chromium order the final input/compose events differently.
        // Commit once, after the same editing event sequence has settled.
        if (timer !== null) clearTimeout(timer);
        timer = setTimeout(() => {
            flushPending();
            input.value = '';
        }, 0);
    }, options);
    input.addEventListener('input', event => {
        if (composing || event.isComposing) { nativePreedit.update(); return; }
        const value = input.value;
        if (pending !== null) {
            // The final composition input may arrive after compositionend.
            // A different ordinary edit in this same turn must first flush
            // that commit, then transmit only its new suffix (not drop it).
            if (/Composition$/.test(event.inputType || '') || value === pending) return;
            const committed = pending;
            flushPending();
            const added = committed && value.startsWith(committed)
                ? value.slice(committed.length)
                : (typeof event.data === 'string' ? event.data : value);
            if (added) commitText(rfb, added);
            input.value = '';
            return;
        }
        // Some engines deliver insertFromComposition after compositionend.
        const duplicate = /Composition$/.test(event.inputType || '') &&
            lastComposition !== null && input.value === lastComposition;
        if (!duplicate && input.value) commitText(rfb, input.value);
        lastComposition = null;
        input.value = '';
    }, options);
    container.addEventListener('pointerdown', event => {
        if (event.pointerType === 'touch' || event.target.tagName !== 'CANVAS') return;
        rfb.focusOnClick = false;
        const bounds = event.target.getBoundingClientRect();
        anchor = { x: event.clientX, y: event.clientY, canvas: event.target,
                   u: bounds.width ? (event.clientX - bounds.left) / bounds.width : 0,
                   v: bounds.height ? (event.clientY - bounds.top) / bounds.height : 0 };
        positionInput();
        input.focus({ preventScroll: true });
    }, { ...options, capture: true });
    rfb.addEventListener('disconnect', () => {
        disposed = true;
        if (timer !== null) clearTimeout(timer);
        keyboard.ungrab();
        nativePreedit.dispose();
        releaseSynthetic();
        clearClipboardKeyAction();
        clearCommands();
        controller.abort();
        input.remove();
    }, { once: true });
}
