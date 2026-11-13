// VisiComms web chat widget. Served from /chat/widget.js; a site embeds it with
//
//   <script src="https://api.visicomms.com/chat/widget.js" data-widget="w_…" async></script>
//
// and can drive it with visicomms('open' | 'close' | 'toggle') and
// visicomms('setUser', {name, email, phone}). Calls made before the script
// loads are queued by this one-liner, placed before the script tag:
//
//   <script>window.visicomms=window.visicomms||function(){(visicomms.q=visicomms.q||[]).push(arguments)}</script>
//
// Nothing is sent to the server until the visitor sends a message, apart from
// fetching the widget's settings. Everything the widget shows from the server
// is set as text, never as HTML.
(function () {
  'use strict';
  if (window.__visicommsChat) return;
  window.__visicommsChat = true;

  var script = document.currentScript || document.querySelector('script[data-widget][src*="/chat/widget.js"]');
  if (!script) return;
  var widgetId = script.getAttribute('data-widget');
  if (!/^w_[0-9a-f]{24}$/.test(widgetId || '')) return;
  var base = new URL(script.src).origin + '/chat/w/' + widgetId;
  var storeKey = 'visicomms_chat_' + widgetId;
  var attrKey = 'visicomms_attr_' + widgetId;

  function load(key, store) {
    try { return JSON.parse((store || localStorage).getItem(key) || 'null'); } catch (e) { return null; }
  }
  function save(key, value, store) {
    try { (store || localStorage).setItem(key, JSON.stringify(value)); } catch (e) { /* private mode */ }
  }
  function forget(key) {
    try { localStorage.removeItem(key); } catch (e) { /* private mode */ }
  }

  // Where the visitor came from: the first page of this visit, unless a later
  // page carries campaign parameters of its own.
  var PARAMS = ['utm_source', 'utm_medium', 'utm_campaign', 'utm_term', 'utm_content', 'gclid', 'gbraid', 'wbraid', 'fbclid', 'msclkid'];
  var attribution = (function () {
    var q = new URLSearchParams(location.search);
    var fresh = {};
    var tagged = false;
    PARAMS.forEach(function (p) {
      var v = q.get(p);
      if (v) { fresh[p] = v.slice(0, 500); tagged = true; }
    });
    var stored = load(attrKey, sessionStorage);
    if (stored && !tagged) return stored;
    fresh.landing_page = location.href.slice(0, 2000);
    var ref = document.referrer;
    if (ref) {
      try { if (new URL(ref).host !== location.host) fresh.referrer = ref.slice(0, 2000); } catch (e) { /* ignore */ }
    }
    save(attrKey, fresh, sessionStorage);
    return fresh;
  })();

  var state = load(storeKey) || {};
  // Returning means they had a conversation before this page load.
  var returning = Boolean(state.token);
  var config = null;
  var user = {};
  var messages = [];
  var lastId = 0;
  var open = false;
  var unread = 0;
  var sending = false;
  var streaming = false;
  var queued = [];
  // The pop-up on screen, held here only: the server saves it into the
  // conversation if the visitor replies, and never otherwise.
  var popup = null;
  var typingName = '';
  var typingTimer = null;
  var typingSentAt = 0;
  // One id per visit, so the server can allow each pop-up once per visit.
  var visit = (function () {
    var v = load('visicomms_visit', sessionStorage);
    if (!v) {
      var b = new Uint8Array(16);
      (window.crypto || window.msCrypto).getRandomValues(b);
      v = Array.prototype.map.call(b, function (x) { return ('0' + x.toString(16)).slice(-2); }).join('');
      save('visicomms_visit', v, sessionStorage);
    }
    return v;
  })();

  // --- API ---

  function request(method, path, body) {
    var headers = {};
    if (state.token) headers.Authorization = 'Bearer ' + state.token;
    if (body) headers['Content-Type'] = 'application/json';
    return fetch(base + path, { method: method, headers: headers, body: body ? JSON.stringify(body) : undefined, credentials: 'omit' })
      .then(function (r) {
        return r.json().catch(function () { return {}; }).then(function (data) {
          if (!r.ok) {
            var err = new Error(data.error || 'Something went wrong. Try again.');
            err.status = r.status;
            err.field = data.field;
            throw err;
          }
          return data;
        });
      });
  }

  function dropConversation() {
    state = {};
    returning = false;
    forget(storeKey);
    messages = [];
    lastId = 0;
    renderMessages();
  }

  function addMessages(list, quiet) {
    var added = false;
    list.forEach(function (m) {
      if (m.id <= lastId) return;
      if (messages.some(function (x) { return x.id === m.id; })) return;
      messages.push(m);
      lastId = Math.max(lastId, m.id);
      added = true;
      if (m.from === 'team') { typingName = ''; }
      if (m.from === 'team' && !open && !quiet) unread++;
    });
    if (added) { renderMessages(); renderBadge(); }
  }

  function history() {
    return request('GET', '/messages').then(function (d) { addMessages(d.messages || [], true); })
      .catch(function (e) { if (e.status === 401) dropConversation(); });
  }

  // Server-sent events over fetch: EventSource cannot send the token header.
  function stream() {
    if (streaming || !state.token || !window.ReadableStream) return;
    streaming = true;
    var retry = 3000;
    fetch(base + '/stream?after=' + lastId, { headers: { Authorization: 'Bearer ' + state.token }, credentials: 'omit' })
      .then(function (r) {
        if (r.status === 401) { dropConversation(); throw new Error('gone'); }
        if (!r.ok || !r.body) throw new Error('stream ' + r.status);
        var reader = r.body.getReader();
        var decoder = new TextDecoder();
        var buffer = '';
        function pump() {
          return reader.read().then(function (chunk) {
            if (chunk.done) return;
            buffer += decoder.decode(chunk.value, { stream: true });
            var parts = buffer.split('\n\n');
            buffer = parts.pop();
            parts.forEach(function (block) {
              var data = '';
              var type = 'message';
              block.split('\n').forEach(function (line) {
                if (line.indexOf('data: ') === 0) data += line.slice(6);
                else if (line.indexOf('event: ') === 0) type = line.slice(7);
                else if (line.indexOf('retry: ') === 0) retry = parseInt(line.slice(7), 10) || retry;
              });
              if (!data) return;
              try {
                var parsed = JSON.parse(data);
                if (type === 'typing') showTyping(parsed.name || '');
                else addMessages([parsed]);
              } catch (e) { /* ignore */ }
            });
            return pump();
          });
        }
        return pump();
      })
      .catch(function () { retry = Math.min(retry * 2, 60000); })
      .then(function () {
        streaming = false;
        if (state.token) setTimeout(stream, retry);
      });
  }

  function send(text, form) {
    var body = {
      text: text,
      page: location.href.slice(0, 2000),
      attribution: attribution,
      name: form.name || user.name || '',
      email: form.email || user.email || '',
      phone: form.phone || user.phone || ''
    };
    if (!state.token && popup) body.popup = { id: popup.id, shown_at: popup.shownAt };
    return request('POST', '/messages', body).then(function (d) {
      if (d.token) {
        state = { token: d.token };
        save(storeKey, state);
        // The pop-up they answered is now part of the conversation on the
        // server; read it back from there.
        if (popup) { popup = null; return history().then(function () { addMessages([d.message]); stream(); }); }
      }
      addMessages([d.message]);
      stream();
    }).catch(function (e) {
      if (e.status === 401 && state.token) {
        // The conversation is gone on the server: start a new one.
        dropConversation();
        renderForm();
        return send(text, form);
      }
      throw e;
    });
  }

  // --- UI ---

  var host = document.createElement('div');
  host.setAttribute('data-visicomms-chat', '');
  var root = host.attachShadow ? host.attachShadow({ mode: 'open' }) : host;
  var els = {};

  function el(tag, attrs, children) {
    var n = document.createElement(tag);
    Object.keys(attrs || {}).forEach(function (k) {
      if (k === 'text') n.textContent = attrs[k];
      else if (k.indexOf('on') === 0) n.addEventListener(k.slice(2), attrs[k]);
      else n.setAttribute(k, attrs[k]);
    });
    (children || []).forEach(function (c) { if (c) n.appendChild(c); });
    return n;
  }

  function css(color, side) {
    return [
      ':host{all:initial}',
      '*{box-sizing:border-box;font-family:system-ui,-apple-system,"Segoe UI",Roboto,sans-serif}',
      '.launcher{position:fixed;bottom:20px;' + side + ':20px;width:56px;height:56px;border-radius:50%;border:0;cursor:pointer;background:' + color + ';color:#fff;box-shadow:0 4px 14px rgba(0,0,0,.25);z-index:2147483646;display:flex;align-items:center;justify-content:center}',
      '.launcher svg{width:26px;height:26px}',
      '.badge{position:absolute;top:-2px;right:-2px;min-width:20px;height:20px;border-radius:10px;background:#dc2626;color:#fff;font-size:12px;line-height:20px;text-align:center;padding:0 5px}',
      '.panel{position:fixed;bottom:88px;' + side + ':20px;width:370px;max-width:calc(100vw - 32px);height:560px;max-height:calc(100vh - 110px);background:#fff;color:#111827;border-radius:14px;box-shadow:0 10px 40px rgba(0,0,0,.25);display:flex;flex-direction:column;overflow:hidden;z-index:2147483647}',
      '.panel[hidden]{display:none}',
      '.head{background:' + color + ';color:#fff;padding:14px 16px;display:flex;align-items:flex-start;gap:8px}',
      '.head .t{flex:1}.head .n{font-weight:600;font-size:16px}.head .s{font-size:13px;opacity:.9;margin-top:2px}',
      '.close{background:none;border:0;color:#fff;cursor:pointer;font-size:22px;line-height:1;padding:0 2px}',
      '.log{flex:1;overflow-y:auto;padding:14px;display:flex;flex-direction:column;gap:8px;background:#f9fafb}',
      '.msg{max-width:82%;padding:9px 12px;border-radius:14px;font-size:14px;line-height:1.4;white-space:pre-wrap;word-wrap:break-word}',
      '.team{align-self:flex-start;background:#fff;border:1px solid #e5e7eb}',
      '.visitor{align-self:flex-end;background:' + color + ';color:#fff}',
      '.who{font-size:11px;color:#6b7280;margin:0 4px -4px}',
      '.note{font-size:13px;color:#374151;background:#fef3c7;border-radius:10px;padding:9px 12px}',
      'form{border-top:1px solid #e5e7eb;padding:10px;display:flex;flex-direction:column;gap:6px;background:#fff}',
      'input,textarea{width:100%;border:1px solid #d1d5db;border-radius:8px;padding:8px 10px;font-size:14px;color:#111827;background:#fff}',
      'textarea{resize:none;height:64px}',
      'input:focus,textarea:focus{outline:2px solid ' + color + ';outline-offset:-1px;border-color:transparent}',
      '.row{display:flex;gap:6px;align-items:flex-end}',
      '.send{background:' + color + ';color:#fff;border:0;border-radius:8px;padding:9px 14px;font-size:14px;cursor:pointer}',
      '.send[disabled]{opacity:.6;cursor:default}',
      '.err{color:#b91c1c;font-size:12px}',
      '.replies{display:flex;flex-wrap:wrap;gap:6px;align-self:flex-start;max-width:90%}',
      '.reply{background:#fff;color:' + color + ';border:1px solid ' + color + ';border-radius:16px;padding:6px 12px;font-size:13px;cursor:pointer}',
      '.reply:hover{background:' + color + ';color:#fff}',
      '.typing{font-size:12px;color:#6b7280;font-style:italic;margin:0 4px}',
      '.bubble{position:fixed;bottom:88px;' + side + ':20px;max-width:300px;background:#fff;color:#111827;border-radius:14px;box-shadow:0 6px 24px rgba(0,0,0,.2);padding:12px 32px 12px 14px;font-size:14px;line-height:1.4;cursor:pointer;z-index:2147483646;white-space:pre-wrap}',
      '.bubble[hidden]{display:none}',
      '.bubble .who{margin:0 0 4px;display:block}',
      '.bubble .x{position:absolute;top:6px;right:8px;background:none;border:0;font-size:18px;line-height:1;color:#6b7280;cursor:pointer}',
      '@media (max-width:480px){.panel{bottom:0;' + side + ':0;width:100vw;max-width:100vw;height:100%;max-height:100%;border-radius:0}}'
    ].join('');
  }

  var ICON = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M21 15a2 2 0 0 1-2 2H7l-4 4V5a2 2 0 0 1 2-2h14a2 2 0 0 1 2 2z"/></svg>';

  function build() {
    var side = config.position === 'left' ? 'left' : 'right';
    root.appendChild(el('style', { text: css(config.color, side) }));
    els.badge = el('span', { class: 'badge', hidden: '' });
    els.launcher = el('button', { class: 'launcher', type: 'button', 'aria-label': 'Chat with ' + config.name, onclick: toggle });
    els.launcher.innerHTML = ICON; // a constant, not server data
    els.launcher.appendChild(els.badge);
    els.status = el('div', { class: 's' });
    els.log = el('div', { class: 'log', 'aria-live': 'polite' });
    els.fields = el('div');
    els.error = el('div', { class: 'err', role: 'alert' });
    els.text = el('textarea', { 'aria-label': 'Message', placeholder: 'Write a message…', maxlength: String(config.max_message || 4000) });
    els.text.addEventListener('keydown', function (e) {
      if (e.key === 'Enter' && !e.shiftKey) { e.preventDefault(); submit(); }
    });
    els.send = el('button', { class: 'send', type: 'submit', text: 'Send' });
    els.form = el('form', { onsubmit: function (e) { e.preventDefault(); submit(); } },
      [els.fields, el('div', { class: 'row' }, [els.text, els.send]), els.error]);
    els.panel = el('div', { class: 'panel', role: 'dialog', 'aria-label': 'Chat with ' + config.name, hidden: '' }, [
      el('div', { class: 'head' }, [
        el('div', { class: 't' }, [el('div', { class: 'n', text: config.name }), els.status]),
        el('button', { class: 'close', type: 'button', 'aria-label': 'Close chat', text: '×', onclick: close })
      ]),
      els.log,
      els.form
    ]);
    els.bubbleWho = el('span', { class: 'who' });
    els.bubbleText = el('span');
    els.bubble = el('div', { class: 'bubble', role: 'status', hidden: '', onclick: show }, [
      els.bubbleWho, els.bubbleText,
      el('button', { class: 'x', type: 'button', 'aria-label': 'Dismiss', text: '×', onclick: function (e) { e.stopPropagation(); els.bubble.setAttribute('hidden', ''); } })
    ]);
    els.text.addEventListener('input', visitorTyping);
    root.appendChild(els.panel);
    root.appendChild(els.bubble);
    root.appendChild(els.launcher);
    document.body.appendChild(host);
    els.status.textContent = config.staffed ? config.reply_time : 'We’re away right now';
    renderForm();
    renderMessages();
  }

  // Pre-chat fields, shown only before the first message. Outside staffed
  // hours an email or phone field is offered so the reply can reach them.
  function fieldsWanted() {
    if (state.token) return [];
    var f = config.form || {};
    var out = [];
    if (f.name !== 'off' && !user.name) out.push(['name', 'Your name', 'text', f.name === 'required']);
    if (f.email !== 'off' && !user.email) out.push(['email', 'Email', 'email', f.email === 'required']);
    if (f.phone !== 'off' && !user.phone) out.push(['phone', 'Phone', 'tel', f.phone === 'required']);
    if (!config.staffed && f.email === 'off' && f.phone === 'off' && !user.email && !user.phone) {
      out.push(['email', 'Email, so we can reply', 'email', false]);
    }
    return out;
  }

  function renderForm() {
    if (!els.fields) return;
    els.fields.textContent = '';
    els.inputs = {};
    fieldsWanted().forEach(function (f) {
      var input = el('input', { name: f[0], type: f[2], placeholder: f[1] + (f[3] ? '' : ' (optional)'), 'aria-label': f[1], autocomplete: f[0] === 'phone' ? 'tel' : f[0] });
      if (f[3]) input.required = true;
      els.inputs[f[0]] = input;
      els.fields.appendChild(input);
    });
    els.fields.style.display = els.fields.childNodes.length ? 'flex' : 'none';
    els.fields.style.flexDirection = 'column';
    els.fields.style.gap = '6px';
  }

  function renderMessages() {
    if (!els.log) return;
    els.log.textContent = '';
    var greeting = returning ? config.returning_greeting : config.greeting;
    els.log.appendChild(el('div', { class: 'msg team', text: greeting }));
    if (!config.staffed) els.log.appendChild(el('div', { class: 'note', text: config.offline_message }));
    var shown = messages.slice();
    // A pop-up on screen is not saved anywhere yet; it is drawn from memory.
    if (popup && !state.token) shown.unshift({ from: 'team', name: popup.sender, text: popup.text, quick_replies: popup.quick_replies });
    var lastName = '';
    shown.forEach(function (m) {
      if (m.from === 'team' && m.name && m.name !== lastName) els.log.appendChild(el('div', { class: 'who', text: m.name }));
      lastName = m.from === 'team' ? m.name : '';
      els.log.appendChild(el('div', { class: 'msg ' + (m.from === 'visitor' ? 'visitor' : 'team'), text: m.text }));
    });
    // Quick replies belong to the latest line only, and only while it is the
    // team's turn to have spoken last.
    var last = shown[shown.length - 1];
    var replies = last ? (last.from === 'team' ? last.quick_replies : null) : config.quick_replies;
    if (replies && replies.length) {
      els.log.appendChild(el('div', { class: 'replies' }, replies.map(function (r) {
        return el('button', { class: 'reply', type: 'button', text: r, onclick: function () { els.text.value = r; submit(); } });
      })));
    }
    if (typingName !== '') els.log.appendChild(el('div', { class: 'typing', text: (typingName || 'Someone') + ' is typing…' }));
    els.log.scrollTop = els.log.scrollHeight;
  }

  function showTyping(name) {
    typingName = name || 'Someone';
    renderMessages();
    clearTimeout(typingTimer);
    typingTimer = setTimeout(function () { typingName = ''; renderMessages(); }, 6000);
  }

  // Tells the team the visitor is typing, at most every few seconds. Before
  // the first message there is no conversation, so nothing is sent.
  function visitorTyping() {
    if (!state.token || Date.now() - typingSentAt < 3000 || !els.text.value.trim()) return;
    typingSentAt = Date.now();
    request('POST', '/typing').catch(function () { /* not important */ });
  }

  function renderBadge() {
    if (!els.badge) return;
    if (unread > 0 && !open) {
      els.badge.textContent = unread > 9 ? '9+' : String(unread);
      els.badge.removeAttribute('hidden');
    } else {
      els.badge.setAttribute('hidden', '');
    }
  }

  function submit() {
    if (sending) return;
    var text = els.text.value.trim();
    if (!text) return;
    var form = {};
    Object.keys(els.inputs || {}).forEach(function (k) { form[k] = els.inputs[k].value.trim(); });
    for (var k in els.inputs) {
      if (els.inputs[k].required && !form[k]) { els.error.textContent = 'Fill in ' + els.inputs[k].getAttribute('aria-label').toLowerCase() + '.'; els.inputs[k].focus(); return; }
    }
    sending = true;
    els.send.disabled = true;
    els.error.textContent = '';
    send(text, form).then(function () {
      els.text.value = '';
      renderForm();
    }).catch(function (e) {
      els.error.textContent = e.message;
      if (e.field && els.inputs && els.inputs[e.field]) els.inputs[e.field].focus();
    }).then(function () {
      sending = false;
      els.send.disabled = false;
    });
  }

  function show() {
    if (!els.panel) { queued.push('open'); return; }
    if (els.bubble) els.bubble.setAttribute('hidden', '');
    open = true;
    unread = 0;
    renderBadge();
    els.panel.removeAttribute('hidden');
    els.log.scrollTop = els.log.scrollHeight;
    els.text.focus();
  }
  function close() {
    if (!els.panel) return;
    open = false;
    els.panel.setAttribute('hidden', '');
    els.launcher.focus();
  }
  function toggle() { if (open) close(); else show(); }

  // --- the site's API ---

  function command(name, arg) {
    if (name === 'open') show();
    else if (name === 'close') close();
    else if (name === 'toggle') toggle();
    else if ((name === 'setUser' || name === 'setData') && arg && typeof arg === 'object') {
      ['name', 'email', 'phone'].forEach(function (k) { if (typeof arg[k] === 'string') user[k] = arg[k].slice(0, 254); });
      renderForm();
    }
  }
  var pending = (window.visicomms && window.visicomms.q) || [];
  window.visicomms = function () { command(arguments[0], arguments[1]); };

  function start() {
    request('GET', '/config').then(function (c) {
      config = c;
      build();
      Array.prototype.forEach.call(pending, function (args) { command(args[0], args[1]); });
      queued.forEach(function (c) { command(c); });
      if (state.token) history().then(stream);
      popups();
    }).catch(function () { /* the widget stays hidden */ });
  }
  // --- pop-up messages ---
  //
  // Each pop-up waits for its moment: a delay on the page, a matching page,
  // exit intent, a scroll depth, or the visitor going idle. The server is
  // asked before one shows; it counts the showing and allows each pop-up once
  // per visit. Pop-ups are for visitors who have not written: none shows
  // once a conversation exists, while the chat is open, or while another is
  // on screen.

  var shownThisVisit = load('visicomms_popups_' + widgetId, sessionStorage) || [];
  var everShown = load('visicomms_popups_' + widgetId) || [];

  function isMobile() {
    return window.matchMedia ? window.matchMedia('(pointer: coarse)').matches || window.innerWidth < 768 : false;
  }

  function pathMatches(pattern) {
    var re = new RegExp('^' + pattern.split('*').map(function (p) { return p.replace(/[.+?^${}()|[\]\\]/g, '\\$&'); }).join('.*') + '/?$');
    return re.test(location.pathname);
  }

  function eligible(p) {
    if (state.token || open || popup) return false;
    if (p.devices === 'desktop' && isMobile()) return false;
    if (p.devices === 'mobile' && !isMobile()) return false;
    if (shownThisVisit.indexOf(p.id) >= 0) return false;
    if (p.frequency === 'first_visit' && everShown.indexOf(p.id) >= 0) return false;
    if (p.only_if_none && shownThisVisit.length > 0) return false;
    if (p.trigger === 'page' && !pathMatches(p.page_pattern || '')) return false;
    return true;
  }

  var tried = {};
  function fire(p) {
    // Scroll and exit fire often: ask the server once per page view.
    if (tried[p.id] || !eligible(p)) return;
    tried[p.id] = true;
    fetch(base + '/popups/' + p.id + '/shown?visit=' + visit, { method: 'POST', credentials: 'omit' }).then(function (r) {
      if (!r.ok || !eligible(p)) return;
      shownThisVisit.push(p.id);
      save('visicomms_popups_' + widgetId, shownThisVisit, sessionStorage);
      if (everShown.indexOf(p.id) < 0) { everShown.push(p.id); save('visicomms_popups_' + widgetId, everShown); }
      popup = { id: p.id, text: p.text, sender: p.sender, quick_replies: p.quick_replies, shownAt: new Date().toISOString() };
      renderMessages();
      if (p.sound) chime();
      if (p.opens_window) {
        show();
      } else {
        els.bubbleWho.textContent = p.sender;
        els.bubbleText.textContent = p.text;
        els.bubble.removeAttribute('hidden');
        unread = 1;
        renderBadge();
      }
    }).catch(function () { /* stay quiet */ });
  }

  // A short tone. Browsers block sound until the visitor has interacted with
  // the page, and then it simply does not play.
  function chime() {
    try {
      var Ctx = window.AudioContext || window.webkitAudioContext;
      var ctx = new Ctx();
      var o = ctx.createOscillator();
      var g = ctx.createGain();
      o.frequency.value = 880;
      g.gain.setValueAtTime(0.08, ctx.currentTime);
      g.gain.exponentialRampToValueAtTime(0.0001, ctx.currentTime + 0.35);
      o.connect(g); g.connect(ctx.destination);
      o.start(); o.stop(ctx.currentTime + 0.35);
    } catch (e) { /* no sound */ }
  }

  function popups() {
    var list = config.popups || [];
    if (!list.length) return;
    var idleTimers = [];
    var idle = list.filter(function (p) { return p.trigger === 'idle'; });
    function activity() {
      idleTimers.forEach(clearTimeout);
      idleTimers = idle.map(function (p) {
        return setTimeout(function () { fire(p); }, p.seconds * 1000);
      });
    }
    list.forEach(function (p) {
      if (p.trigger === 'delay' || p.trigger === 'page') {
        setTimeout(function () { fire(p); }, (p.seconds || 0) * 1000);
      } else if (p.trigger === 'exit') {
        document.addEventListener('mouseout', function (e) {
          if (!e.relatedTarget && e.clientY <= 0) fire(p);
        });
      } else if (p.trigger === 'scroll') {
        window.addEventListener('scroll', function () {
          var h = document.documentElement;
          var depth = (h.scrollTop + window.innerHeight) / Math.max(h.scrollHeight, 1) * 100;
          if (depth >= p.scroll_percent) fire(p);
        }, { passive: true });
      }
    });
    if (idle.length) {
      ['mousemove', 'keydown', 'scroll', 'touchstart'].forEach(function (ev) {
        window.addEventListener(ev, activity, { passive: true });
      });
      activity();
    }
  }

  if (document.body) start();
  else document.addEventListener('DOMContentLoaded', start);
})();
