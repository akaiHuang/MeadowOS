import { Marked } from '../vendor/marked.js';
import purify from '../vendor/purify.js';

const allowedTags = [
  'p', 'br', 'h1', 'h2', 'h3', 'h4', 'h5', 'h6', 'strong', 'em', 'del',
  'a', 'ul', 'ol', 'li', 'blockquote', 'pre', 'code', 'hr', 'table', 'thead',
  'tbody', 'tr', 'th', 'td', 'sup', 'sub',
];

function escapeHTML(value) {
  return String(value).replace(/[&<>"']/g, char => ({
    '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;',
  })[char]);
}

export function safeAnswerURL(value) {
  try {
    // No base URL: relative links must never navigate an authenticated API.
    const url = new URL(value);
    if (!['https:', 'http:', 'mailto:'].includes(url.protocol)) return null;
    if (url.username || url.password) return null;
    return url.href;
  } catch { return null; }
}

// Marked normalizes CRLF before tokenizing. Keep offsets into the original
// source so copying an ordinary fenced block preserves its original bytes,
// including blank lines, CRLF and the newline before its closing fence.
function sourceOffsets(source) {
  if (!source.includes('\r')) return { text: source, at: index => index };
  const offsets = [];
  let text = '';
  for (let index = 0; index < source.length; index++) {
    offsets.push(index);
    if (source[index] === '\r') {
      text += '\n';
      if (source[index + 1] === '\n') index++;
    } else text += source[index];
  }
  offsets.push(source.length);
  return { text, at: index => offsets[index] };
}

function codeContent(token, source, offsets, sourceIndex) {
  const opening = /^( {0,3})(`{3,}|~{3,})[^\n]*(?:\n|$)/.exec(token.raw);
  if (!opening) return token.text;
  const start = opening[0].length;
  const closing = new RegExp(
    `^ {0,3}${opening[2][0]}{${opening[2].length},}[ \\t]*\\n*$`, 'm',
  ).exec(token.raw.slice(start));
  const end = closing ? start + closing.index : token.raw.length;
  // Nested lists/quotes remove their Markdown prefix before tokenization.
  // Their token contains the literal code with that structural prefix removed.
  let text = sourceIndex == null
    ? token.raw.slice(start, end)
    : source.slice(offsets.at(sourceIndex + start), offsets.at(sourceIndex + end));
  if (opening[1].length) {
    text = text.replace(new RegExp(`^ {1,${opening[1].length}}`, 'gm'), '');
  }
  return text;
}

async function copyText(document, text) {
  const view = document.defaultView;
  try {
    if (!view?.navigator.clipboard?.writeText) throw new Error('Clipboard unavailable');
    await view.navigator.clipboard.writeText(text);
    return;
  } catch {
    // Older WebViews can lack the Clipboard API. This fallback is confined to
    // this explicit copy click, uses a temporary field and restores selection.
    const active = document.activeElement;
    const selection = view?.getSelection();
    const ranges = selection
      ? Array.from({ length: selection.rangeCount }, (_, i) => selection.getRangeAt(i).cloneRange())
      : [];
    const field = document.createElement('textarea');
    field.value = text;
    field.readOnly = true;
    field.tabIndex = -1;
    field.setAttribute('aria-label', '複製程式碼');
    Object.assign(field.style, { position: 'fixed', top: '0', left: '-10000px', opacity: '0' });
    document.body.append(field);
    try {
      field.select();
      if (!document.execCommand?.('copy')) throw new Error('Clipboard unavailable');
    } finally {
      field.remove();
      if (active?.isConnected) active.focus({ preventScroll: true });
      if (selection) {
        selection.removeAllRanges();
        for (const range of ranges) selection.addRange(range);
      }
    }
  }
}

function copyButton(document, text) {
  const button = document.createElement('button');
  let copying = false;
  button.type = 'button';
  button.className = 'meadow-copy';
  button.textContent = '複製程式碼';
  button.setAttribute('aria-label', '複製程式碼');
  button.setAttribute('aria-live', 'polite');
  button.addEventListener('click', async () => {
    if (copying) return;
    copying = true;
    button.setAttribute('aria-busy', 'true');
    try {
      await copyText(document, text);
      button.textContent = '已複製';
    } catch { button.textContent = '複製失敗，請選取文字'; }
    finally {
      copying = false;
      button.removeAttribute('aria-busy');
    }
    setTimeout(() => { button.textContent = '複製程式碼'; }, 1800);
  });
  return button;
}

/**
 * Synchronously replace only an answer's presentation. The caller owns raw
 * model text, streaming state and file-link decoration. No fetches or text
 * conversion occur here; repeated calls cannot leave a stale async render.
 */
export function renderAnswer(element, rawText) {
  const source = rawText == null ? '' : String(rawText);
  const document = element.ownerDocument;
  const codeBlocks = [];
  const offsets = sourceOffsets(source);
  const topLevelCodeOffsets = new WeakMap();
  try {
    if (!purify.isSupported) throw new Error('Sanitizer unavailable');
    const markdown = new Marked({
      gfm: true,
      breaks: true,
      async: false,
      renderer: {
        // Raw HTML is shown as text. Only Markdown creates presentation nodes.
        html: token => escapeHTML(token.text),
        // Never load an image or other remote resource from a model answer.
        image: token => escapeHTML(token.text || ''),
        code: token => {
          const text = codeContent(token, source, offsets, topLevelCodeOffsets.get(token));
          codeBlocks.push({ text, language: (token.lang || '').split(/\s/)[0] });
          return `<pre><code>${escapeHTML(text)}</code></pre>`;
        },
      },
    });
    const tokens = markdown.lexer(source);
    let cursor = 0;
    for (const token of tokens) {
      const found = offsets.text.indexOf(token.raw, cursor);
      if (found < 0) continue;
      if (token.type === 'code') topLevelCodeOffsets.set(token, found);
      cursor = found + token.raw.length;
    }
    const fragment = purify.sanitize(markdown.parser(tokens), {
      RETURN_DOM_FRAGMENT: true,
      ALLOWED_TAGS: allowedTags,
      ALLOWED_ATTR: ['href', 'title', 'start'],
      ALLOW_DATA_ATTR: false,
      ALLOW_ARIA_ATTR: false,
    });
    for (const link of fragment.querySelectorAll('a')) {
      const href = safeAnswerURL(link.getAttribute('href'));
      if (href) {
        link.href = href;
        link.target = '_blank';
        link.rel = 'noopener noreferrer';
      } else link.removeAttribute('href');
    }
    [...fragment.querySelectorAll('pre')].forEach((pre, index) => {
      const literal = codeBlocks[index];
      const block = document.createElement('div');
      block.className = 'meadow-code-block';
      const bar = document.createElement('div');
      bar.className = 'meadow-code-bar';
      const label = document.createElement('span');
      label.textContent = literal?.language || '程式碼';
      bar.append(label, copyButton(document, literal?.text ?? pre.textContent));
      pre.tabIndex = 0;
      pre.setAttribute('aria-label', '程式碼，可左右捲動');
      pre.replaceWith(block);
      block.append(bar, pre);
    });
    for (const table of fragment.querySelectorAll('table')) {
      const scroller = document.createElement('div');
      scroller.className = 'meadow-table-scroll';
      scroller.tabIndex = 0;
      scroller.setAttribute('role', 'region');
      scroller.setAttribute('aria-label', '表格，可左右捲動');
      table.replaceWith(scroller);
      scroller.append(table);
    }
    const root = document.createElement('div');
    root.setAttribute('data-meadow-markdown', '');
    root.append(fragment);
    element.classList.add('is-rich-answer');
    element.replaceChildren(root);
  } catch {
    // A broken parser/sanitizer must not hide the answer or inject raw HTML.
    element.classList.remove('is-rich-answer');
    element.textContent = source;
  }
  return element;
}
