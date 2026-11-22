(() => {
  'use strict';

  // Small independent lexers. Every match consumes a whole token, so words in
  // a comment or string never acquire a second highlight.
  const words = s => new Set(s.split(' '));
  const js = 'async await break case catch class const continue debugger default delete do else export extends finally for from function get if import in instanceof let new of return set static super switch this throw try typeof var void while with yield';
  const keywords = {
    js: words(js),
    ts: words(js + ' abstract as asserts declare enum implements infer interface keyof namespace private protected public readonly satisfies type unknown'),
    go: words('break case chan const continue default defer else fallthrough for func go goto if import interface map package range return select struct switch type var'),
    py: words('and as assert async await break class continue def del elif else except finally for from global if import in is lambda match case nonlocal not or pass raise return try while with yield'),
    sh: words('case do done elif else esac fi for function if in select then time until while'),
    json: new Set(),
  };
  const aliases = new Map(Object.entries({
    js: 'js', javascript: 'js', jsx: 'js', ts: 'ts', typescript: 'ts', tsx: 'ts',
    go: 'go', golang: 'go', python: 'py', py: 'py', shell: 'sh', sh: 'sh', bash: 'sh', zsh: 'sh', json: 'json',
  }));
  const quoted = /"(?:\\[\s\S]|[^"\\])*"|'(?:\\[\s\S]|[^'\\])*'/.source;
  const blockComment = /\/\*[\s\S]*?(?:\*\/|$)/.source;
  const triple = /"""[\s\S]*?(?:"""|$)|'''[\s\S]*?(?:'''|$)/.source;
  function lexer(lang) {
    const cStyle = ['go', 'js', 'ts'].includes(lang);
    const comments = cStyle ? blockComment + '|//[^\n]*' : '#[^\n]*';
    let strings = quoted;
    if (cStyle || lang === 'sh') strings = /`(?:\\[\s\S]|[^`\\])*(?:`|$)/.source + '|' + strings;
    if (lang === 'py') strings = triple + '|' + strings;
    const parts = lang === 'json' ? [/(?<string>"(?:\\.|[^"\\\n])*")/.source]
      : ['(?<comment>' + comments + ')', '(?<string>' + strings + ')'];
    parts.push(/(?<number>\b(?:0[xX][\da-fA-F_]+|0[bB][01_]+|\d[\d_]*(?:\.\d+)?(?:[eE][+-]?\d+)?))/.source,
      /(?<word>[a-zA-Z_$][\w$]*)/.source);
    return new RegExp(parts.join('|'), 'g');
  }
  function tokenize(source, lang) {
    const tokens = [];
    if (!lang || source.length > 65536) return tokens;
    for (const match of source.matchAll(lexer(lang))) {
      const value = match[0], end = match.index + value.length;
      let type;
      if (match.groups.comment) type = 'comment';
      else if (match.groups.string) type = 'string';
      else if (match.groups.number) type = 'number';
      else if (keywords[lang].has(value)) type = 'keyword';
      else if (/^(?:true|false|null|nil|undefined|True|False|None)$/.test(value)) type = 'builtin';
      else if (lang !== 'json' && source[end] === '(') type = 'function';
      if (type) tokens.push({ start: match.index, end, type });
      if (tokens.length === 4096) break;
    }
    return tokens;
  }

  const component = {
    props: ['source', 'language'],
    setup(props) {
      const { h, ref, watch, onMounted, onBeforeUnmount } = Vue;
      const element = ref(null);
      let observer, visible = false, ranges = [];
      const native = Boolean(window.CSS?.highlights && window.StaticRange && window.Highlight);
      function release() {
        for (const [key, range] of ranges) {
          const group = CSS.highlights.get(key);
          if (group) {
            group.delete(range);
            if (!group.size) CSS.highlights.delete(key);
          }
        }
        ranges = [];
      }
      function paint() {
        release();
        const el = element.value;
        if (!el) return;
        const source = props.source;
        el.textContent = source;
        const lang = aliases.get(props.language);
        if (!visible || !lang || !source) return;
        const tokens = tokenize(source, lang);
        if (native) {
          const node = el.firstChild;
          for (const { start, end, type } of tokens) {
            const key = 'ircapybara-' + type;
            if (!CSS.highlights.has(key)) CSS.highlights.set(key, new Highlight());
            const range = new StaticRange({ startContainer: node, startOffset: start, endContainer: node, endOffset: end });
            CSS.highlights.get(key).add(range);
            ranges.push([key, range]);
          }
        } else {
          const fragment = document.createDocumentFragment();
          let last = 0;
          for (const { start, end, type } of tokens) {
            fragment.append(source.slice(last, start));
            const span = document.createElement('span');
            span.className = 'irc-token-' + type;
            span.textContent = source.slice(start, end);
            fragment.append(span);
            last = end;
          }
          fragment.append(source.slice(last));
          el.replaceChildren(fragment);
        }
      }
      onMounted(() => {
        visible = !window.IntersectionObserver;
        paint();
        if (!visible) {
          observer = new IntersectionObserver(entries => {
            const next = entries[entries.length - 1].isIntersecting;
            if (visible !== next) { visible = next; paint(); }
          });
          observer.observe(element.value);
        }
      });
      watch(() => [props.source, props.language], paint, { flush: 'post' });
      onBeforeUnmount(() => { observer?.disconnect(); release(); });
      return () => h('code', { ref: element });
    },
  };
  window.IRCapybaraCodeBlock = component;
})();
