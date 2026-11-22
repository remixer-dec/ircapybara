/* Shared chat Markdown. All user content reaches Vue as text or a checked URL. */
(() => {
  'use strict';

  const text = value => ({ type: 'text', value });
  const literal = value => ({ type: 'literal', value });
  const punctuation = /[\p{P}\p{S}]/u;
  const whitespace = ch => ch === '' || /\s/u.test(ch);
  const fence = /^ {0,3}(`{3,}|~{3,})[ \t]*([\w.+#-]*)[ \t]*$/;
  const heading = /^ {0,3}(#{1,6})(?:[ \t]+(.*)|$)/;
  const quote = /^ {0,3}>[ \t]?(.*)$/;
  const item = /^ {0,3}([-+*]|\d{1,9}[.)])[ \t]+(.*)$/;

  function safeURL(value) {
    if (!/^https?:\/\//i.test(value) || /[\s\\\u0000-\u001f\u007f]/.test(value)) return false;
    try { return Boolean(new URL(value).hostname); } catch { return false; }
  }

  function inline(source) {
    const nodes = [];
    const stack = [{ children: nodes }];
    const current = () => stack[stack.length - 1].children;
    // Index matching backtick runs once so unmatched delimiters stay cheap.
    const last = new Map(), closer = new Map();
    for (const match of source.matchAll(/`+/g)) {
      const size = match[0].length;
      if (last.has(size)) closer.set(last.get(size), match.index);
      last.set(size, match.index);
    }
    const token = /\n|\\[!-/:-@\[-`{-~]|`+|\*+|_+|!?\[|<[a-z!/]|https?:\/\//gi;
    const link = /(!?)\[([^\[\]\n]{1,512})\]\(([^()\s]*(?:\([^()\s]*\)[^()\s]*)*)\)/y;
    const html = /<(?:\/?[A-Za-z][\w:-]*(?:\s+[^<>\n]*)?\s*\/?|!--[^<>]*--)>/y;
    const url = /https?:\/\/[^\s<>"']+/iy;
    let cursor = 0, match;
    while ((match = token.exec(source))) {
      const at = match.index, mark = match[0];
      if (at > cursor) current().push(text(source.slice(cursor, at)));
      cursor = token.lastIndex;
      if (mark === '\n') {
        current().push({ type: 'break' });
      } else if (mark[0] === '\\') {
        current().push(literal(mark[1]));
      } else if (mark[0] === '`' && closer.has(at)) {
        const end = closer.get(at);
        let value = source.slice(cursor, end).replace(/\n/g, ' ');
        if (/^ .* $/s.test(value) && /[^ ]/.test(value)) value = value.slice(1, -1);
        current().push({ type: 'code', value });
        cursor = end + mark.length;
      } else if (mark === '[' || mark === '![') {
        link.lastIndex = at;
        const m = link.exec(source);
        if (m) {
          current().push(!m[1] && safeURL(m[3]) ? { type: 'link', value: m[2], href: m[3] } : literal(m[0]));
          cursor = link.lastIndex;
        } else current().push(text(mark));
      } else if (mark[0] === '<') {
        html.lastIndex = at;
        const m = html.exec(source);
        current().push(m ? literal(m[0]) : text(mark));
        if (m) cursor = html.lastIndex;
      } else if (/^https?:/i.test(mark)) {
        url.lastIndex = at;
        const m = url.exec(source);
        current().push(text(m ? m[0] : mark));
        if (m) cursor = url.lastIndex;
      } else if ((mark[0] === '*' || mark[0] === '_') && mark.length <= 3) {
        const before = source[at - 1] || '', after = source[cursor] || '';
        const left = !whitespace(after) && (!punctuation.test(after) || whitespace(before) || punctuation.test(before));
        const right = !whitespace(before) && (!punctuation.test(before) || whitespace(after) || punctuation.test(after));
        const top = stack[stack.length - 1];
        if (top.mark === mark && right && (mark[0] === '*' || !left || punctuation.test(after))) {
          stack.pop();
          const type = mark.length === 1 ? 'em' : 'strong';
          const node = { type, children: top.children };
          current().push(mark.length === 3 ? { type: 'em', children: [node] } : node);
        } else if (left && stack.length < 16 && (mark[0] === '*' || !right || punctuation.test(before))) {
          stack.push({ mark, children: [] });
        } else current().push(text(mark));
      } else current().push(text(mark));
      token.lastIndex = cursor;
    }
    if (cursor < source.length) current().push(text(source.slice(cursor)));
    while (stack.length > 1) {
      const top = stack.pop();
      current().push(text(top.mark), ...top.children);
    }
    return nodes;
  }

  function blocks(lines, depth = 0) {
    const result = [];
    let paragraph = [];
    const flush = () => {
      if (paragraph.length) result.push({ type: 'p', children: inline(paragraph.join('\n')) });
      paragraph = [];
    };
    for (let i = 0; i < lines.length;) {
      const line = lines[i++];
      let m;
      if ((m = fence.exec(line))) {
        flush();
        const close = new RegExp('^ {0,3}' + m[1][0] + '{' + m[1].length + ',}[ \\t]*$');
        let end = i;
        while (end < lines.length && !close.test(lines[end])) end++;
        if (end === lines.length) {
          result.push(literal(lines.slice(i - 1).join('\n')));
          break;
        }
        const indent = line.length - line.trimStart().length;
        const body = lines.slice(i, end).map(s => s.slice(Math.min(indent, s.match(/^ */)[0].length)));
        result.push({ type: 'pre', language: m[2].toLowerCase(), value: body.length ? body.join('\n') + '\n' : '' });
        i = end + 1;
      } else if ((m = heading.exec(line))) {
        flush();
        result.push({ type: 'h' + m[1].length, children: inline((m[2] || '').replace(/(?:^|[ \t]+)#+[ \t]*$/, '')) });
      } else if (depth < 8 && (m = quote.exec(line))) {
        flush();
        const quoted = [m[1]];
        while (i < lines.length && (m = quote.exec(lines[i]))) { quoted.push(m[1]); i++; }
        result.push({ type: 'blockquote', children: blocks(quoted, depth + 1) });
      } else if ((m = item.exec(line))) {
        flush();
        const marker = m[1].slice(-1), ordered = /[.)]/.test(marker);
        const list = { type: ordered ? 'ol' : 'ul', start: ordered ? parseInt(m[1], 10) : undefined, children: [] };
        for (;;) {
          list.children.push({ type: 'li', children: inline(m[2]) });
          m = i < lines.length && item.exec(lines[i]);
          if (!m || m[1].slice(-1) !== marker) break;
          i++;
        }
        result.push(list);
      } else if (/^[ \t]*$/.test(line)) {
        flush();
        result.push({ type: 'blank' });
      } else paragraph.push(line);
    }
    flush();
    return result;
  }

  function parse(value) {
    // 32,768 code points can occupy 65,536 JS code units.
    if (value.length > 65536) return [literal(value)];
    const lines = value.split('\n');
    if (lines.length > 1024) return [literal(value)];
    const tree = blocks(lines);
    let count = 0;
    const withinBudget = nodes => nodes.every(n => ++count <= 4096 && (!n.children || withinBudget(n.children)));
    return withinBudget(tree) ? tree : [literal(value)];
  }

  const component = {
    props: ['text', 'conversation', 'linkify', 'openLink', 'openNick', 'copyCode'],
    setup(props) {
      const { h, computed } = Vue;
      const tree = computed(() => parse(props.text));
      const anchor = (href, value) => h('a', { class: 'message-link', href, onClick(event) {
        event.preventDefault();
        props.openLink(href);
      } }, value);
      function render(node) {
        switch (node.type) {
          case 'literal': return node.value;
          case 'text': return props.linkify(node.value, props.conversation).map(s => {
            if (s.url && safeURL(s.url)) return anchor(s.url, s.text);
            if (s.mention) return h('button', { type: 'button', class: 'message-mention', onClick: () => props.openNick(s.mention) }, s.text);
            return s.text;
          });
          case 'code': return h('code', node.value);
          case 'break': return h('br');
          case 'link': return anchor(node.href, node.value);
          case 'pre': return h('div', { class: 'markdown-code-block' }, [
            h('pre', [h(window.IRCapybaraCodeBlock, { source: node.value, language: node.language })]),
            h('button', {
              type: 'button',
              class: 'icon-button markdown-copy-code',
              title: 'Copy code',
              'aria-label': 'Copy code',
              onClick: () => props.copyCode(node.value),
            }, [h('svg', { class: 'icon', 'aria-hidden': 'true' }, [h('use', { href: '#i-copy' })])]),
          ]);
          case 'blank': return h('div', { class: 'markdown-blank', 'aria-hidden': 'true' });
          default: return h(node.type, node.type === 'ol' ? { start: node.start } : null, node.children.map(render));
        }
      }
      return () => h('div', { class: 'message-text markdown' }, tree.value.map(render));
    },
  };
  window.IRCapybaraMarkdown = { parse, component };
})();
