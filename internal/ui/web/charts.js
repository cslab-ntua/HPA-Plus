// charts.js draws the UI's time-series charts as SVG. Series colours are CSS
// custom properties, so a theme switch restyles a chart without redrawing it.
(function () {
  'use strict';

  const SVG = 'http://www.w3.org/2000/svg';
  const MARGIN = { top: 18, right: 14, bottom: 26, left: 50 };
  const TIME_STEPS = [5, 10, 15, 30, 60, 120, 300, 600, 900, 1800, 3600, 7200, 10800, 21600, 43200, 86400].map((s) => s * 1000);
  const groups = new Map();

  function node(name, attrs, parent) {
    const el = document.createElementNS(SVG, name);
    for (const key in attrs) el.setAttribute(key, attrs[key]);
    if (parent) parent.appendChild(el);
    return el;
  }

  function html(tag, className, parent) {
    const el = document.createElement(tag);
    if (className) el.className = className;
    if (parent) parent.appendChild(el);
    return el;
  }

  function clock(t, seconds) {
    return new Date(t).toLocaleTimeString([], seconds ? { hour: '2-digit', minute: '2-digit', second: '2-digit' } : { hour: '2-digit', minute: '2-digit' });
  }

  function spacing(times) {
    if (times.length < 2) return 30000;
    const gaps = [];
    for (let i = 1; i < times.length; i++) gaps.push(times[i] - times[i - 1]);
    gaps.sort((a, b) => a - b);
    return Math.max(1000, gaps[Math.floor(gaps.length / 2)]);
  }

  // A series is points ([t, v] pairs, drawn as a line or as steps that hold
  // until the next point) or windows ({from, until, value}, drawn as stairs).
  function prepare(series) {
    return series.map((s) => {
      const out = Object.assign({}, s);
      if (s.windows) {
        out.windows = s.windows.filter((w) => Number.isFinite(w.value)).sort((a, b) => a.from - b.from);
        out.times = out.windows.map((w) => (w.from + w.until) / 2);
      } else {
        out.points = (s.points || []).filter((p) => Number.isFinite(p[1])).sort((a, b) => a[0] - b[0]);
        out.times = out.points.map((p) => p[0]);
        out.gap = spacing(out.times);
      }
      return out;
    });
  }

  function valueAt(s, t) {
    if (s.windows) {
      for (const w of s.windows) if (t >= w.from && t < w.until) return w.value;
      return null;
    }
    const p = s.points;
    if (!p.length) return null;
    let lo = 0;
    let hi = p.length - 1;
    while (lo < hi) {
      const mid = (lo + hi + 1) >> 1;
      if (p[mid][0] <= t) lo = mid; else hi = mid - 1;
    }
    if (s.step) {
      if (t < p[0][0] || t > p[p.length - 1][0] + s.gap) return null;
      return p[lo][1];
    }
    let best = lo;
    if (lo + 1 < p.length && Math.abs(p[lo + 1][0] - t) < Math.abs(p[lo][0] - t)) best = lo + 1;
    return Math.abs(p[best][0] - t) <= s.gap ? p[best][1] : null;
  }

  // valueScale picks about four round gridlines and a top that is one of them.
  function valueScale(max, integer) {
    const raw = max > 0 ? max * 1.08 : integer ? 2 : 1;
    const rough = raw / 4;
    const mag = Math.pow(10, Math.floor(Math.log10(rough)));
    let step = [1, 2, 2.5, 5, 10].map((s) => s * mag).find((s) => s >= rough) || 10 * mag;
    if (integer) step = Math.max(1, Math.ceil(step));
    const count = Math.ceil(raw / step - 1e-9);
    const ticks = [];
    for (let i = 0; i <= count; i++) ticks.push(Math.round(i * step * 1e6) / 1e6);
    return { max: ticks[ticks.length - 1], ticks: ticks };
  }

  function timeTicks(start, end, width) {
    const most = Math.max(2, Math.floor(width / 92));
    const step = TIME_STEPS.find((s) => (end - start) / s <= most) || TIME_STEPS[TIME_STEPS.length - 1];
    const offset = -new Date(start).getTimezoneOffset() * 60000;
    const ticks = [];
    for (let t = Math.ceil((start + offset) / step) * step - offset; t <= end; t += step) ticks.push(t);
    return { ticks, seconds: step < 60000 };
  }

  function linePath(s, x, y, start, end) {
    let d = '';
    let prev = null;
    for (const [t, v] of s.points) {
      if (t < start - s.gap || t > end + s.gap) continue;
      const broken = prev === null || t - prev[0] > s.gap * 3;
      if (s.step) {
        if (broken) d += 'M' + x(t).toFixed(1) + ',' + y(v).toFixed(1);
        else d += 'H' + x(t).toFixed(1) + 'V' + y(v).toFixed(1);
      } else {
        d += (broken ? 'M' : 'L') + x(t).toFixed(1) + ',' + y(v).toFixed(1);
      }
      prev = [t, v];
    }
    if (s.step && prev) d += 'H' + x(Math.min(end, prev[0] + s.gap)).toFixed(1);
    return d;
  }

  function areaPath(s, x, y, start, end) {
    const runs = [];
    let run = [];
    let prev = null;
    for (const p of s.points) {
      if (p[0] < start - s.gap || p[0] > end + s.gap) continue;
      if (prev !== null && p[0] - prev > s.gap * 3) { runs.push(run); run = []; }
      run.push(p);
      prev = p[0];
    }
    runs.push(run);
    const base = y(0).toFixed(1);
    return runs.filter((r) => r.length > 1).map((r) =>
      'M' + x(r[0][0]).toFixed(1) + ',' + base + r.map((p) => 'L' + x(p[0]).toFixed(1) + ',' + y(p[1]).toFixed(1)).join('') +
      'L' + x(r[r.length - 1][0]).toFixed(1) + ',' + base + 'Z').join('');
  }

  function stairPath(s, x, y, start, end) {
    let d = '';
    let prevUntil = null;
    for (const w of s.windows) {
      if (w.until < start || w.from > end) continue;
      const from = x(Math.max(start, w.from)).toFixed(1);
      const until = x(Math.min(end, w.until)).toFixed(1);
      const level = y(w.value).toFixed(1);
      if (prevUntil !== null && Math.abs(w.from - prevUntil) < 1500) d += 'V' + level + 'H' + until;
      else d += 'M' + from + ',' + level + 'H' + until;
      prevUntil = w.until;
    }
    return d;
  }

  function paint(el, color, fill, opacity) {
    if (fill) {
      el.style.fill = 'var(' + color + ')';
      el.style.fillOpacity = opacity;
    } else {
      el.style.stroke = 'var(' + color + ')';
    }
  }

  function legend(container, series) {
    let box = container.querySelector(':scope > .legend');
    if (series.length < 2) {
      if (box) box.remove();
      return;
    }
    if (!box) box = html('div', 'legend', null);
    box.textContent = '';
    for (const s of series) {
      const item = html('span', '', box);
      const key = html('span', s.dash ? 'key dash' : 'key', item);
      if (s.dash) key.style.borderTopColor = 'var(' + s.color + ')';
      else key.style.background = 'var(' + s.color + ')';
      item.appendChild(document.createTextNode(s.name));
    }
    const plot = container.querySelector(':scope > .plot');
    container.insertBefore(box, plot);
  }

  // render draws or redraws a chart into container. Options: series, format(v),
  // integer, now (ms), domain [start, end], group (shared crosshair), label, empty.
  function render(container, opts) {
    container._chart = opts;
    let plot = container.querySelector(':scope > .plot');
    if (!plot) {
      plot = html('div', 'plot', container);
      observe(container);
    }
    const series = prepare(opts.series || []);
    const drawn = series.filter((s) => (s.windows || s.points).length);
    legend(container, series.filter((s) => !s.hideLegend));
    if (!drawn.length) {
      plot.textContent = '';
      html('p', 'chart-empty', plot).textContent = opts.empty || 'No data yet.';
      container._state = null;
      return;
    }

    let start = Infinity;
    let end = -Infinity;
    for (const s of drawn) {
      if (s.windows) { start = Math.min(start, s.windows[0].from); end = Math.max(end, s.windows[s.windows.length - 1].until); }
      else { start = Math.min(start, s.points[0][0]); end = Math.max(end, s.points[s.points.length - 1][0]); }
    }
    if (opts.domain) [start, end] = opts.domain;
    if (Number.isFinite(opts.now)) end = Math.max(end, opts.now);
    if (end - start < 60000) start = end - 60000;

    let max = 0;
    for (const s of drawn) {
      if (s.windows) { for (const w of s.windows) if (w.until >= start && w.from <= end) max = Math.max(max, w.value); }
      else { for (const p of s.points) if (p[0] >= start && p[0] <= end) max = Math.max(max, p[1]); }
    }
    const scale = valueScale(Math.max(max, opts.minMax || 0), opts.integer);
    max = scale.max;

    const available = Math.round(plot.clientWidth || container.clientWidth || 640);
    container._renderedWidth = available;
    const width = Math.max(280, available);
    const height = opts.height || 220;
    const left = MARGIN.left;
    const right = width - MARGIN.right;
    const top = MARGIN.top;
    const bottom = height - MARGIN.bottom;
    const x = (t) => left + ((t - start) / (end - start)) * (right - left);
    const y = (v) => bottom - (v / max) * (bottom - top);

    const svg = node('svg', { viewBox: '0 0 ' + width + ' ' + height, width: width, height: height, role: 'img', tabindex: '0', 'aria-label': opts.label || '' });
    const clipId = 'clip-' + Math.random().toString(36).slice(2, 9);
    const clip = node('clipPath', { id: clipId }, node('defs', {}, svg));
    node('rect', { x: left, y: top - 4, width: right - left, height: bottom - top + 8 }, clip);

    if (Number.isFinite(opts.now) && opts.now < end) {
      node('rect', { class: 'future', x: x(Math.max(start, opts.now)), y: top, width: right - x(Math.max(start, opts.now)), height: bottom - top }, svg);
    }
    for (const v of scale.ticks) {
      node('line', { class: v === 0 ? 'baseline' : 'gridline', x1: left, x2: right, y1: y(v), y2: y(v) }, svg);
      node('text', { x: left - 8, y: y(v) + 4, 'text-anchor': 'end' }, svg).textContent = opts.tick ? opts.tick(v) : String(v);
    }
    const times = timeTicks(start, end, right - left);
    for (const t of times.ticks) {
      node('line', { class: 'tick', x1: x(t), x2: x(t), y1: bottom, y2: bottom + 4 }, svg);
      node('text', { x: x(t), y: height - 6, 'text-anchor': 'middle' }, svg).textContent = clock(t, times.seconds);
    }

    const marks = node('g', { 'clip-path': 'url(#' + clipId + ')' }, svg);
    for (const s of drawn) {
      if (s.area && s.points) paint(node('path', { class: 'area', d: areaPath(s, x, y, start, end) }, marks), s.color, true, 0.14);
    }
    for (const s of drawn) {
      const d = s.windows ? stairPath(s, x, y, start, end) : linePath(s, x, y, start, end);
      const path = node('path', { class: 'series', d: d, fill: 'none', 'stroke-width': s.width || 2 }, marks);
      if (s.dash) path.setAttribute('stroke-dasharray', '6 4');
      paint(path, s.color);
    }
    for (const s of drawn) {
      if (s.points && !s.step && s.points.length) {
        const last = s.points[s.points.length - 1];
        if (last[0] >= start && last[0] <= end) paint(node('circle', { class: 'end', cx: x(last[0]), cy: y(last[1]), r: 3.5 }, marks), s.color, true, 1);
      }
    }
    // Markers are labelled lines; a label that would overlap another moves
    // down a row, and one near the right edge sits left of its line.
    const placed = [];
    for (const m of opts.markers || []) {
      if (!(m.t > start && m.t < end)) continue;
      const mx = x(m.t);
      node('line', { class: 'marker', x1: mx, x2: mx, y1: top, y2: bottom }, svg);
      const w = m.label.length * 6.7 + 4;
      const before = mx + 5 + w > right;
      const x0 = before ? mx - 5 - w : mx + 5;
      let row = 0;
      while (placed.some((p) => p.row === row && x0 < p.x1 && x0 + w > p.x0)) row++;
      placed.push({ row: row, x0: x0, x1: x0 + w });
      node('text', { class: 'marker-label', x: before ? mx - 5 : mx + 5, y: top + 12 + row * 14, 'text-anchor': before ? 'end' : 'start' }, svg).textContent = m.label;
    }
    if (Number.isFinite(opts.now) && opts.now >= start && opts.now <= end) {
      node('line', { class: 'now', x1: x(opts.now), x2: x(opts.now), y1: top - 6, y2: bottom }, svg);
      node('text', { class: 'now-label', x: x(opts.now), y: top - 8, 'text-anchor': 'middle' }, svg).textContent = 'now';
    }

    const cross = node('g', { class: 'cross', visibility: 'hidden' }, svg);
    node('line', { x1: 0, x2: 0, y1: top, y2: bottom }, cross);
    const dots = drawn.map((s) => {
      const dot = node('circle', { class: 'dot', r: 4.5, cx: 0, cy: 0 }, cross);
      paint(dot, s.color, true, 1);
      return dot;
    });
    const hit = node('rect', { class: 'hit', x: left, y: top, width: right - left, height: bottom - top }, svg);

    plot.textContent = '';
    plot.appendChild(svg);
    const tip = html('div', 'tip', plot);
    tip.hidden = true;

    const snaps = Array.from(new Set(drawn.flatMap((s) => s.times))).filter((t) => t >= start && t <= end).sort((a, b) => a - b);
    const state = { svg, cross, dots, tip, drawn, x, y, start, end, snaps, opts, width, top, left, right, index: -1 };
    container._state = state;

    const fromEvent = (event) => {
      const box = svg.getBoundingClientRect();
      const px = ((event.clientX - box.left) / box.width) * width;
      return start + ((px - left) / (right - left)) * (end - start);
    };
    hit.addEventListener('pointermove', (event) => hover(container, nearest(snaps, fromEvent(event)), true));
    hit.addEventListener('pointerdown', (event) => hover(container, nearest(snaps, fromEvent(event)), true));
    hit.addEventListener('pointerleave', () => leave(container));
    svg.addEventListener('blur', () => leave(container));
    svg.addEventListener('keydown', (event) => {
      if (!snaps.length) return;
      let i = state.index < 0 ? snaps.length - 1 : state.index;
      if (event.key === 'ArrowLeft') i = Math.max(0, i - 1);
      else if (event.key === 'ArrowRight') i = Math.min(snaps.length - 1, i + 1);
      else if (event.key === 'Home') i = 0;
      else if (event.key === 'End') i = snaps.length - 1;
      else if (event.key === 'Escape') { leave(container); return; }
      else return;
      event.preventDefault();
      hover(container, snaps[i], true);
    });
    svg.addEventListener('focus', () => { if (snaps.length) hover(container, snaps[state.index < 0 ? snaps.length - 1 : state.index], true); });

    if (opts.group) {
      if (!groups.has(opts.group)) groups.set(opts.group, new Set());
      groups.get(opts.group).add(container);
    }
    if (container._hoverT !== undefined && container._hoverT !== null) hover(container, nearest(snaps, container._hoverT), container._hoverTip, true);
  }

  function nearest(snaps, t) {
    if (!snaps.length) return t;
    let best = snaps[0];
    for (const s of snaps) if (Math.abs(s - t) < Math.abs(best - t)) best = s;
    return best;
  }

  function hover(container, t, withTip, fromGroup) {
    const st = container._state;
    if (!st) return;
    container._hoverT = t;
    container._hoverTip = withTip;
    if (t < st.start || t > st.end) { hide(container); return; }
    st.index = st.snaps.indexOf(t);
    const cx = st.x(t);
    st.cross.setAttribute('visibility', 'visible');
    st.cross.firstChild.setAttribute('x1', cx);
    st.cross.firstChild.setAttribute('x2', cx);
    const rows = [];
    st.drawn.forEach((s, i) => {
      const v = valueAt(s, t);
      const dot = st.dots[i];
      if (v === null) { dot.setAttribute('visibility', 'hidden'); return; }
      dot.setAttribute('visibility', 'visible');
      dot.setAttribute('cx', cx);
      dot.setAttribute('cy', st.y(v));
      rows.push([s, v]);
    });
    if (withTip) showTip(st, t, cx, rows); else st.tip.hidden = true;
    if (!fromGroup && st.opts.group) {
      const members = groups.get(st.opts.group) || new Set();
      for (const other of Array.from(members)) {
        if (!other.isConnected) members.delete(other);
        else if (other !== container) hover(other, nearest(other._state ? other._state.snaps : [], t), false, true);
      }
    }
  }

  function showTip(st, t, cx, rows) {
    const tip = st.tip;
    tip.textContent = '';
    const head = html('div', 't', tip);
    head.textContent = clock(t, true) + (st.opts.now ? ' · ' + relative(t - st.opts.now) : '');
    for (const [s, v] of rows) {
      const row = html('div', 'row', tip);
      const name = html('span', '', row);
      const key = html('span', s.dash ? 'key dash' : 'key', name);
      if (s.dash) key.style.borderTopColor = 'var(' + s.color + ')';
      else key.style.background = 'var(' + s.color + ')';
      name.appendChild(document.createTextNode(s.name));
      html('b', '', row).textContent = (s.format || st.opts.format || String)(v);
    }
    if (!rows.length) html('div', 'row', tip).textContent = 'No value at this time';
    tip.hidden = false;
    const box = st.svg.getBoundingClientRect();
    const scale = box.width / st.width;
    const px = cx * scale;
    const w = tip.offsetWidth;
    let left = px + 14;
    if (left + w > box.width) left = Math.max(0, px - w - 14);
    tip.style.left = left + 'px';
    tip.style.top = st.top * scale + 'px';
  }

  function relative(ms) {
    const s = Math.round(Math.abs(ms) / 1000);
    if (s < 2) return 'now';
    const text = s < 90 ? s + ' s' : s < 5400 ? Math.round(s / 60) + ' min' : s < 129600 ? Math.round(s / 3600) + ' h' : Math.round(s / 86400) + ' days';
    return ms > 0 ? 'in ' + text : text + ' ago';
  }

  function hide(container) {
    const st = container._state;
    if (!st) return;
    st.cross.setAttribute('visibility', 'hidden');
    st.tip.hidden = true;
  }

  function leave(container) {
    container._hoverT = null;
    hide(container);
    const st = container._state;
    if (st && st.opts.group) {
      for (const other of groups.get(st.opts.group) || []) {
        if (other !== container) { other._hoverT = null; hide(other); }
      }
    }
  }

  const resized = new ResizeObserver((entries) => {
    for (const entry of entries) {
      const el = entry.target;
      if (el._chart && Math.abs(Math.round(entry.contentRect.width) - (el._renderedWidth || 0)) > 1) render(el, el._chart);
    }
  });

  function observe(container) {
    resized.observe(container);
  }

  // spark draws a small CPU sparkline: an area for use and a thin line for the
  // CPU the pods can use at their target.
  function spark(container, points, capacity, label) {
    container.textContent = '';
    const data = points.filter((p) => Number.isFinite(p[1]));
    if (data.length < 2) {
      html('span', 'spark-empty', container).textContent = 'No CPU history yet';
      return;
    }
    const width = 300;
    const height = 54;
    const start = data[0][0];
    const end = data[data.length - 1][0];
    let max = 0;
    for (const p of data) max = Math.max(max, p[1]);
    for (const p of capacity) if (Number.isFinite(p[1])) max = Math.max(max, p[1]);
    max = max > 0 ? max * 1.1 : 1;
    const x = (t) => 2 + ((t - start) / Math.max(1, end - start)) * (width - 8);
    const y = (v) => height - 3 - (v / max) * (height - 8);
    const svg = node('svg', { viewBox: '0 0 ' + width + ' ' + height, preserveAspectRatio: 'none', role: 'img', 'aria-label': label }, container);
    const s = { points: data, gap: spacing(data.map((p) => p[0])) };
    paint(node('path', { d: areaPath(s, x, y, start, end) }, svg), '--s-cpu', true, 0.16);
    paint(node('path', { d: linePath(s, x, y, start, end), fill: 'none', 'stroke-width': 1.6, 'vector-effect': 'non-scaling-stroke' }, svg), '--s-cpu');
    const cap = capacity.filter((p) => Number.isFinite(p[1]));
    if (cap.length) {
      const c = { points: cap, gap: spacing(cap.map((p) => p[0])), step: true };
      paint(node('path', { d: linePath(c, x, y, start, end), fill: 'none', 'stroke-width': 1.2, 'stroke-dasharray': '3 3', 'vector-effect': 'non-scaling-stroke' }, svg), '--s-cap');
    }
  }

  window.HPACharts = { render: render, spark: spark, clock: clock, relative: relative };
})();
