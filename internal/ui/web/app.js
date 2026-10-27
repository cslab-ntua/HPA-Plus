// app.js routes between the overview and one autoscaler's page, polls the
// read-only API and renders both. Text from the cluster only goes in as text.
(function () {
  'use strict';

  const C = window.HPACharts;
  const app = document.getElementById('app');
  const select = document.getElementById('namespace');
  const live = document.getElementById('live');
  const liveText = document.getElementById('live-text');
  const liveTime = document.getElementById('live-time');

  const OVERVIEW_EVERY = 10000;
  const DETAIL_EVERY = 5000;
  const TREND_EVERY = 30000;
  const LOG_LIMIT = 60;

  let generation = 0;
  let timer = null;
  let pending = null;
  let clockOffset = 0;
  let view = null;
  let namespaces = [];
  let namespacesAt = 0;
  let fixedNamespace = '';
  const trends = new Map();
  const logs = new Map();

  // ---------- small helpers

  function h(tag, props) {
    const el = document.createElement(tag);
    for (const key in props || {}) {
      const value = props[key];
      if (value === undefined || value === null || value === false) continue;
      if (key === 'class') el.className = value;
      else if (key === 'text') el.textContent = value;
      else el.setAttribute(key, value === true ? '' : value);
    }
    for (let i = 2; i < arguments.length; i++) append(el, arguments[i]);
    return el;
  }

  function append(el, child) {
    if (child === null || child === undefined || child === false) return;
    if (Array.isArray(child)) child.forEach((c) => append(el, c));
    else el.append(child instanceof Node ? child : String(child));
  }

  const now = () => Date.now() + clockOffset;
  const time = (value) => (value ? Date.parse(value) : NaN);

  function ago(value) {
    const t = time(value);
    return Number.isFinite(t) ? C.relative(t - now()) : '';
  }

  function plural(n, one, many) {
    return n + ' ' + (n === 1 ? one : many || one + 's');
  }

  const pods = (n) => plural(n, 'pod');
  const trim = (v, digits) => String(Number(v.toFixed(digits)));
  const cores = (v) => (v < 10 ? v.toFixed(2) : v.toFixed(1)) + ' cores';
  const rate = (v) => trim(v, v < 10 ? 2 : 1) + ' req/s';
  const latency = (v) => (v < 1 ? Math.round(v * 1000) + ' ms' : v.toFixed(2) + ' s');

  function chip(kind, text) {
    return h('span', { class: 'chip ' + kind, text: text });
  }

  const HEALTH = {
    healthy: ['healthy', 'Healthy'],
    learning: ['learning', 'Learning'],
    waiting: ['waiting', 'Waiting'],
    idle: ['waiting', 'Idle'],
    partial: ['waiting', 'Partial metrics'],
    attention: ['attention', 'Needs attention'],
  };

  function healthChip(health) {
    const [kind, label] = HEALTH[health] || ['plain', health];
    return chip(kind, label);
  }

  const MODES = { control: 'Controls pods', assist: 'Assists the standard autoscaler', observe: 'Watching only' };

  const DECIDED = {
    forecast: 'forecast',
    floor: 'demand now',
    hold: 'waiting for data',
    rising: 'demand rising',
    fast: 'sudden rise',
    postponed: 'rise postponed',
    predictive: 'forecast',
    mixed: 'forecast and CPU rule',
    baseline: 'CPU rule',
  };

  function decidedBy(d) {
    if (d.decidedBy && DECIDED[d.decidedBy]) return DECIDED[d.decidedBy];
    if (d.decision && d.decision.candidateMode === 'hold') return 'no forecast';
    return d.decision ? DECIDED[d.decision.candidateMode] || d.decision.candidateMode : '';
  }

  function link(namespace, name) {
    if (!namespace) return '#/';
    return '#/ns/' + encodeURIComponent(namespace) + (name ? '/' + encodeURIComponent(name) : '');
  }

  function parse() {
    const parts = location.hash.replace(/^#\/?/, '').split('/').filter(Boolean).map(decodeURIComponent);
    if (parts[0] === 'ns' && parts[1]) return { namespace: parts[1], name: parts[2] || '' };
    return { namespace: '', name: '' };
  }

  // ---------- theme

  function applyTheme(choice) {
    if (choice === 'light' || choice === 'dark') document.documentElement.dataset.theme = choice;
    else delete document.documentElement.dataset.theme;
    for (const button of document.querySelectorAll('[data-theme-choice]')) {
      button.setAttribute('aria-pressed', String(button.dataset.themeChoice === (choice || 'auto')));
    }
  }

  let theme = 'auto';
  try { theme = localStorage.getItem('hpaplus-theme') || 'auto'; } catch (e) { /* storage unavailable */ }
  applyTheme(theme);
  document.getElementById('theme').addEventListener('click', (event) => {
    const button = event.target.closest('[data-theme-choice]');
    if (!button) return;
    applyTheme(button.dataset.themeChoice);
    try { localStorage.setItem('hpaplus-theme', button.dataset.themeChoice); } catch (e) { /* storage unavailable */ }
  });

  // ---------- data

  async function api(path) {
    const controller = new AbortController();
    const timeout = setTimeout(() => controller.abort(), 20000);
    let response;
    try {
      response = await fetch(path, { signal: controller.signal, cache: 'no-store', headers: { Accept: 'application/json' } });
    } catch (e) {
      const err = new Error(e.name === 'AbortError' ? 'The cluster took too long to answer.' : 'The UI server stopped answering. Is make ui still running?');
      err.status = 0;
      throw err;
    } finally {
      clearTimeout(timeout);
    }
    let body = null;
    try { body = await response.json(); } catch (e) { body = null; }
    if (!response.ok) {
      const err = new Error((body && body.error) || response.status + ' ' + response.statusText);
      err.status = response.status;
      throw err;
    }
    return body;
  }

  function setLive(ok, text, detail) {
    live.classList.toggle('ok', ok);
    live.classList.toggle('err', !ok);
    if (liveText.textContent !== text) liveText.textContent = text;
    liveTime.textContent = ok ? C.clock(now(), true) : '';
    live.title = detail || '';
  }

  function schedule(fn, delay) {
    clearTimeout(timer);
    timer = setTimeout(() => {
      if (document.hidden) pending = fn;
      else fn();
    }, delay);
  }

  document.addEventListener('visibilitychange', () => {
    if (!document.hidden && pending) {
      const fn = pending;
      pending = null;
      fn();
    }
  });

  async function loadNamespaces(current, force) {
    if (force || Date.now() - namespacesAt > 30000) {
      namespacesAt = Date.now();
      try {
        const data = await api('api/hpapluses');
        fixedNamespace = data.fixedNamespace ? data.namespace : '';
        namespaces = Array.from(new Set(data.items.map((i) => i.namespace))).sort();
      } catch (e) {
        /* the select keeps what it has; the page shows its own errors */
      }
    }
    syncSelect(current);
  }

  function syncSelect(current) {
    const list = fixedNamespace ? [fixedNamespace] : Array.from(new Set(namespaces.concat(current ? [current] : []))).sort();
    const wanted = (fixedNamespace ? [] : ['']).concat(list);
    const have = Array.from(select.options).map((o) => o.value);
    if (wanted.join('\n') !== have.join('\n')) {
      select.replaceChildren.apply(select, wanted.map((ns) => h('option', { value: ns, text: ns || 'All namespaces' })));
    }
    select.value = fixedNamespace || current || '';
    select.disabled = Boolean(fixedNamespace);
  }

  select.addEventListener('change', () => { location.hash = link(select.value); });
  select.addEventListener('focus', () => loadNamespaces(parse().namespace));

  function problem(box, err, context) {
    let title = 'Could not read from the cluster.';
    let body = err.message + ' The page keeps the last data it had and tries again.';
    if (err.status === 403) {
      title = 'Your Kubernetes account is not allowed to read this.';
      body = context.all
        ? 'It may only be allowed in some namespaces. Start the UI for one namespace instead: make ui UI_NAMESPACE=<namespace>'
        : err.message;
    } else if (err.status === 404 && context.name) {
      title = 'There is no HPAPlus named ' + context.name + ' in ' + context.namespace + '.';
      body = 'It may have been deleted or renamed.';
    } else if (err.status === 0) {
      title = err.message;
      body = 'The page tries again on its own.';
    }
    box.replaceChildren(h('div', { class: 'banner', role: 'alert' }, h('strong', { text: title }), h('p', { text: body })));
  }

  // ---------- routing

  function route() {
    generation += 1;
    clearTimeout(timer);
    pending = null;
    const r = parse();
    sparks.disconnect();
    queue.length = 0;
    app.textContent = '';
    window.scrollTo(0, 0);
    if (r.name) startDetail(r.namespace, r.name, generation);
    else startOverview(r.namespace, generation);
  }

  window.addEventListener('hashchange', route);

  // ---------- overview

  function startOverview(namespace, g) {
    document.title = 'HPA+' + (namespace ? ' · ' + namespace : '');
    view = { kind: 'overview', cards: new Map(), sections: new Map() };
    view.title = h('h1', { text: namespace || 'Your autoscalers' });
    view.stats = h('div', { class: 'chips' });
    view.notice = h('div');
    view.groups = h('div', { class: 'groups' });
    app.append(
      h('section', { class: 'head' },
        h('p', { class: 'eyebrow', text: namespace ? 'Namespace' : 'HPA+ · read-only view' }),
        view.title,
        h('p', { class: 'lede', text: 'Each card is one HPAPlus. Open it to see what it decided, why, and the CPU and forecast behind the decision.' }),
        view.stats),
      view.notice,
      view.groups);
    syncSelect(namespace);
    const tick = async () => {
      try {
        const data = await api('api/hpapluses' + (namespace ? '?namespace=' + encodeURIComponent(namespace) : ''));
        if (g !== generation) return;
        clockOffset = Date.parse(data.now) - Date.now();
        if (!namespace) {
          fixedNamespace = data.fixedNamespace ? data.namespace : '';
          namespaces = Array.from(new Set(data.items.map((i) => i.namespace))).sort();
          namespacesAt = Date.now();
          syncSelect('');
        }
        renderOverview(data.items, namespace || (data.fixedNamespace ? data.namespace : ''));
        view.notice.textContent = '';
        setLive(true, 'Live');
      } catch (err) {
        if (g !== generation) return;
        setLive(false, 'Not updating', err.message);
        problem(view.notice, err, { all: !namespace && !fixedNamespace });
      }
      if (g === generation) schedule(tick, OVERVIEW_EVERY);
    };
    tick();
    if (namespace) loadNamespaces(namespace);
  }

  function renderOverview(items, namespace) {
    const count = (health) => items.filter((i) => i.health === health).length;
    const spaces = new Set(items.map((i) => i.namespace));
    view.stats.replaceChildren(
      chip('plain', plural(items.length, 'autoscaler')),
      !namespace && spaces.size > 1 ? chip('plain', plural(spaces.size, 'namespace')) : '',
      count('attention') ? chip('attention', count('attention') + ' need attention') : '',
      count('learning') ? chip('learning', count('learning') + ' learning') : '',
      count('idle') ? chip('waiting', count('idle') + ' idle') : '');
    if (!items.length) {
      view.cards.clear();
      view.sections.clear();
      view.groups.replaceChildren(h('div', { class: 'card empty' },
        h('h2', { text: 'No HPAPlus here yet' }),
        h('p', { class: 'muted', text: namespace
          ? 'There is no HPAPlus in ' + namespace + '. Pick another namespace above, or create one.'
          : 'Create an HPAPlus for a Deployment and it shows up here within a few seconds. The getting-started guide in the docs has an example.' })));
      return;
    }
    const groups = new Map();
    for (const item of items) {
      if (!groups.has(item.namespace)) groups.set(item.namespace, []);
      groups.get(item.namespace).push(item);
    }
    // Namespaces with something current come first, newest decision first;
    // namespaces where every autoscaler is idle go last.
    const newest = (list) => Math.max.apply(null, list.map((i) => time(i.evaluatedAt) || 0));
    const current = (list) => (list.some((i) => i.health !== 'idle') ? 1 : 0);
    const order = Array.from(groups.keys()).sort((a, b) =>
      current(groups.get(b)) - current(groups.get(a)) || newest(groups.get(b)) - newest(groups.get(a)) || a.localeCompare(b));
    const seen = new Set();
    const sections = order.map((ns) => {
      let section = view.sections.get(ns);
      if (!section) {
        section = { title: h('h2'), meta: h('p', { class: 'muted' }), grid: h('div', { class: 'cards' }) };
        section.head = h('div', { class: 'group-head' }, section.title, section.meta);
        section.el = h('section', { class: 'group' }, section.head, section.grid);
        view.sections.set(ns, section);
      }
      const list = groups.get(ns).slice().sort((a, b) => (a.health === 'idle') - (b.health === 'idle') || a.name.localeCompare(b.name));
      section.head.hidden = Boolean(namespace);
      section.title.replaceChildren(h('a', { href: link(ns), text: ns }));
      const active = list.filter((i) => i.health !== 'idle').length;
      section.meta.textContent = plural(list.length, 'autoscaler') + (active < list.length ? ' · ' + active + ' active' : '');
      section.grid.replaceChildren.apply(section.grid, list.map((item) => {
        seen.add(item.namespace + '/' + item.name);
        return card(item);
      }));
      return section.el;
    });
    for (const key of Array.from(view.cards.keys())) {
      if (!seen.has(key)) { sparks.unobserve(view.cards.get(key).el); view.cards.delete(key); }
    }
    for (const ns of Array.from(view.sections.keys())) if (!groups.has(ns)) view.sections.delete(ns);
    view.groups.replaceChildren.apply(view.groups, sections);
  }

  function card(item) {
    const key = item.namespace + '/' + item.name;
    let c = view.cards.get(key);
    if (!c) {
      c = { body: h('div', { class: 'hbody' }), spark: h('div', { class: 'spark' }), note: h('p', { class: 'spark-note' }) };
      c.el = h('a', { class: 'card hcard', href: link(item.namespace, item.name) }, c.body, h('div', { class: 'spark-wrap' }, c.spark, c.note));
      c.el.dataset.key = key;
      view.cards.set(key, c);
      sparks.observe(c.el);
    }
    c.el.dataset.models = String(item.models.length);
    const signature = JSON.stringify(item) + Math.floor(now() / 10000);
    if (c.signature !== signature) {
      c.signature = signature;
      c.body.replaceChildren.apply(c.body, cardBody(item));
    }
    drawTrend(key);
    if (c.visible) want(key);
    return c.el;
  }

  function cardBody(item) {
    const wanted = item.desired !== undefined && item.desired !== item.replicas;
    const foot = [];
    if (item.evaluatedAt) foot.push(h('span', { text: 'Decided ' + ago(item.evaluatedAt) }));
    const by = decidedBy(item);
    if (by && item.health !== 'idle') foot.push(h('span', { text: 'by ' + by }));
    if (item.mode !== 'control') foot.push(h('span', { text: MODES[item.mode] || item.mode }));
    return [
      h('div', { class: 'top' },
        h('div', null, h('h3', { text: item.name }), h('p', { class: 'where', text: item.target })),
        healthChip(item.health)),
      h('div', { class: 'pods' },
        h('span', { class: 'big', text: String(item.replicas) }),
        wanted ? h('span', { class: 'arrow', 'aria-hidden': 'true', text: '→' }) : '',
        wanted ? h('span', { class: 'big', text: String(item.desired) }) : '',
        h('span', { class: 'label', text: (wanted ? 'pods now → wanted' : item.replicas === 1 ? 'pod' : 'pods') + ' · range ' + item.min + '–' + item.max })),
      item.reason && item.health !== 'idle' ? h('p', { class: 'reason', text: item.reason }) : '',
      h('div', { class: 'foot' }, foot),
    ];
  }

  // Sparklines load only for cards on screen, a few at a time.
  const queue = [];
  let loading = 0;
  const sparks = new IntersectionObserver((entries) => {
    for (const entry of entries) {
      const c = view && view.cards && view.cards.get(entry.target.dataset.key);
      if (!c || c.el !== entry.target) continue;
      c.visible = entry.isIntersecting;
      if (c.visible) want(entry.target.dataset.key);
    }
  }, { rootMargin: '200px' });

  function want(key) {
    const entry = trends.get(key);
    if (entry && (entry.loading || now() - entry.at < TREND_EVERY)) return;
    if (!queue.includes(key)) queue.push(key);
    pump();
  }

  function pump() {
    while (loading < 4 && queue.length) {
      const key = queue.shift();
      const entry = trends.get(key) || {};
      entry.loading = true;
      trends.set(key, entry);
      loading += 1;
      const [ns, name] = key.split('/');
      api('api/hpapluses/' + encodeURIComponent(ns) + '/' + encodeURIComponent(name) + '/trend')
        .then((data) => { entry.samples = data.samples; entry.error = ''; })
        .catch((err) => { entry.error = err.message; })
        .finally(() => {
          entry.loading = false;
          entry.at = now();
          loading -= 1;
          drawTrend(key);
          pump();
        });
    }
  }

  function drawTrend(key) {
    const c = view && view.kind === 'overview' && view.cards.get(key);
    const entry = trends.get(key);
    if (!c) return;
    if (!entry || !entry.samples) {
      if (!c.spark.childNodes.length) c.spark.append(h('span', { class: 'spark-empty', text: entry && entry.error ? 'CPU history unavailable' : 'Loading CPU history…' }));
      return;
    }
    if (c.drawn === entry.at) return;
    c.drawn = entry.at;
    const samples = entry.samples;
    if (!samples.length) {
      const models = c.el.dataset.models === '0';
      c.spark.replaceChildren(h('span', { class: 'spark-empty', text: models ? 'No models, so HPA+ stores no CPU history' : 'No CPU history yet' }));
      c.note.textContent = '';
      return;
    }
    const points = samples.map((s) => [time(s.t), s.cores]);
    const capacity = samples.filter((s) => s.capacity !== undefined).map((s) => [time(s.t), s.capacity]);
    const last = samples[samples.length - 1];
    const minutes = samples.length > 1 ? Math.round((time(last.t) - time(samples[0].t)) / 60000) : 0;
    C.spark(c.spark, points, capacity, last ? 'CPU over the last ' + minutes + ' minutes, now ' + cores(last.cores) : 'No CPU history yet');
    c.note.textContent = 'CPU, last ' + minutes + ' min · ' + (last.capacity !== undefined ? number(last.cores) + ' of ' + cores(last.capacity) + ' at target' : cores(last.cores)) +
      (time(last.t) < now() - 600000 ? ' · ' + ago(last.t) : '');
  }

  // ---------- one autoscaler

  function startDetail(namespace, name, g) {
    document.title = name + ' · HPA+';
    const key = namespace + '/' + name;
    view = { kind: 'detail', key: key };
    view.eyebrow = h('p', { class: 'eyebrow' });
    view.chips = h('div', { class: 'chips' });
    view.notice = h('div');
    view.tiles = h('div', { class: 'tiles' });
    view.why = h('div', { class: 'card why' });
    view.charts = h('div', { class: 'charts' });
    view.log = h('div', { class: 'card' });
    view.models = h('div', { class: 'card' });
    view.conditions = h('div', { class: 'card' });
    view.figures = {};
    // The body stays hidden until the first answer, so a missing object shows
    // only its explanation.
    view.body = h('div', { class: 'stack', hidden: true },
      view.tiles,
      h('section', null, h('h2', { text: 'Why this many pods' }), view.why),
      h('section', null,
        h('div', { class: 'chart-head' },
          h('h2', { text: 'Recent history and forecast' }),
          h('p', { class: 'note', text: 'Times are in your time zone. The shaded part on the right is the future.' })),
        view.charts),
      h('div', { class: 'two' }, view.log, view.models),
      view.conditions,
      h('footer', { class: 'note', text: 'Read-only. This page reads the HPAPlus, its scale target and the history HPA+ stores in ConfigMaps, using your kubeconfig. It never changes the cluster.' }));
    app.append(
      h('section', { class: 'head' },
        h('a', { class: 'crumb', href: link(namespace), text: '← ' + namespace }),
        view.eyebrow,
        h('h1', { text: name }),
        view.chips),
      view.notice,
      view.body);
    syncSelect(namespace);
    loadNamespaces(namespace);
    const tick = async () => {
      try {
        const data = await api('api/hpapluses/' + encodeURIComponent(namespace) + '/' + encodeURIComponent(name));
        if (g !== generation) return;
        clockOffset = Date.parse(data.now) - Date.now();
        renderDetail(data);
        view.notice.textContent = '';
        setLive(true, 'Live');
      } catch (err) {
        if (g !== generation) return;
        setLive(false, 'Not updating', err.message);
        problem(view.notice, err, { namespace: namespace, name: name });
      }
      if (g === generation) schedule(tick, DETAIL_EVERY);
    };
    tick();
  }

  function renderDetail(d) {
    view.eyebrow.textContent = d.namespace + ' · ' + d.target;
    view.chips.replaceChildren(
      healthChip(d.health),
      chip('plain', MODES[d.mode] || d.mode),
      d.reason ? h('span', { class: 'chip-note', text: d.reason }) : '');
    view.body.hidden = false;
    const series = chartData(d);
    renderTiles(d, series);
    renderWhy(d);
    renderCharts(d, series);
    renderLog(d);
    renderModels(d);
    renderConditions(d);
  }

  function tile(label, value, sub, unit) {
    return h('div', { class: 'tile' },
      h('span', { class: 'label', text: label }),
      h('span', { class: 'value' }, value, unit ? h('span', { class: 'unit', text: unit }) : ''),
      sub ? h('span', { class: 'sub', text: sub }) : '');
  }

  const number = (v) => (v < 10 ? v.toFixed(2) : v.toFixed(1));

  function renderTiles(d, s) {
    const tiles = [];
    tiles.push(tile('Pods now', String(d.replicas), d.ready !== undefined ? d.ready + ' ready' : ''));
    if (d.decision) {
      tiles.push(tile('HPA+ wants', String(d.decision.desiredReplicas), (decidedBy(d) ? 'by ' + decidedBy(d) + ', ' : '') + ago(d.decision.evaluatedAt)));
    } else {
      tiles.push(tile('HPA+ wants', '–', 'no decision yet'));
    }
    // The tiles describe the last stored half-minute window; the decision may
    // have used a newer live window, so each tile says when its window ended.
    const lastCapacity = s.capacity.length ? s.capacity[s.capacity.length - 1][1] : undefined;
    const used = d.cpuCores !== undefined;
    const usedAt = s.used.length ? ', window to ' + C.clock(s.used[s.used.length - 1][0], true) : '';
    tiles.push(tile('CPU used', used ? number(d.cpuCores) : '–',
      (used && lastCapacity && d.capacity ? Math.round((100 * d.cpuCores) / lastCapacity) + '% of what the pods handle at their ' + d.capacity.targetPercent + '% target' : '') + usedAt, used ? 'cores' : ''));
    if (s.forecast.length) {
      const peak = Math.max.apply(null, s.forecast.map((w) => w.value));
      const minutes = Math.max(1, Math.round((s.forecast[s.forecast.length - 1].until - now()) / 60000));
      tiles.push(tile('Forecast peak', number(peak),
        'next ' + minutes + ' min' + (s.forecastPods !== undefined ? ', asks for ' + pods(s.forecastPods) : ''), s.forecastUnit === 'cores' ? 'cores' : 'req/s'));
    }
    if (s.arrivals.length) {
      const last = s.arrivals[s.arrivals.length - 1];
      const p95 = s.latency.length ? s.latency[s.latency.length - 1].value : undefined;
      tiles.push(tile('Requests', trim(last.value, last.value < 10 ? 2 : 1), (p95 !== undefined ? '95% answered within ' + latency(p95) : '') + ', window to ' + C.clock(last.until, true), 'req/s'));
    }
    tiles.push(tile('Allowed range', d.min + '–' + d.max, '', 'pods'));
    view.tiles.replaceChildren.apply(view.tiles, tiles);
  }

  // ---------- why

  const CONSTRAINTS = {
    stabilization: () => 'It waits a while before removing pods, so a short dip does not make the count flap.',
    rate_limit: () => 'It can only add or remove so many pods per period, so it gets there in steps.',
    disabled: () => 'Scaling in this direction is turned off in the behavior settings.',
    minimum: (d) => 'It never goes below the minimum of ' + pods(d.min) + '.',
    maximum: (d) => 'It cannot go above the maximum of ' + pods(d.max) + '.',
    partial_metrics: () => 'Some CPU numbers were missing, so it does not remove pods until they are back.',
    assist_baseline: (d) => 'In assist mode it never goes below what the standard autoscaler would choose (' + pods(d.decision.baselineReplicas) + ').',
    prediction_unavailable_hold: () => 'No forecast was available, so it kept the pods it had.',
  };

  function mainReason(d) {
    const dec = d.decision;
    const model = d.models.find((m) => m.decidedBy);
    const want = dec.desiredReplicas;
    if (model) {
      const forecast = model.forecastOnly || 0;
      const floor = model.floorReplicas || 0;
      switch (model.decidedBy) {
        case 'forecast':
          return (forecast ? 'The forecast for the next few minutes needs ' + pods(forecast) + (floor && floor < forecast ? ', more than the ' + floor + ' that demand right now needs' : '') + '. ' : 'HPA+ follows the forecast for the next few minutes. ') + 'HPA+ plans for ' + pods(want) + '.';
        case 'floor':
          return (floor ? 'Demand right now needs ' + pods(floor) + (forecast && forecast < floor ? ', more than the forecast’s ' + forecast : '') + '. ' : 'Demand right now needs more than the forecast. ') + 'HPA+ keeps enough for what is happening now: ' + pods(want) + '.';
        case 'hold':
          return 'The newest measurement window has not been read yet, so HPA+ keeps ' + pods(want) + ' until it has.';
        case 'rising':
          return 'Demand is still rising, so HPA+ is not removing pods yet and stays at ' + pods(want) + '.';
        case 'fast':
          return 'The last few seconds needed more pods than planned, so HPA+ asked for ' + pods(want) + ' right away.';
        case 'postponed':
          return 'A newer forecast moved an expected rise later. HPA+ keeps ' + pods(want) + ' until the earlier forecast’s window ends, in case the rise comes on time.';
      }
    }
    const evidence = dec.evidence || {};
    const candidates = (evidence.candidates || []).map((c) => c.model + ' asks for ' + c.replicas);
    switch (dec.candidateMode) {
      case 'predictive':
        return (candidates.length ? 'The forecast' + (candidates.length > 1 ? 's say: ' : ' says: ') + candidates.join(', ') + '. ' : '') + 'HPA+ plans for ' + pods(want) + '.';
      case 'mixed':
        return 'HPA+ compared the forecast (' + (candidates.join(', ') || 'none') + ') with the standard CPU rule (' + dec.baselineReplicas + ') and plans for ' + pods(want) + '.';
      case 'baseline':
        return 'No forecast is in use yet, so HPA+ follows the standard CPU rule: ' + pods(dec.baselineReplicas) + '.';
      case 'hold':
        return 'No forecast is available right now, so HPA+ keeps the ' + pods(dec.requestedReplicas) + ' it has.';
    }
    return 'HPA+ plans for ' + pods(want) + '.';
  }

  function writeResult(dec) {
    const from = dec.requestedReplicas;
    const to = dec.desiredReplicas;
    switch (dec.scaleWrite) {
      case 'accepted': return from !== to ? 'It changed the pod count from ' + from + ' to ' + to + '.' : 'It set the pod count to ' + to + '.';
      case 'unchanged': return 'No change was needed: the workload already has ' + pods(to) + '.';
      case 'observed': return 'Nothing was changed, because this HPAPlus only watches.';
      case 'rejected': return 'Kubernetes rejected the change. The operator’s logs say why.';
      case 'not_attempted': return 'No change was attempted in this round.';
      case 'unknown': return 'It could not confirm whether the change went through; it checks again next round.';
    }
    return '';
  }

  function renderWhy(d) {
    const dec = d.decision;
    if (!dec) {
      view.why.replaceChildren(h('p', { class: 'say', text: 'HPA+ has not made a decision yet. It first needs some CPU history; this page fills in as it learns.' }));
      return;
    }
    const notes = [];
    if (dec.mode === 'observe') notes.push('This HPAPlus only watches: it works out a pod count but does not change it.');
    for (const reason of (dec.evidence && dec.evidence.constraints) || []) notes.push((CONSTRAINTS[reason] || (() => 'Limit applied: ' + reason))(d));
    const result = writeResult(dec);
    view.why.replaceChildren(
      h('p', { class: 'say', text: mainReason(d) }),
      notes.length ? h('ul', null, notes.map((n) => h('li', { text: n }))) : '',
      h('p', { class: 'muted', text: [result, 'Decided at ' + C.clock(time(dec.evaluatedAt), true) + ' (' + ago(dec.evaluatedAt) + ').'].filter(Boolean).join(' ') }));
  }

  // ---------- charts

  function chartData(d) {
    let primary = null;
    for (const s of d.series || []) {
      const h0 = s.history || [];
      if (!h0.length) continue;
      if (!primary || time(h0[h0.length - 1].t) > time(primary.history[primary.history.length - 1].t)) primary = s;
    }
    const history = primary ? primary.history : [];
    const demand = (d.demand || [])[0];
    const windows = demand ? demand.windows : [];
    const pick = (field) => windows.filter((w) => w[field] !== undefined && w[field] !== null).map((w) => ({ from: time(w.at), until: time(w.until), value: w[field] }));
    // The forecast drawn is the one the pod count came from: a request-demand
    // model's own, else the first model's cached CPU trajectory. Its pods are
    // the count HPA+ reported, which already includes tolerance and timing.
    let forecast = [];
    let forecastUnit = 'cores';
    let forecastModel = '';
    if (demand && demand.forecast && demand.forecast.length) {
      forecast = demand.forecast.map((p) => ({ from: time(p.at), until: time(p.until), value: p.value }));
      forecastUnit = demand.target === 'reconstructedCPU' ? 'cores' : 'requests';
      forecastModel = demand.model;
    } else {
      const withForecast = (d.series || []).find((s) => s.forecast && s.forecast.length);
      if (withForecast) {
        forecast = withForecast.forecast.map((p) => ({ from: time(p.at), until: time(p.until), value: p.value }));
        forecastModel = withForecast.model;
      }
    }
    const model = d.models.find((m) => m.name === forecastModel);
    const forecastPods = model ? (model.forecastOnly || model.forecastReplicas) : undefined;
    const nowT = time(d.now);
    let start = Infinity;
    if (history.length) start = Math.min(start, time(history[0].t));
    if (windows.length) start = Math.min(start, time(windows[0].at));
    if (!Number.isFinite(start)) start = nowT - 15 * 60000;
    start = Math.max(start, nowT - 2 * 3600000);
    let end = nowT + 30000;
    for (const w of forecast) end = Math.max(end, w.until);
    return {
      domain: [start, end],
      now: nowT,
      used: history.map((s) => [time(s.t), s.cores]),
      capacity: history.filter((s) => s.capacity !== undefined).map((s) => [time(s.t), s.capacity]),
      limit: d.limitCoresPerPod ? history.map((s) => [time(s.t), s.replicas * d.limitCoresPerPod]).concat(history.length ? [[nowT, d.replicas * d.limitCoresPerPod]] : []) : [],
      replicas: history.map((s) => [time(s.t), s.replicas]).concat(history.length ? [[nowT, d.replicas]] : []),
      needed: pick('needed'),
      arrivals: pick('arrivals'),
      completions: pick('completions'),
      latency: pick('latencyP95'),
      errors: pick('errors'),
      forecast: forecast,
      forecastUnit: forecastUnit,
      forecastPods: forecastPods,
      historyError: d.historyError,
    };
  }

  function figure(id, title, note) {
    let f = view.figures[id];
    if (!f) {
      f = { note: h('p', { class: 'note' }) };
      f.el = h('figure', { class: 'card chart' }, h('div', { class: 'chart-head' }, h('h3', { text: title }), f.note));
      view.figures[id] = f;
    }
    f.note.textContent = note || '';
    return f;
  }

  function renderCharts(d, s) {
    const shown = [];
    const marks = (d.markers || []).map((m) => ({ t: time(m.at), label: m.label }));
    const common = { now: s.now, domain: s.domain, group: 'detail', markers: marks };
    const tick = (v) => trim(v, 2);

    const notes = [];
    if (d.capacity) notes.push('Capacity at target is what the pods can use at their ' + d.capacity.targetPercent + '% CPU target.');
    if (d.limitCoresPerPod) notes.push('The limit is the pods’ CPU limit, ' + number(d.limitCoresPerPod) + (d.limitCoresPerPod === 1 ? ' core' : ' cores') + ' each.');
    else if (s.used.length) notes.push('The pods have no CPU limit.');
    const cpu = figure('cpu', 'CPU (cores)', notes.join(' '));
    const cpuSeries = [{ name: 'CPU used', color: '--s-cpu', points: s.used, area: true }];
    if (s.capacity.length) cpuSeries.push({ name: 'Capacity at target', color: '--s-cap', points: s.capacity, step: true, width: 1.5 });
    if (s.limit.length) cpuSeries.push({ name: 'CPU limit', color: '--s-limit', points: s.limit, step: true, width: 1.5 });
    if (s.forecast.length && s.forecastUnit === 'cores') cpuSeries.push({ name: 'Forecast', color: '--s-forecast', windows: s.forecast, dash: true });
    C.render(cpu.el, Object.assign({}, common, {
      series: cpuSeries, format: cores, tick: tick, height: 250,
      label: 'CPU used' + (s.used.length ? ', now ' + cores(s.used[s.used.length - 1][1]) : '') + (s.forecast.length && s.forecastUnit === 'cores' ? ', forecast up to ' + cores(Math.max.apply(null, s.forecast.map((w) => w.value))) : ''),
      empty: s.historyError ? 'Could not read the stored history: ' + s.historyError : 'No CPU history yet. HPA+ stores a sample every sync period, so this fills in within minutes.',
    }));
    shown.push(cpu.el);

    const asks = s.forecastPods !== undefined && s.forecast.length;
    const podsFigure = figure('pods', 'Pods', asks ? 'The dashed line is the pod count the newest forecast asks for, over the minutes it covers.' : '');
    const podSeries = [{ name: 'Pods', color: '--s-pods', points: s.replicas, step: true }];
    if (asks) {
      podSeries.push({ name: 'Forecast asks for', color: '--s-forecast', dash: true, windows: [{ from: Math.max(s.now, s.forecast[0].from), until: s.forecast[s.forecast.length - 1].until, value: s.forecastPods }] });
    }
    C.render(podsFigure.el, Object.assign({}, common, {
      series: podSeries, integer: true, format: pods, height: 200, minMax: d.min,
      label: 'Pods over time, now ' + d.replicas, empty: 'No pod history yet.',
    }));
    shown.push(podsFigure.el);

    if (s.arrivals.length || s.completions.length) {
      const req = figure('requests', 'Requests per second', 'Arriving requests and the requests the pods finished, per measurement window.');
      const reqSeries = [{ name: 'Arriving', color: '--s-arrivals', windows: s.arrivals, width: 3 }, { name: 'Finished', color: '--s-done', windows: s.completions, width: 1.25 }];
      if (s.forecast.length && s.forecastUnit === 'requests') reqSeries.push({ name: 'Forecast', color: '--s-forecast', windows: s.forecast, dash: true });
      C.render(req.el, Object.assign({}, common, { series: reqSeries, format: rate, tick: tick, height: 200, label: 'Requests per second' }));
      shown.push(req.el);
    }
    if (s.latency.length) {
      const lat = figure('latency', 'Response time', '95% of requests in each window were answered within this time.');
      C.render(lat.el, Object.assign({}, common, {
        series: [{ name: '95th percentile', color: '--s-latency', windows: s.latency }],
        format: latency, tick: (v) => (v < 1 ? Math.round(v * 1000) + 'ms' : trim(v, 2) + 's'), height: 180, label: 'Response time, 95th percentile',
      }));
      shown.push(lat.el);
    }
    if (s.errors.length) {
      const err = figure('errors', 'Failed requests per second', '');
      C.render(err.el, Object.assign({}, common, {
        series: [{ name: 'Failed', color: '--s-errors', windows: s.errors }],
        format: (v) => trim(v, 3) + ' /s', tick: tick, height: 180, label: 'Failed requests per second',
      }));
      shown.push(err.el);
    }
    const [first, second] = shown;
    const rest = shown.slice(2);
    const rows = [first, h('div', { class: 'two' }, second, rest[0] || '')];
    if (rest.length > 1) rows.push(h('div', { class: 'two' }, rest.slice(1)));
    if (!sameChildren(view.charts, shown)) view.charts.replaceChildren.apply(view.charts, rows);
  }

  function sameChildren(container, figures) {
    const current = Array.from(container.querySelectorAll('figure'));
    return current.length === figures.length && current.every((f, i) => f === figures[i]);
  }

  // ---------- decisions, models and checks

  function renderLog(d) {
    const log = logs.get(view.key) || [];
    logs.set(view.key, log);
    const dec = d.decision;
    if (dec && (!log.length || log[0].id !== dec.id || log[0].at !== dec.evaluatedAt)) {
      log.unshift({ id: dec.id, at: dec.evaluatedAt, from: dec.requestedReplicas, to: dec.desiredReplicas, by: decidedBy(d), write: dec.scaleWrite, limits: (dec.evidence && dec.evidence.constraints) || [] });
      log.length = Math.min(log.length, LOG_LIMIT);
    }
    const WRITES = { accepted: 'changed', unchanged: 'no change', observed: 'watched', rejected: 'rejected', not_attempted: 'not tried', unknown: 'unconfirmed' };
    const rows = log.map((e) => h('tr', null,
      h('td', { class: 'mono', text: C.clock(time(e.at), true) }),
      h('td', { class: 'n mono', text: e.from === e.to ? String(e.to) : e.from + ' → ' + e.to }),
      h('td', { text: e.by || '–' }),
      h('td', { text: WRITES[e.write] || e.write || '–' }),
      h('td', { class: 'muted', text: e.limits.join(', ') || '–' })));
    view.log.replaceChildren(
      h('h2', { text: 'Decisions while this page is open' }),
      h('p', { class: 'muted', text: 'HPA+ keeps only its latest decision, so this list grows while you watch.' }),
      log.length
        ? h('div', { class: 'scroll' }, h('table', null,
          h('thead', null, h('tr', null, h('th', { text: 'Time' }), h('th', { class: 'n', text: 'Pods' }), h('th', { text: 'Decided by' }), h('th', { text: 'Result' }), h('th', { text: 'Limits' }))),
          h('tbody', null, rows)))
        : h('p', { class: 'muted', text: 'No decision yet.' }));
  }

  const STATE = { Ready: 'healthy', Cached: 'healthy', Scheduled: 'healthy', Waiting: 'learning', WarmingUp: 'learning' };

  function renderModels(d) {
    const rows = d.models.map((m) => h('tr', null,
      h('td', null, h('strong', { text: m.name }), m.type ? h('div', { class: 'muted', text: m.type }) : ''),
      h('td', null, chip(STATE[m.state] || 'attention', m.state || 'unknown')),
      h('td', { class: 'n mono', text: m.samples + ' / ' + m.required }),
      h('td', { class: 'n mono', text: m.forecastReplicas !== undefined ? String(m.forecastReplicas) : '–' }),
      h('td', { text: m.lastSuccess ? ago(m.lastSuccess) : m.lastRunOutcome || '–' })));
    view.models.replaceChildren(
      h('h2', { text: 'Models' }),
      h('p', { class: 'muted', text: 'Each model learns from the stored CPU history and forecasts the next minutes.' }),
      d.models.length
        ? h('div', { class: 'scroll' }, h('table', null,
          h('thead', null, h('tr', null, h('th', { text: 'Model' }), h('th', { text: 'State' }), h('th', { class: 'n', text: 'Samples' }), h('th', { class: 'n', text: 'Forecast pods' }), h('th', { text: 'Last forecast' }))),
          h('tbody', null, rows)))
        : h('p', { class: 'muted', text: 'No model reported yet. Without a model HPA+ follows the standard CPU rule.' }));
  }

  const CHECKS = {
    ConfigurationValid: 'Configuration',
    SamplingActive: 'CPU sampling',
    MetricsAvailable: 'Metrics',
    PredictionsAvailable: 'Forecasts',
    ScalingActive: 'Scaling',
    HistoryCapacityAvailable: 'History storage',
    MetricStatusComplete: 'Metric report',
    ModelStatusComplete: 'Model report',
  };

  function renderConditions(d) {
    const rows = (d.conditions || []).map((c) => {
      let state = c.status === 'True' ? chip('healthy', 'OK') : c.status === 'False' ? chip('attention', 'Problem') : chip('waiting', 'Unknown');
      if (c.type === 'ScalingActive' && c.reason === 'ObserveOnly') state = chip('plain', 'Watching only');
      return h('tr', null,
        h('td', { text: CHECKS[c.type] || c.type }),
        h('td', null, state),
        h('td', { text: c.message || c.reason }),
        h('td', { class: 'muted', text: ago(c.lastTransitionTime) }));
    });
    view.conditions.replaceChildren(
      h('h2', { text: 'Checks' }),
      h('p', { class: 'muted', text: 'What the operator reported about this HPAPlus in its latest round.' }),
      rows.length
        ? h('div', { class: 'scroll' }, h('table', null,
          h('thead', null, h('tr', null, h('th', { text: 'Check' }), h('th', { text: 'State' }), h('th', { text: 'Details' }), h('th', { text: 'Since' }))),
          h('tbody', null, rows)))
        : h('p', { class: 'muted', text: 'The operator has not reported yet.' }));
  }

  route();
})();
