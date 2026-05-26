/* Conductor 页 — 直连 Conductor WS，不走 bridge session */
(function () {
  'use strict';
  const wsUrl = () => `${location.protocol === 'https:' ? 'wss:' : 'ws:'}//${location.hostname}:8900/ws`;
  const FAIL_MAX = 5, RECON_BASE = 1200, RECON_MAX = 30000;
  const $ = id => document.getElementById(id);
  const t = k => (window.gaT && window.gaT(k)) || k;
  const esc = s => String(s ?? '').replace(/[&<>"]/g, c => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;' }[c]));
  const md = s => { try { return typeof marked !== 'undefined' ? marked.parse(s || '') : esc(s); } catch { return esc(s); } };
  const stripAttach = text => String(text || '').replace(/\[(Image|File)\s+#\d+\]\s*/g, '').trim();
  const FC_SVG = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M14 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V8z"/><polyline points="14 2 14 8 20 8"/></svg>';
  const ST_ICONS = {
    running: '<span class="collab-st-ic collab-st-ic--spin" aria-hidden="true"></span>',
    reported: '<span class="collab-st-ic collab-st-ic--ok" aria-hidden="true">✓</span>',
    paused: '<span class="collab-st-ic collab-st-ic--pause" aria-hidden="true">⏸</span>',
    failed: '<span class="collab-st-ic collab-st-ic--warn" aria-hidden="true">!</span>',
    terminated: '<span class="collab-st-ic collab-st-ic--off" aria-hidden="true">×</span>',
  };
  const ST_KEYS = { running: 'collab.stRunning', reported: 'collab.stReported', paused: 'collab.stPaused', failed: 'collab.stFailed', terminated: 'collab.stTerminated' };
  const CHIP_KEYS = ['collab.chipProgress', 'collab.chipPause', 'collab.chipSummary'];

  const S = {
    everConnected: false, reconnecting: false, serviceAvailable: false,
    messages: [], workers: [], runningCount: 0,
    conductorTyping: false, failCount: 0,
    historyReady: false, reconnectAt: 0, progressOpen: false,
  };
  let ws, connectTimer, reconnectTick, titleSeq = 0, wsGen = 0, localSeq = 0;
  const titleSeen = new Map();

  let draftEl = null;

  const scrollMsgs = () => { const a = $('collab-msgs'); if (a) a.scrollTop = a.scrollHeight; };
  const showDraft = () => S.conductorTyping && S.serviceAvailable && S.historyReady && S.messages.length > 0;

  function clearDraft() {
    if (draftEl) { draftEl.remove(); draftEl = null; }
  }

  function syncDraft() {
    const list = $('collab-msg-list');
    if (!list || list.hidden || !showDraft()) return clearDraft();
    if (!draftEl) draftEl = document.createElement('div');
    draftEl.className = 'msg system collab-msg-enter';
    draftEl.setAttribute('aria-label', t('collab.typing'));
    draftEl.innerHTML = '<div class="bubble sys"><span class="collab-wait-dots" aria-hidden="true"><i></i><i></i><i></i></span></div>';
    list.appendChild(draftEl);
    requestAnimationFrame(scrollMsgs);
  }

  function relTime(ts) {
    if (!ts) return '';
    const ms = typeof ts === 'number' ? (ts > 1e12 ? ts : ts * 1000) : Date.parse(ts);
    if (!ms || Number.isNaN(ms)) return '';
    const sec = Math.max(0, Math.floor((Date.now() - ms) / 1000));
    if (sec < 10) return t('collab.timeJust');
    if (sec < 60) return t('collab.timeSec').replace('{n}', sec);
    const min = Math.floor(sec / 60);
    if (min < 60) return t('collab.timeMin').replace('{n}', min);
    const hr = Math.floor(min / 60);
    return hr < 24 ? t('collab.timeHr').replace('{n}', hr) : t('collab.timeDay').replace('{n}', Math.floor(hr / 24));
  }

  function mapStatus(status, reply) {
    const r = (reply || '').trim();
    if (status === 'running') return 'running';
    if (status === 'failed') return 'failed';
    if (status === 'aborted') return 'terminated';
    if (status === 'stopped') return r ? 'reported' : 'paused';
    return 'paused';
  }

  function normalizeWorker(raw) {
    if (!titleSeen.has(raw.id)) titleSeen.set(raw.id, ++titleSeq);
    const ui = mapStatus(raw.status, raw.reply);
    let title = String(raw.prompt ?? '').replace(/^[\s请帮我麻烦]+/u, '').trim();
    if (!title) title = t('collab.taskFallback').replace('{n}', titleSeen.get(raw.id));
    else {
      title = (title.split(/[\n。！？.!?]/)[0] || '').trim();
      if (title.length > 18) title = title.slice(0, 18) + '…';
    }
    const reply = String(raw.reply || '').replace(/\s+/g, ' ').trim();
    let summary = reply ? (reply.length > 80 ? reply.slice(0, 80) + '…' : reply) : t(ui === 'running' ? 'collab.summaryRunning' : 'collab.summaryWait');
    return { id: raw.id, title, status: ui, summary, updatedAt: raw.updated_at };
  }

  function syncProgressToggle() {
    const btn = $('collab-progress-toggle'), body = $('collab-body');
    if (!btn || !body) return;
    const split = body.classList.contains('collab-body--split');
    btn.hidden = !split;
    body.classList.toggle('collab-progress-open', split && S.progressOpen);
  }

  function setConnUi() {
    const off = $('collab-offline'), recon = $('collab-reconnect'), inp = $('collab-input'), btn = $('collab-send');
    const avail = S.serviceAvailable;
    const trying = !avail && !S.everConnected && S.failCount < FAIL_MAX;
    if (off) off.hidden = avail || S.reconnecting || trying;
    if (recon) {
      recon.hidden = !S.reconnecting;
      recon.textContent = S.reconnecting && S.reconnectAt > Date.now()
        ? t('collab.reconnect') + ' ' + t('collab.reconnectIn').replace('{n}', Math.ceil((S.reconnectAt - Date.now()) / 1000))
        : t('collab.reconnect');
    }
    if (inp) inp.disabled = !avail;
    if (btn) btn.disabled = !avail;
    syncDraft();
    syncProgressToggle();
  }

  function renderWorkers() {
    const box = $('collab-workers'), empty = $('collab-progress-empty');
    if (!box) return;
    if (empty) empty.hidden = S.workers.length > 0;
    box.innerHTML = S.workers.map(w => `
      <article class="collab-card collab-card--${w.status}">
        <div class="collab-card-st">${ST_ICONS[w.status] || ''}<span class="collab-dot"></span>${esc(t(ST_KEYS[w.status] || 'collab.stPaused'))}${w.updatedAt ? `<span class="collab-card-time">${esc(relTime(w.updatedAt))}</span>` : ''}</div>
        <div class="collab-card-title">${esc(w.title)}</div>
        <div class="collab-card-sum">${esc(w.summary)}</div>
      </article>`).join('');
    const running = S.workers.filter(w => w.status === 'running').length;
    S.runningCount = running;
    document.dispatchEvent(new CustomEvent('collab:running-count', { detail: { count: running } }));
    const sticky = $('collab-sticky');
    if (sticky) { sticky.hidden = !running; sticky.textContent = t('collab.sticky').replace('{n}', running); }
    const stats = $('collab-progress-stats');
    if (stats) {
      const has = S.workers.length > 0;
      stats.hidden = !has;
      if (has) stats.textContent = t('collab.progressStats')
        .replace('{running}', running)
        .replace('{done}', S.workers.filter(w => w.status === 'reported').length)
        .replace('{issue}', S.workers.filter(w => w.status === 'failed' || w.status === 'terminated').length);
    }
  }

  function renderUserMsg(item) {
    const sub = n => (window.gaFileSubLabel && window.gaFileSubLabel(n)) || n;
    const imgs = (item.images || []).map(im => `<img src="${esc(im.dataUrl || '')}" alt="">`).join('');
    const files = (item.files || []).map(f => {
      const name = f.name || 'file';
      return `<div class="file-chip" data-path="${esc(f.path || '')}" data-name="${esc(name)}"><span class="fc-icon">${FC_SVG}</span><span class="fc-meta"><span class="fc-name">${esc(name)}</span><span class="fc-sub">${esc(sub(name))}</span></span></div>`;
    }).join('');
    const clean = stripAttach(item.msg);
    const text = clean ? `<div class="bubble">${esc(clean).replace(/\n/g, '<br>')}</div>` : '';
    return `<div class="msg user collab-msg-enter"><div class="user-stack">${files ? `<div class="user-files">${files}</div>` : ''}${imgs ? `<div class="user-imgs">${imgs}</div>` : ''}${text}</div></div>`;
  }

  function renderMsg(item) {
    if (item.role === 'user') return renderUserMsg(item);
    if (item.role === 'conductor') return `<div class="msg assistant collab-msg-enter"><div class="bubble md">${md(item.msg)}</div></div>`;
    return `<div class="msg system collab-msg-enter"><div class="bubble sys">${esc(item.msg)}</div></div>`;
  }

  function syncLayout() {
    const body = $('collab-body');
    if (!body) return;
    body.classList.toggle('collab-body--split', S.historyReady && S.messages.length > 0);
    syncProgressToggle();
  }

  function syncMessages() {
    const area = $('collab-msgs'), welcome = $('collab-welcome'), list = $('collab-msg-list'), head = $('collab-chat-head');
    if (!area || !list) return;
    if (!S.historyReady) {
      area.classList.remove('has-msgs');
      if (welcome) welcome.hidden = true;
      if (head) head.hidden = true;
      list.hidden = true;
      return syncLayout();
    }
    const has = S.messages.length > 0;
    area.classList.toggle('has-msgs', has);
    if (welcome) welcome.hidden = has;
    if (head) head.hidden = !has;
    list.hidden = !has;
    list.innerHTML = S.messages.map(renderMsg).join('');
    syncDraft();
    scrollMsgs();
    syncLayout();
  }

  function pushMsg(item) {
    if (item.id && S.messages.some(m => m.id === item.id)) return;
    if (item.role === 'user') {
      const plain = stripAttach(item.msg);
      for (let i = S.messages.length - 1; i >= 0; i--) {
        const m = S.messages[i];
        if (m._local && m.role === 'user' && (stripAttach(m.msg) === plain || m.msg === item.msg)) {
          S.messages.splice(i, 1);
          break;
        }
      }
    }
    S.messages.push(item);
    if (item.role === 'conductor') S.conductorTyping = false;
    syncMessages();
    setConnUi();
  }

  function setWorkers(rawList) {
    S.workers = (rawList || []).map(normalizeWorker);
    renderWorkers();
    syncLayout();
  }

  function onWsData(data, gen) {
    if (gen !== wsGen) return;
    if (data.type === 'hello') {
      S.historyReady = true;
      S.messages = (data.chat || []).map(raw => ({ id: raw.id, role: raw.role || 'system', msg: raw.msg || '', ts: raw.ts, read: raw.read }));
      setWorkers(data.subagents || []);
      syncMessages();
      setConnUi();
    } else if (data.type === 'subagents') setWorkers(data.items || []);
    else if (data.type === 'chat') pushMsg({ id: data.item.id, role: data.item.role || 'system', msg: data.item.msg || '', ts: data.item.ts, read: data.item.read });
  }

  function resetWs() {
    wsGen++;
    if (!ws) return;
    const old = ws;
    ws = null;
    old.onopen = old.onclose = old.onerror = old.onmessage = null;
    try { old.close(); } catch {}
  }

  function scheduleReconnect() {
    clearTimeout(connectTimer);
    clearInterval(reconnectTick);
    if (!S.everConnected && S.failCount >= FAIL_MAX) {
      S.reconnecting = false;
      return setConnUi();
    }
    const delay = Math.min(RECON_MAX, RECON_BASE * Math.pow(2, Math.max(0, S.failCount - 1)));
    S.reconnectAt = Date.now() + delay;
    S.reconnecting = S.everConnected;
    setConnUi();
    reconnectTick = setInterval(() => { if (!S.reconnecting) clearInterval(reconnectTick); else setConnUi(); }, 500);
    connectTimer = setTimeout(connect, delay);
  }

  function connect() {
    if (ws && (ws.readyState === WebSocket.OPEN || ws.readyState === WebSocket.CONNECTING)) return;
    clearTimeout(connectTimer);
    clearInterval(reconnectTick);
    const gen = ++wsGen;
    setConnUi();
    let sock;
    try { sock = new WebSocket(wsUrl()); } catch (e) {
      if (gen !== wsGen) return;
      S.failCount++;
      return scheduleReconnect();
    }
    ws = sock;
    sock.onopen = () => {
      if (gen !== wsGen) return;
      S.everConnected = true;
      S.serviceAvailable = true;
      S.reconnecting = false;
      S.failCount = 0;
      setConnUi();
    };
    sock.onclose = (ev) => {
      if (gen !== wsGen) return;
      S.serviceAvailable = false;
      if (S.everConnected) S.reconnecting = true;
      else S.failCount++;
      setConnUi();
      scheduleReconnect();
    };
    sock.onerror = () => {};
    sock.onmessage = ev => {
      if (gen !== wsGen) return;
      try { onWsData(JSON.parse(ev.data), gen); } catch {}
    };
  }

  function sendText(rawText) {
    const text = (rawText || '').trim();
    if (!text || !ws || ws.readyState !== WebSocket.OPEN) return false;
    const expand = window.gaExpandFilePlaceholders || (s => s);
    const collect = window.gaCollectUsedFiles || (() => []);
    const clearUsed = window.gaClearUsedPendingFiles || (() => {});
    const used = collect(text);
    const images = [], files = [];
    for (const f of used) (f.isImage ? images : files).push(f.isImage ? { path: f.path, dataUrl: f.dataUrl } : { path: f.path, name: f.name });
    S.messages.push({ id: `_local_${++localSeq}`, _local: true, role: 'user', msg: text, ts: Date.now() / 1000, images, files });
    S.conductorTyping = true;
    syncMessages();
    ws.send(JSON.stringify({ msg: expand(text) }));
    clearUsed(text);
    const inp = $('collab-input');
    if (inp && inp.value.trim() === text) inp.value = '';
    setConnUi();
    return true;
  }

  function buildChips() {
    const box = $('collab-chips');
    if (box) box.innerHTML = CHIP_KEYS.map(k => `<button type="button" class="chip sm collab-chip" data-text-key="${k}">${esc(t(k))}</button>`).join('');
  }

  $('collab-send')?.addEventListener('click', () => sendText($('collab-input')?.value || ''));
  $('collab-retry')?.addEventListener('click', () => { S.failCount = 0; S.reconnecting = false; resetWs(); connect(); });
  $('collab-input')?.addEventListener('keydown', e => { if (e.key === 'Enter' && !e.shiftKey) { e.preventDefault(); sendText(e.target.value); } });
  $('collab-chips')?.addEventListener('click', e => { const k = e.target.closest('.collab-chip')?.dataset.textKey; if (k) sendText(t(k)); });
  $('collab-progress-toggle')?.addEventListener('click', () => { S.progressOpen = !S.progressOpen; syncProgressToggle(); });
  document.querySelector('[data-page="collab"]')?.addEventListener('click', e => {
    if (!S.progressOpen || e.target.closest('.collab-progress') || e.target.closest('#collab-progress-toggle')) return;
    S.progressOpen = false;
    syncProgressToggle();
  });

  window.collabInit = () => {
    window.gaSetActiveFileComposer?.('collab');
    buildChips();
    syncMessages();
    setConnUi();
    renderWorkers();
    connect();
  };
  window.collabFocus = () => $('collab-input')?.focus();
  window.collabRetranslate = () => { buildChips(); renderWorkers(); syncMessages(); setConnUi(); };
})();
