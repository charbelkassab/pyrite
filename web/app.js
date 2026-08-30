/* natural-quant front end.
 *
 * Everything on screen derives from a list of "views". A view is one thing you
 * want on the chart: a strategy, or a ticker/index. Views own their colour,
 * name and visibility, and the user can edit all three. A colour is claimed on
 * creation and released only on delete, so removing one view never repaints
 * the others.
 */
'use strict';

const LC = window.LightweightCharts;

/* ── helpers ─────────────────────────────────────────────── */

const $ = (id) => document.getElementById(id);
const el = (tag, cls, text) => {
  const n = document.createElement(tag);
  if (cls) n.className = cls;
  if (text !== undefined) n.textContent = text;
  return n;
};

async function api(path, opts) {
  const res = await fetch(path, opts);
  const text = await res.text();
  let body;
  try { body = text ? JSON.parse(text) : null; } catch { body = { error: text }; }
  if (!res.ok) throw new Error((body && body.error) || `request failed (${res.status})`);
  return body;
}

const fmtPct = (v, d = 2) =>
  v === null || v === undefined || !isFinite(v) ? '—' : `${(v * 100).toFixed(d)}%`;
const fmtMoney = (v) =>
  v === null || v === undefined || !isFinite(v)
    ? '—'
    : v.toLocaleString('en-US', { style: 'currency', currency: 'USD', maximumFractionDigits: 2 });
const fmtNum = (v, d = 2) =>
  v === null || v === undefined || !isFinite(v) ? '—' : v.toFixed(d);
const signClass = (v) => (v > 0 ? 'pos' : v < 0 ? 'neg' : '');
const signed = (v) => `${v >= 0 ? '+' : ''}${v.toFixed(2)}%`;

const isoToday = () => new Date().toISOString().slice(0, 10);
// EARLIEST stands for "as far back as the data goes". No symbol served here
// has daily history before it.
const EARLIEST = '1970-01-02';
function isoYearsAgo(n) {
  const d = new Date();
  d.setFullYear(d.getFullYear() - n);
  return d.toISOString().slice(0, 10);
}

/* ── palette & state ─────────────────────────────────────── */

const PALETTE = ['--series-1', '--series-2', '--series-3', '--series-4',
                 '--series-5', '--series-6', '--series-7', '--series-8'];
const cssVar = (n) => getComputedStyle(document.documentElement).getPropertyValue(n).trim();

const state = {
  views: [],
  usedSlots: new Set(),
  activeTab: 'overview',
  selectedDay: null,
  chart: null,
  ddChart: null,
  markersApi: new Map(),
  nextId: 1,
  needsFit: false,
  // baselineDate is the first bar of the visible window; percentage returns
  // are measured from there, not from the start of the loaded history.
  baselineDate: null,
  rebasing: false,
  composerKind: 'strategy',
  // period is the data window that is loaded and backtested, as opposed to
  // the zoom level, which only decides what part of it is on screen.
  period: { from: null, to: null },
  // bulk suppresses per-view redraws while a period change reloads them all.
  bulk: false,
};

function claimSlot() {
  for (let i = 0; i < PALETTE.length; i++) {
    if (!state.usedSlots.has(i)) { state.usedSlots.add(i); return i; }
  }
  return PALETTE.length - 1;
}

/* ── preferences ─────────────────────────────────────────── */

const PREF_KEY = 'nq.prefs.v1';
const TOGGLES = ['tglPercent', 'tglLog', 'tglMarkers', 'tglDrawdown', 'tglNoDown'];

function loadPrefs() {
  let p = {};
  try { p = JSON.parse(localStorage.getItem(PREF_KEY) || '{}'); } catch { p = {}; }
  for (const id of TOGGLES) {
    if (typeof p[id] === 'boolean') $(id).checked = p[id];
  }
}
function savePrefs() {
  const p = {};
  for (const id of TOGGLES) p[id] = $(id).checked;
  try { localStorage.setItem(PREF_KEY, JSON.stringify(p)); } catch { /* private mode */ }
}

/* ── chart ───────────────────────────────────────────────── */

function chartOptions(height) {
  return {
    height,
    layout: {
      background: { type: 'solid', color: cssVar('--surface-1') },
      textColor: cssVar('--text-muted'),
      fontFamily: getComputedStyle(document.body).fontFamily,
      attributionLogo: false,
    },
    grid: { vertLines: { color: cssVar('--border') }, horzLines: { color: cssVar('--border') } },
    rightPriceScale: { borderColor: cssVar('--border') },
    timeScale: {
      borderColor: cssVar('--border'),
      rightOffset: 4,
      // The default minimum bar spacing of 0.5px caps how much history can be
      // on screen: at ~1000px that is only ~2000 bars, so a 33-year daily
      // series silently showed just its last eight years even on "All".
      minBarSpacing: 0.02,
    },
    crosshair: {
      mode: LC.CrosshairMode.Normal,
      vertLine: { color: cssVar('--border-strong'), labelBackgroundColor: cssVar('--surface-3') },
      horzLine: { color: cssVar('--border-strong'), labelBackgroundColor: cssVar('--surface-3') },
    },
    localization: {
      priceFormatter: (v) => {
        switch (displayMode()) {
          case 'index': return v.toFixed(0);
          case 'percent': return `${v.toFixed(1)}%`;
          default: return fmtMoney(v);
        }
      },
    },
  };
}

function initCharts() {
  state.chart = LC.createChart($('chart'), chartOptions($('chart').clientHeight || 400));
  state.ddChart = LC.createChart($('ddChart'), {
    ...chartOptions(130),
    localization: { priceFormatter: (v) => `${v.toFixed(1)}%` },
  });

  let syncing = false;
  const sync = (from, to) => {
    if (syncing) return;
    syncing = true;
    const r = from.timeScale().getVisibleLogicalRange();
    if (r) to.timeScale().setVisibleLogicalRange(r);
    syncing = false;
  };
  state.chart.timeScale().subscribeVisibleLogicalRangeChange(() => {
    sync(state.chart, state.ddChart);
    scheduleRebase();
  });
  state.ddChart.timeScale().subscribeVisibleLogicalRangeChange(() => sync(state.ddChart, state.chart));

  state.chart.subscribeCrosshairMove(renderLegendFor);
  state.chart.subscribeClick((p) => { if (p && p.time) selectDay(p.time); });

  const ro = new ResizeObserver(() => {
    const w = $('chart').clientWidth;
    if (w <= 0) return;
    state.chart.applyOptions({ width: w, height: $('chart').clientHeight || 400 });
    if (state.needsFit) { state.needsFit = false; state.chart.timeScale().fitContent(); }
  });
  ro.observe($('chart'));

  // The drawdown chart is built while its container is hidden, so it starts
  // with no size and stays blank when revealed. Watching its own element —
  // rather than the main chart's — is what actually resizes it, because
  // showing the panel does not change the main chart's dimensions at all.
  const ddRo = new ResizeObserver(() => sizeDrawdownChart());
  ddRo.observe($('ddChart'));
}

// fitAll shows the whole loaded range.
//
// It fires twice on purpose. The chart is often laid out in the same tick that
// data arrives, and a fitContent() issued before the container has its final
// size silently does nothing, leaving the default ~200-bar window. Repeating
// on the next frame costs nothing and removes the race.
// sizeDrawdownChart matches the sub-chart to its container and re-syncs it to
// the main chart's time range.
function sizeDrawdownChart() {
  if (!state.ddChart) return;
  const wrap = $('ddChart');
  const w = wrap.clientWidth, h = wrap.clientHeight;
  if (w <= 0 || h <= 0) return;
  state.ddChart.applyOptions({ width: w, height: h });
  const r = state.chart && state.chart.timeScale().getVisibleLogicalRange();
  if (r) state.ddChart.timeScale().setVisibleLogicalRange(r);
}

function fitAll() {
  if (!state.chart) return;
  const doFit = () => {
    if (!state.chart) return;
    if ($('chart').clientWidth > 0) {
      state.chart.timeScale().fitContent();
      state.needsFit = false;
    } else {
      state.needsFit = true;
    }
  };
  doFit();
  requestAnimationFrame(() => {
    doFit();
    scheduleRebase();
  });
}

/* ── rebasing ────────────────────────────────────────────── */

// Percentage returns are measured from the first bar actually on screen.
// Without this, zooming to 1Y would still show returns accumulated since the
// start of the loaded history, which makes two series look far further apart
// than they were over the window you are looking at.
let rebaseTimer = null;
function scheduleRebase() {
  if (state.rebasing) return;
  clearTimeout(rebaseTimer);
  rebaseTimer = setTimeout(applyBaseline, 60);
}

function visibleFromDate() {
  if (!state.chart) return null;
  const r = state.chart.timeScale().getVisibleRange();
  return r && r.from ? String(r.from) : null;
}

function applyBaseline() {
  if (!$('tglPercent').checked) {
    if (state.baselineDate !== null) { state.baselineDate = null; redrawSeries(); }
    else renderBaselineNote();
    return;
  }
  const from = visibleFromDate();
  if (!from || from === state.baselineDate) { renderBaselineNote(); return; }
  state.baselineDate = from;
  redrawSeries({ keepRange: true });
}

// baseValueFor returns the value each view is measured against: its first
// point at or after the visible start.
function baseValueFor(view) {
  const c = view.curve;
  if (!c || !c.length) return null;
  if (!state.baselineDate) return c[0].value;
  for (let i = 0; i < c.length; i++) {
    if (c[i].date >= state.baselineDate) return c[i].value;
  }
  return c[c.length - 1].value;
}

// stripDownDays rebuilds a curve with every losing day flattened: each daily
// return is floored at zero and the series recompounded.
//
// It is pure fantasy and is labelled as such on the chart. It exists because
// seeing what a strategy would look like without its bad days makes the size
// of those bad days obvious, which is a more visceral lesson than a drawdown
// number. The metrics table deliberately keeps reporting the real strategy.
function stripDownDays(curve) {
  if (!curve || !curve.length) return curve;
  const out = [{ ...curve[0] }];
  let value = curve[0].value;
  for (let i = 1; i < curve.length; i++) {
    const prev = curve[i - 1].value;
    const r = prev > 0 ? curve[i].value / prev - 1 : 0;
    value *= 1 + Math.max(0, r);
    out.push({ ...curve[i], value, drawdown: 0 });
  }
  return out;
}

// toChartData shapes a curve for the three display modes.
//
// A log axis cannot render a percentage series that crosses zero, so enabling
// Log switches to indexed growth instead: every series starts at 100 and the
// axis reads 200 for a double. Without this, ticking Log while "% return" was
// on did nothing at all, which looked like a broken control.
function toChartData(view, asPercent) {
  let c = view.curve;
  if (!c || !c.length) return [];
  if ($('tglNoDown').checked) c = stripDownDays(c);
  const logMode = $('tglLog').checked;
  const base = baseValueFor(view) || 1;

  if (asPercent && logMode) {
    return c.map((p) => ({ time: p.date, value: (p.value / base) * 100 }));
  }
  if (asPercent) {
    return c.map((p) => ({ time: p.date, value: (p.value / base - 1) * 100 }));
  }
  return c.map((p) => ({ time: p.date, value: p.value }));
}

// displayMode names what the axis is currently showing.
function displayMode() {
  const pct = $('tglPercent').checked, log = $('tglLog').checked;
  if (pct && log) return 'index';
  if (pct) return 'percent';
  return 'value';
}

const toDrawdownData = (view) => {
  const c = $('tglNoDown').checked ? stripDownDays(view.curve || []) : (view.curve || []);
  return c.map((p) => ({ time: p.date, value: (p.drawdown || 0) * 100 }));
};

/* ── markers ─────────────────────────────────────────────── */

// A strategy that trades daily produces one marker per bar; a thousand
// overlapping arrows render as a solid band that hides the curve.
const MAX_MARKERS = 120;

function tradeMarkers(view) {
  if (!view.fills || !view.fills.length) return [];
  const byDay = new Map();
  for (const f of view.fills) {
    const key = `${f.date}|${f.side === 'buy' || f.side === 'cover' ? 'in' : 'out'}`;
    if (!byDay.has(key)) byDay.set(key, { ...f, count: 1, total: f.value });
    else { const e = byDay.get(key); e.count++; e.total += f.value; }
  }
  let days = [...byDay.values()];
  view.markersTotal = days.length;
  if (days.length > MAX_MARKERS) days = days.sort((a, b) => b.total - a.total).slice(0, MAX_MARKERS);
  view.markersShown = days.length;

  const labelled = days.length <= 40;
  return days.sort((a, b) => (a.date < b.date ? -1 : 1)).map((f) => {
    const entry = f.side === 'buy' || f.side === 'cover';
    return {
      time: f.date,
      position: entry ? 'belowBar' : 'aboveBar',
      color: entry ? cssVar('--good') : cssVar('--bad'),
      shape: entry ? 'arrowUp' : 'arrowDown',
      text: labelled ? (f.count > 1 ? `${f.side} ×${f.count}` : `${f.side} ${f.symbol}`) : '',
    };
  });
}

/* ── drawing ─────────────────────────────────────────────── */

function redrawSeries(opts = {}) {
  if (!state.chart) return;
  const asPercent = $('tglPercent').checked;
  const showMarkers = $('tglMarkers').checked;
  const showDD = $('tglDrawdown').checked;
  $('ddChartWrap').hidden = !showDD;
  if (showDD) requestAnimationFrame(sizeDrawdownChart);

  state.rebasing = true;
  const keep = opts.keepRange ? state.chart.timeScale().getVisibleLogicalRange() : null;

  for (const v of state.views) {
    const visible = v.visible !== false && v.curve && v.curve.length > 0;

    if (!visible) {
      if (v.series) { state.chart.removeSeries(v.series); v.series = null; state.markersApi.delete(v.id); }
      if (v.ddSeries) { state.ddChart.removeSeries(v.ddSeries); v.ddSeries = null; }
      continue;
    }
    if (!v.series) {
      v.series = state.chart.addSeries(LC.LineSeries, {
        color: v.color, lineWidth: 2, priceLineVisible: false,
        lastValueVisible: true, crosshairMarkerRadius: 4,
      });
    }
    v.series.applyOptions({ color: v.color });
    v.series.setData(toChartData(v, asPercent));

    if (v.kind === 'strategy') {
      const markers = showMarkers ? tradeMarkers(v) : [];
      const api = state.markersApi.get(v.id);
      if (api) api.setMarkers(markers);
      else state.markersApi.set(v.id, LC.createSeriesMarkers(v.series, markers));
    }

    if (showDD) {
      if (!v.ddSeries) {
        v.ddSeries = state.ddChart.addSeries(LC.LineSeries, {
          color: v.color, lineWidth: 1, priceLineVisible: false, lastValueVisible: false,
        });
      }
      v.ddSeries.applyOptions({ color: v.color });
      v.ddSeries.setData(toDrawdownData(v));
    } else if (v.ddSeries) {
      state.ddChart.removeSeries(v.ddSeries);
      v.ddSeries = null;
    }
  }

  state.chart.priceScale('right').applyOptions({
    mode: $('tglLog').checked ? LC.PriceScaleMode.Logarithmic : LC.PriceScaleMode.Normal,
  });

  if (keep) state.chart.timeScale().setVisibleLogicalRange(keep);
  state.rebasing = false;

  renderBaselineNote();
  renderMarkerNote(showMarkers);
  renderLegendFor(null);
  renderViews();
  renderMetrics();
  renderTrades();
  renderCodeSelect();
  renderNotes();
}

function renderBaselineNote() {
  const n = $('baselineNote');
  if (!n) return;
  const mode = displayMode();
  if (mode === 'value') { n.textContent = 'showing absolute portfolio value'; return; }
  const from = state.baselineDate
    ? `${state.baselineDate} (the start of the visible window)`
    : 'the start of the data';
  n.textContent = mode === 'index'
    ? `growth of 100 invested at ${from}, on a log axis`
    : `returns measured from ${from}`;

  if ($('tglNoDown').checked) {
    n.textContent = '';
    const warn = el('span', 'fun', 'FANTASY: every losing day removed. ');
    n.appendChild(warn);
    n.appendChild(document.createTextNode('Not a real result — the table below still reports the real strategy.'));
  }
}

function renderMarkerNote(show) {
  const n = $('markerNote');
  if (!n) return;
  if (!show) { n.textContent = ''; return; }
  let shown = 0, total = 0;
  for (const v of state.views) {
    if (v.kind !== 'strategy' || !v.markersTotal || v.visible === false) continue;
    shown += v.markersShown || 0;
    total += v.markersTotal;
  }
  n.textContent = total > shown ? `showing the ${shown} largest of ${total} trading days` : '';
}

/* ── legend ──────────────────────────────────────────────── */

function renderLegendFor(param) {
  const box = $('legend');
  box.textContent = '';
  const asPercent = $('tglPercent').checked;

  for (const v of state.views) {
    if (v.visible === false || !v.curve || !v.curve.length) continue;
    const row = el('div', 'legend-row');
    const sw = el('span', 'legend-swatch');
    sw.style.background = v.color;
    row.appendChild(sw);
    row.appendChild(el('span', 'legend-name', v.label));

    let value = null;
    if (param && param.seriesData && v.series) {
      const d = param.seriesData.get(v.series);
      if (d) value = d.value;
    } else {
      // Read the last point of the series actually plotted, so the legend can
      // never contradict the axis — notably in fantasy mode, where the drawn
      // curve is not the real one.
      const plotted = toChartData(v, asPercent);
      value = plotted.length ? plotted[plotted.length - 1].value : null;
    }
    const out = el('span', 'legend-val');
    const mode = displayMode();
    if (value === null || value === undefined) out.textContent = '—';
    else if (mode === 'index') {
      out.textContent = value.toFixed(1);
      out.classList.add(signClass(value - 100));
    } else if (mode === 'percent') {
      out.textContent = signed(value);
      out.classList.add(signClass(value));
    } else out.textContent = fmtMoney(value);
    row.appendChild(out);
    box.appendChild(row);
  }
}

/* ── views panel ─────────────────────────────────────────── */

function renderViews() {
  const list = $('viewsList');
  list.textContent = '';

  if (!state.views.length) {
    list.appendChild(el('p', 'views-empty',
      'No views yet. Add a strategy or a ticker to get started.'));
    return;
  }

  for (const v of state.views) {
    const item = el('div', 'view-item');
    if (v.visible === false) item.classList.add('dim');
    if (v.status === 'running') item.classList.add('busy');
    if (v.status === 'error') item.classList.add('failed');
    if (v.flash) item.classList.add('flash');

    const row = el('div', 'view-row');

    const sw = el('button', 'view-swatch');
    sw.style.background = v.color;
    sw.title = 'Change colour';
    sw.onclick = (e) => openSwatchPicker(e.currentTarget, v);
    row.appendChild(sw);

    const name = el('div', 'view-name', v.label);
    name.contentEditable = 'true';
    name.spellcheck = false;
    name.title = 'Click to rename';
    name.onblur = () => {
      const t = name.textContent.trim();
      v.label = t || v.label;
      name.textContent = v.label;
      renderLegendFor(null); renderMetrics(); renderCodeSelect(); renderNotes();
    };
    name.onkeydown = (e) => { if (e.key === 'Enter') { e.preventDefault(); name.blur(); } };
    row.appendChild(name);

    const actions = el('div', 'view-actions');

    const eye = el('button', 'icon-btn', v.visible === false ? '○' : '●');
    eye.title = v.visible === false ? 'Show on chart' : 'Hide from chart';
    eye.onclick = () => { v.visible = v.visible === false; redrawSeries({ keepRange: true }); };
    actions.appendChild(eye);

    if (v.kind === 'strategy' && v.prompt) {
      const edit = el('button', 'icon-btn', '✎');
      edit.title = 'Edit and re-run this strategy';
      edit.onclick = () => openComposer('strategy', v.prompt, v.id);
      actions.appendChild(edit);
    }

    const del = el('button', 'icon-btn danger', '×');
    del.title = 'Delete this view';
    del.onclick = () => {
      // A strategy took a model call and a backtest to produce, so losing one
      // by mistake is expensive. A ticker is one click to add back.
      if (v.kind === 'strategy' && !confirm(`Delete "${v.label}"?`)) return;
      removeView(v.id);
    };
    actions.appendChild(del);

    row.appendChild(actions);
    item.appendChild(row);

    const meta = el('div', 'view-meta');
    meta.appendChild(el('span', 'view-kind', v.kind === 'strategy' ? 'strategy' : (v.symbol || 'ticker')));
    if (v.metrics) {
      const base = baseValueFor(v);
      const last = v.curve[v.curve.length - 1];
      const r = base ? last.value / base - 1 : v.metrics.total_return;
      const val = el('span', `view-return ${signClass(r)}`, signed(r * 100));
      meta.appendChild(val);
    } else if (v.status === 'running') {
      meta.appendChild(el('span', '', v.stage || 'working…'));
    }
    item.appendChild(meta);

    if (v.status === 'running') {
      const bar = el('div', 'view-progress');
      const inner = el('div');
      inner.style.width = `${v.progress || 0}%`;
      bar.appendChild(inner);
      item.appendChild(bar);
    }
    if (v.status === 'error') item.appendChild(el('div', 'view-error', v.error || 'failed'));

    list.appendChild(item);
  }
}

/* ── colour picker ───────────────────────────────────────── */

function openSwatchPicker(anchor, view) {
  const pop = $('swatchPop');
  const grid = $('swatchGrid');
  grid.textContent = '';

  PALETTE.forEach((slotVar, i) => {
    const b = el('button');
    const color = cssVar(slotVar);
    b.style.background = color;
    b.title = `Colour ${i + 1}`;
    b.onclick = () => {
      // Release the old slot so it can be reused, and claim the new one.
      state.usedSlots.delete(view.colorSlot);
      view.colorSlot = i;
      state.usedSlots.add(i);
      view.color = color;
      pop.hidden = true;
      redrawSeries({ keepRange: true });
    };
    grid.appendChild(b);
  });

  const r = anchor.getBoundingClientRect();
  pop.hidden = false;
  pop.style.left = `${Math.max(8, r.left - 130)}px`;
  pop.style.top = `${r.bottom + window.scrollY + 6}px`;
}

document.addEventListener('click', (e) => {
  const pop = $('swatchPop');
  if (!pop || pop.hidden) return;
  if (!pop.contains(e.target) && !e.target.classList.contains('view-swatch')) pop.hidden = true;
});

/* ── view lifecycle ──────────────────────────────────────── */

function addView(props) {
  const slot = claimSlot();
  const v = {
    id: state.nextId++,
    colorSlot: slot,
    color: cssVar(PALETTE[slot]),
    visible: true,
    curve: [],
    ...props,
  };
  state.views.push(v);
  showWorkspace();
  renderViews();
  return v;
}

function removeView(id) {
  const i = state.views.findIndex((v) => v.id === id);
  if (i < 0) return;
  const v = state.views[i];
  if (v.eventSource) v.eventSource.close();
  if (v.series) state.chart.removeSeries(v.series);
  if (v.ddSeries) state.ddChart.removeSeries(v.ddSeries);
  state.markersApi.delete(v.id);
  state.usedSlots.delete(v.colorSlot);
  state.views.splice(i, 1);

  if (!state.views.length) {
    $('workspace').hidden = true;
    $('emptyState').hidden = false;
    renderViews();
    return;
  }
  redrawSeries({ keepRange: true });
}

function showWorkspace() {
  $('emptyState').hidden = true;
  $('workspace').hidden = false;
  if (!state.chart) initCharts();
}

/* ── running strategies ──────────────────────────────────── */

function runOptions() {
  // start is omitted unless a custom period is set. The server reads that as
  // "run over as much history as the data allows".
  return {
    start: state.period.from || undefined,
    end: state.period.to || undefined,
    initial_cash: Number($('optCash').value) || 100000,
    benchmarks: [],
    fill: $('optFill').value,
    allow_short: $('optShort').checked,
    slippage_bps: Number($('optSlippage').value) || 0,
    commission_pct: (Number($('optCommission').value) || 0) / 100,
  };
}

async function addStrategyView(prompt, replaceId) {
  if (replaceId) removeView(replaceId);

  const view = addView({
    kind: 'strategy',
    label: prompt.slice(0, 42) + (prompt.length > 42 ? '…' : ''),
    prompt,
    status: 'running',
    stage: 'compiling',
    progress: 0,
  });

  try {
    const { id } = await api('/api/runs', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ prompt, ...runOptions() }),
    });
    view.runId = id;
    history.replaceState(null, '', `?run=${encodeURIComponent(id)}`);
    await streamRun(id, view);

    const bm = $('optBenchmark').value.trim();
    if (bm && !state.views.some((v) => v.kind === 'symbol' && v.symbol === bm.toUpperCase())) {
      await addSymbolView(bm, { silent: true });
    }
  } catch (err) {
    view.status = 'error';
    view.error = err.message;
    renderViews();
  }
}

function streamRun(id, view) {
  return new Promise((resolve, reject) => {
    const es = new EventSource(`/api/runs/${id}/events`);
    view.eventSource = es;

    es.onmessage = (msg) => {
      let ev;
      try { ev = JSON.parse(msg.data); } catch { return; }
      const run = ev.run;
      if (!run) return;

      if (ev.type === 'progress' || ev.type === 'status') {
        view.progress = run.progress || 0;
        view.stage = run.stage || run.status;
        if (run.plan && run.plan.name && !view.renamed) {
          view.label = run.plan.name;
          view.plan = run.plan;
        }
        renderViews();
      }
      if (ev.type === 'done') {
        es.close(); view.eventSource = null;
        applyRunResult(view, run);
        resolve();
      } else if (ev.type === 'error') {
        es.close(); view.eventSource = null;
        reject(new Error(run.error || 'the run failed'));
      }
    };
    es.onerror = () => { es.close(); view.eventSource = null; reject(new Error('lost connection to the server')); };
  });
}

function applyRunResult(view, run) {
  const res = run.result;
  view.status = 'done';
  view.plan = run.plan;
  view.label = (run.plan && run.plan.name) || run.label || view.label;
  view.curve = res.curve || [];
  view.metrics = res.metrics;
  view.days = res.days || [];
  view.fills = res.fills || [];
  view.warnings = res.warnings || [];
  view.usedAI = (res.ai_call_count || 0) > 0;
  view.critique = res.critique || null;
  view.trades = res.trades || [];
  view.tradeStats = res.trade_stats || null;
  view.risk = res.risk || null;
  view.attribution = res.attribution || null;
  renderSearchStrategies();
  if (state.activeTab === 'trust') renderTrust();

  for (const b of res.benchmarks || []) {
    if (state.views.some((v) => v.kind === 'symbol' && v.symbol === b.symbol)) continue;
    const bv = addView({ kind: 'symbol', symbol: b.symbol, label: b.label || b.symbol });
    bv.curve = b.curve;
    bv.metrics = b.metrics;
  }

  if (!state.period.from && view.curve.length) {
    state.period = { from: view.curve[0].date, to: view.curve[view.curve.length - 1].date };
    syncPeriodInputs();
  }
  if (state.bulk) { renderViews(); return; }

  state.baselineDate = null;
  redrawSeries();
  fitAll();
}

/* ── adding a symbol ─────────────────────────────────────── */

// chartedWindow keeps a new comparison over exactly the period already shown,
// so its metrics are measured over the same window as everything beside it.
function chartedWindow() {
  let from = null, to = null;
  for (const v of state.views) {
    if (!v.curve || !v.curve.length) continue;
    const a = v.curve[0].date, b = v.curve[v.curve.length - 1].date;
    if (!from || a < from) from = a;
    if (!to || b > to) to = b;
  }
  return { from, to };
}

async function addSymbolView(sym, opts = {}) {
  const symbol = (sym || '').trim().toUpperCase();
  if (!symbol) return;

  const existing = state.views.find((v) => v.kind === 'symbol' && v.symbol === symbol);
  if (existing) {
    // Already charted. Flash it rather than silently doing nothing, which is
    // indistinguishable from a failure.
    existing.flash = true;
    renderViews();
    setTimeout(() => { existing.flash = false; renderViews(); }, 900);
    return;
  }

  // The view is created up front and marked as loading. That way a slow or
  // failing fetch is visible in the list, instead of the click appearing to do
  // nothing at all.
  const view = addView({
    kind: 'symbol', symbol, label: symbol,
    status: 'running', stage: 'loading…',
  });

  const w = chartedWindow();
  const params = new URLSearchParams({
    symbols: symbol,
    base: String(Number($('optCash').value) || 100000),
    from: w.from || state.period.from || EARLIEST,
    to: w.to || state.period.to || isoToday(),
  });

  try {
    const data = await api(`/api/series?${params}`);
    const s = (data.series || [])[0];
    if (!s || !s.curve.length) {
      const why = data.failed && data.failed[symbol];
      throw new Error(why ? shortReason(why) : 'no data for this period');
    }
    view.symbol = s.symbol;
    view.label = s.label || s.symbol;
    view.curve = s.curve;
    view.metrics = s.metrics;
    view.status = 'done';
    view.error = null;

    redrawSeries({ keepRange: state.views.length > 2 });
    if (state.views.length <= 2) fitAll();
  } catch (err) {
    view.status = 'error';
    view.error = err.message;
    renderViews();
    if (opts.silent) removeView(view.id);
  }
}

// shortReason trims a server error down to something that fits in the views
// list without losing the cause.
function shortReason(msg) {
  const m = String(msg);
  if (/not found|no usable bars|404/i.test(m)) return 'no such symbol';
  if (/http 4\d\d|http 5\d\d/i.test(m)) return 'the data provider refused the request';
  return m.length > 90 ? m.slice(0, 89) + '…' : m;
}

/* ── composer ────────────────────────────────────────────── */

function openComposer(kind, prefill, replaceId) {
  $('composer').hidden = false;
  $('composeHint').textContent = '';
  $('composeHint').className = 'hint';
  state.replaceId = replaceId || null;
  setComposerKind(kind || 'strategy');
  if (prefill !== undefined) $('promptInput').value = prefill;
  ($('composer').querySelector(kind === 'symbol' ? '#symbolInput' : '#promptInput')).focus();
}

function setComposerKind(kind) {
  state.composerKind = kind;
  for (const b of $('composerKind').querySelectorAll('button')) {
    b.classList.toggle('active', b.dataset.kind === kind);
  }
  $('composerStrategy').hidden = kind !== 'strategy';
  $('composerSymbol').hidden = kind !== 'symbol';
}

function closeComposer() {
  $('composer').hidden = true;
  state.replaceId = null;
  $('symbolSuggest').hidden = true;
}

async function submitComposer() {
  const hint = $('composeHint');
  hint.className = 'hint';
  hint.textContent = '';

  if (state.composerKind === 'symbol') {
    const raw = $('symbolInput').value.trim();
    if (!raw) { hint.className = 'hint error'; hint.textContent = 'Type a ticker.'; return; }
    $('symbolInput').value = '';
    closeComposer();
    for (const s of raw.split(',')) {
      try { await addSymbolView(s); } catch { /* reported inline */ }
    }
    return;
  }

  const prompt = $('promptInput').value.trim();
  if (!prompt) { hint.className = 'hint error'; hint.textContent = 'Describe a strategy.'; return; }
  const replaceId = state.replaceId;
  closeComposer();
  addStrategyView(prompt, replaceId);
}

/* ── metrics ─────────────────────────────────────────────── */

const METRIC_ROWS = [
  ['Total return', (m) => fmtPct(m.total_return), (m) => m.total_return],
  ['Annualised (CAGR)', (m) => fmtPct(m.cagr), (m) => m.cagr],
  ['Volatility', (m) => fmtPct(m.volatility), null],
  ['Sharpe ratio', (m) => fmtNum(m.sharpe), (m) => m.sharpe],
  ['Sortino ratio', (m) => fmtNum(m.sortino), (m) => m.sortino],
  ['Max drawdown', (m) => fmtPct(m.max_drawdown), (m) => m.max_drawdown],
  ['Calmar ratio', (m) => fmtNum(m.calmar), (m) => m.calmar],
  ['Best day', (m) => fmtPct(m.best_day), null],
  ['Worst day', (m) => fmtPct(m.worst_day), null],
  ['Final value', (m) => fmtMoney(m.end_value), null],
  ['Trades', (m) => (m.total_trades ? String(m.total_trades) : '—'), null],
  ['Trade win rate', (m) => (m.total_trades ? fmtPct(m.trade_win_rate) : '—'), null],
  ['Profit factor', (m) => (m.total_trades ? fmtNum(m.profit_factor) : '—'), null],
  ['Costs paid', (m) => (m.total_costs ? fmtMoney(m.total_costs) : '—'), null],
];

// windowReturn reports the return over the visible window, so the table agrees
// with what the chart is showing rather than with the whole loaded history.
function windowReturn(v) {
  if (!v.curve || !v.curve.length) return null;
  const base = baseValueFor(v);
  if (!base) return null;
  return v.curve[v.curve.length - 1].value / base - 1;
}

const TRADING_DAYS = 252;

// windowMetrics recomputes the risk and return statistics over exactly the
// visible window. Without this the table would mix a window-scoped total
// return with full-period volatility and Sharpe, which is precisely the kind
// of quiet inconsistency that makes a backtest untrustworthy.
function windowMetrics(v) {
  const full = v.metrics || {};
  if (!v.curve || v.curve.length < 2) return full;

  const from = state.baselineDate;
  const slice = from ? v.curve.filter((p) => p.date >= from) : v.curve;
  if (slice.length < 2) return full;
  // Nothing was trimmed, so the server's figures already describe this window.
  if (slice.length === v.curve.length) return full;

  const start = slice[0].value, end = slice[slice.length - 1].value;
  const rets = [];
  for (let i = 1; i < slice.length; i++) {
    const prev = slice[i - 1].value;
    rets.push(prev > 0 ? slice[i].value / prev - 1 : 0);
  }

  const mean = rets.reduce((a, b) => a + b, 0) / rets.length;
  const variance = rets.reduce((a, r) => a + (r - mean) ** 2, 0) / Math.max(1, rets.length - 1);
  const sd = Math.sqrt(variance);

  const downs = rets.filter((r) => r < 0);
  const dd = downs.length
    ? Math.sqrt(downs.reduce((a, r) => a + r * r, 0) / downs.length)
    : 0;

  let peak = start, maxDD = 0;
  for (const p of slice) {
    if (p.value > peak) peak = p.value;
    if (peak > 0) maxDD = Math.min(maxDD, p.value / peak - 1);
  }

  const days = (new Date(slice[slice.length - 1].date) - new Date(slice[0].date)) / 86400000;
  const years = days / 365.25;
  const cagr = years > 0 && start > 0 && end > 0 ? Math.pow(end / start, 1 / years) - 1 : null;

  // Trade statistics restricted to fills inside the window.
  const to = slice[slice.length - 1].date;
  const fills = (v.fills || []).filter((f) => f.date >= slice[0].date && f.date <= to);
  const realised = fills.filter((f) => f.realized_pnl);
  const wins = realised.filter((f) => f.realized_pnl > 0);
  const grossWin = wins.reduce((a, f) => a + f.realized_pnl, 0);
  const grossLoss = realised.filter((f) => f.realized_pnl < 0).reduce((a, f) => a - f.realized_pnl, 0);

  return {
    ...full,
    total_return: end / start - 1,
    cagr,
    volatility: sd * Math.sqrt(TRADING_DAYS),
    sharpe: sd > 0 ? (mean / sd) * Math.sqrt(TRADING_DAYS) : null,
    sortino: dd > 0 ? (mean / dd) * Math.sqrt(TRADING_DAYS) : null,
    max_drawdown: maxDD,
    calmar: maxDD < 0 && cagr !== null ? cagr / Math.abs(maxDD) : null,
    best_day: Math.max(...rets),
    worst_day: Math.min(...rets),
    end_value: end,
    total_trades: realised.length,
    trade_win_rate: realised.length ? wins.length / realised.length : 0,
    profit_factor: grossLoss > 0 ? grossWin / grossLoss : null,
    total_costs: fills.reduce((a, f) => a + (f.commission || 0) + (f.slippage || 0), 0),
  };
}

function renderMetrics() {
  const t = $('metricsTable');
  t.textContent = '';
  const shown = state.views.filter((v) => v.visible !== false && v.metrics);
  if (!shown.length) return;

  const head = t.insertRow();
  head.appendChild(el('th', '', 'Metric'));
  for (const v of shown) {
    const th = el('th');
    const cell = el('div', 'series-cell');
    const dot = el('span', 'series-dot');
    dot.style.background = v.color;
    cell.appendChild(dot);
    cell.appendChild(el('span', '', v.label));
    th.appendChild(cell);
    head.appendChild(th);
  }
  // Every figure below describes the visible window, matching the chart.
  const computed = shown.map(windowMetrics);

  for (const [label, fmt, colorFn] of METRIC_ROWS) {
    const row = t.insertRow();
    row.appendChild(el('td', '', label));
    shown.forEach((v, i) => {
      const m = computed[i];
      const td = el('td', 'num', fmt(m, v));
      if (colorFn) {
        const raw = colorFn(m, v);
        if (typeof raw === 'number' && isFinite(raw)) td.classList.add(signClass(raw));
      }
      row.appendChild(td);
    });
  }
}

/* ── trades ──────────────────────────────────────────────── */

function renderTrades() {
  const t = $('tradesTable');
  t.textContent = '';
  const strategies = state.views.filter((v) => v.kind === 'strategy' && v.fills && v.visible !== false);
  if (!strategies.length) {
    t.appendChild(el('caption', 'muted', 'Add a strategy to see its trades.'));
    return;
  }
  const head = t.insertRow();
  for (const h of ['Date', 'View', 'Side', 'Symbol', 'Shares', 'Price', 'Value', 'P&L', 'Why']) {
    head.appendChild(el('th', '', h));
  }
  const rows = [];
  for (const v of strategies) for (const f of v.fills) rows.push({ ...f, _v: v });
  rows.sort((a, b) => (a.date < b.date ? 1 : -1));

  for (const f of rows.slice(0, 600)) {
    const r = t.insertRow();
    const d = el('td', '', f.date);
    d.style.cursor = 'pointer';
    d.onclick = () => selectDay(f.date);
    r.appendChild(d);

    const se = el('td');
    const cell = el('div', 'series-cell');
    const dot = el('span', 'series-dot');
    dot.style.background = f._v.color;
    cell.appendChild(dot);
    cell.appendChild(el('span', '', f._v.label));
    se.appendChild(cell);
    r.appendChild(se);

    const sd = el('td');
    sd.appendChild(el('span', `pill pill-${f.side}`, f.side));
    r.appendChild(sd);
    r.appendChild(el('td', '', f.symbol));
    r.appendChild(el('td', 'num', fmtNum(f.shares, 4)));
    r.appendChild(el('td', 'num', fmtMoney(f.price)));
    r.appendChild(el('td', 'num', fmtMoney(f.value)));
    r.appendChild(el('td', `num ${signClass(f.realized_pnl)}`, f.realized_pnl ? fmtMoney(f.realized_pnl) : '—'));
    r.appendChild(el('td', 'muted', f.reason || ''));
  }
  if (rows.length > 600) {
    const r = t.insertRow();
    const td = el('td', 'muted', `showing the 600 most recent of ${rows.length} fills`);
    td.colSpan = 9;
    r.appendChild(td);
  }
}

/* ── day detail ──────────────────────────────────────────── */

function selectDay(day) {
  state.selectedDay = day;
  switchTab('day');
  renderDayDetail();
}

function renderDayDetail() {
  const box = $('dayDetail');
  box.textContent = '';
  const day = state.selectedDay;
  if (!day) {
    box.appendChild(el('p', 'muted', 'Click anywhere on the chart to inspect that day.'));
    return;
  }
  box.appendChild(el('h3', '', day));

  const strategies = state.views.filter((v) => v.kind === 'strategy' && v.days && v.visible !== false);
  if (!strategies.length) {
    box.appendChild(el('p', 'muted', 'No strategy is loaded for this day.'));
    return;
  }

  for (const v of strategies) {
    const rec = v.days.find((d) => d.date === day);
    const wrap = el('div', 'day-section');

    const title = el('h4');
    const dot = el('span');
    dot.style.cssText = `background:${v.color};display:inline-block;width:9px;height:3px;margin-right:8px;vertical-align:middle;border-radius:2px;`;
    title.appendChild(dot);
    title.appendChild(document.createTextNode(v.label));
    wrap.appendChild(title);

    if (!rec) {
      wrap.appendChild(el('p', 'muted', 'No data on this date.'));
      box.appendChild(wrap);
      continue;
    }

    const stats = el('div', 'day-stats');
    const stat = (k, val, cls) => {
      const s = el('div', 'day-stat');
      s.appendChild(el('span', 'k', k));
      s.appendChild(el('span', `v ${cls || ''}`, val));
      stats.appendChild(s);
    };
    stat('Equity', fmtMoney(rec.equity));
    stat('Day', fmtPct(rec.return), signClass(rec.return));
    stat('Cash', fmtMoney(rec.cash));
    stat('Exposure', fmtPct(rec.exposure, 0));
    stat('Drawdown', fmtPct(rec.drawdown), signClass(rec.drawdown));
    wrap.appendChild(stats);

    if (rec.fills && rec.fills.length) {
      const sec = el('div', 'day-section');
      sec.appendChild(el('h4', '', `Trades executed (${rec.fills.length})`));
      const t = el('table');
      const h = t.insertRow();
      for (const c of ['Side', 'Symbol', 'Shares', 'Price', 'Value', 'P&L', 'Why']) h.appendChild(el('th', '', c));
      for (const f of rec.fills) {
        const r = t.insertRow();
        const sd = el('td');
        sd.appendChild(el('span', `pill pill-${f.side}`, f.side));
        r.appendChild(sd);
        r.appendChild(el('td', '', f.symbol));
        r.appendChild(el('td', 'num', fmtNum(f.shares, 4)));
        r.appendChild(el('td', 'num', fmtMoney(f.price)));
        r.appendChild(el('td', 'num', fmtMoney(f.value)));
        r.appendChild(el('td', `num ${signClass(f.realized_pnl)}`, f.realized_pnl ? fmtMoney(f.realized_pnl) : '—'));
        r.appendChild(el('td', 'muted', f.reason || ''));
      }
      sec.appendChild(t);
      wrap.appendChild(sec);
    }

    if (rec.positions && rec.positions.length) {
      const sec = el('div', 'day-section');
      sec.appendChild(el('h4', '', `Holdings at the close (${rec.positions.length})`));
      const t = el('table');
      const h = t.insertRow();
      for (const c of ['Symbol', 'Shares', 'Entry', 'Price', 'Value', 'Weight', 'Return', 'Held']) h.appendChild(el('th', '', c));
      for (const p of rec.positions) {
        const r = t.insertRow();
        r.appendChild(el('td', '', p.symbol));
        r.appendChild(el('td', 'num', fmtNum(p.shares, 4)));
        r.appendChild(el('td', 'num', fmtMoney(p.avg_price)));
        r.appendChild(el('td', 'num', fmtMoney(p.price)));
        r.appendChild(el('td', 'num', fmtMoney(p.value)));
        r.appendChild(el('td', 'num', fmtPct(p.weight, 1)));
        r.appendChild(el('td', `num ${signClass(p.return_pct)}`, fmtPct(p.return_pct)));
        r.appendChild(el('td', 'num', `${p.days_held}d`));
      }
      sec.appendChild(t);
      wrap.appendChild(sec);
    }

    if (rec.ai_calls && rec.ai_calls.length) {
      const sec = el('div', 'day-section');
      sec.appendChild(el('h4', '', `Model and web calls (${rec.ai_calls.length})`));
      for (const c of rec.ai_calls) {
        const card = el('div', 'ai-call');
        const head = el('div', 'ai-head');
        head.appendChild(el('span', '', c.kind.toUpperCase()));
        if (c.model) head.appendChild(el('span', '', `${c.provider} · ${c.model}`));
        head.appendChild(el('span', '', c.cached ? 'cached' : `${c.millis} ms`));
        if (c.error) head.appendChild(el('span', 'neg', c.error));
        card.appendChild(head);
        card.appendChild(el('div', 'ai-prompt', c.prompt));
        if (c.response) card.appendChild(el('div', 'ai-response', c.response));
        sec.appendChild(card);
      }
      wrap.appendChild(sec);
    }

    if (rec.logs && rec.logs.length) {
      const sec = el('div', 'day-section');
      sec.appendChild(el('h4', '', 'Strategy log'));
      for (const l of rec.logs) sec.appendChild(el('div', 'logline', l));
      wrap.appendChild(sec);
    }
    if (rec.error) {
      const sec = el('div', 'day-section');
      sec.appendChild(el('h4', '', 'Error on this day'));
      sec.appendChild(el('div', 'logline neg', rec.error));
      wrap.appendChild(sec);
    }
    box.appendChild(wrap);
  }
}

/* ── code & notes ────────────────────────────────────────── */

function renderCodeSelect() {
  const sel = $('codeSelect');
  const prev = sel.value;
  sel.textContent = '';
  const strategies = state.views.filter((v) => v.kind === 'strategy' && v.plan);
  for (const v of strategies) {
    const o = el('option', '', v.label);
    o.value = v.id;
    sel.appendChild(o);
  }
  if (prev && strategies.some((v) => String(v.id) === prev)) sel.value = prev;
  showCode();
}

function showCode() {
  const v = state.views.find((x) => String(x.id) === $('codeSelect').value);
  $('codeView').value = v && v.plan ? v.plan.code : '';
}

function renderNotes() {
  const box = $('notesBody');
  box.textContent = '';
  const strategies = state.views.filter((v) => v.kind === 'strategy' && v.plan);
  if (!strategies.length) {
    box.appendChild(el('p', 'muted', 'Add a strategy to see how it was interpreted.'));
    return;
  }
  for (const v of strategies) {
    box.appendChild(el('h4', '', v.label));
    if (v.plan.description) box.appendChild(el('p', 'muted', v.plan.description));
    const addList = (title, items) => {
      if (!items || !items.length) return;
      box.appendChild(el('h4', '', title));
      const ul = el('ul');
      for (const i of items) ul.appendChild(el('li', '', i));
      box.appendChild(ul);
    };
    addList('Assumptions made', v.plan.assumptions);
    addList('Limitations', v.plan.limitations);
    addList('Warnings from the run', v.warnings);

    if (v.usedAI) {
      const c = el('div', 'callout');
      c.innerHTML =
        '<strong>This strategy consulted a model or the internet during the backtest.</strong> ' +
        'Those sources reflect the world as it is today, not as it was on each simulated day. ' +
        'That is lookahead bias, and it can flatter results substantially.';
      box.appendChild(c);
    }
  }
  const c = el('div', 'callout');
  c.innerHTML =
    '<strong>Backtests overstate what you would really have earned.</strong> ' +
    'Symbol lists contain the companies that matter today, so past picks are drawn from known survivors. ' +
    'Costs are modelled but taxes are not.';
  box.appendChild(c);
}

/* ── period and ranges ───────────────────────────────────── */

// Two distinct ideas, deliberately kept separate:
//   period  — what data is loaded and backtested (state.period)
//   zoom    — which part of the loaded data is on screen
// A preset only zooms when the loaded data already covers it. When it does
// not — asking for 2002 on a run that loaded 2020 onwards — the period is
// widened and everything reloads, because zooming to a window with no data
// would just show an empty chart.

function loadedSpan() {
  let from = null, to = null;
  for (const v of state.views) {
    if (!v.curve || !v.curve.length) continue;
    const a = v.curve[0].date, b = v.curve[v.curve.length - 1].date;
    if (!from || a < from) from = a;
    if (!to || b > to) to = b;
  }
  return { from, to };
}

function syncPeriodInputs() {
  const span = loadedSpan();
  $('periodFrom').value = state.period.from || span.from || '';
  $('periodTo').value = state.period.to || span.to || '';
}

function openPeriodDialog() {
  syncPeriodInputs();
  $('periodHint').className = 'hint';
  $('periodHint').textContent = state.period.from
    ? 'Currently showing a custom period.'
    : 'Currently showing all available history.';
  $('periodDialog').showModal();
}

// markCustomRange keeps the toolbar in step with whether a custom period is
// active, so the highlighted button never contradicts what is charted.
function markCustomRange(custom) {
  for (const o of $('rangeButtons').querySelectorAll('button')) o.classList.remove('active');
  const target = custom ? '[data-range="custom"]' : '[data-range="all"]';
  const b = $('rangeButtons').querySelector(target);
  if (b) b.classList.add('active');
}

function applyRange(range) {
  if (!state.views.length || !state.chart) return;
  const span = loadedSpan();
  if (!span.to) return;

  if (range === 'all') { fitAll(); scheduleRebase(); return; }

  const start = new Date(span.to);
  switch (range) {
    case '1m': start.setMonth(start.getMonth() - 1); break;
    case '3m': start.setMonth(start.getMonth() - 3); break;
    case '6m': start.setMonth(start.getMonth() - 6); break;
    case 'ytd': start.setMonth(0); start.setDate(1); break;
    case '1y': start.setFullYear(start.getFullYear() - 1); break;
    case '3y': start.setFullYear(start.getFullYear() - 3); break;
    case '5y': start.setFullYear(start.getFullYear() - 5); break;
  }
  const from = start.toISOString().slice(0, 10);

  // Not covered by what is loaded: widen the period instead of zooming into
  // a gap. A few days of slack avoids reloading over a weekend boundary.
  if (from < span.from && daysBetween(from, span.from) > 5) {
    setPeriod(from, state.period.to || span.to);
    return;
  }
  state.chart.timeScale().setVisibleRange({ from: from < span.from ? span.from : from, to: span.to });
  scheduleRebase();
}

function daysBetween(a, b) {
  return Math.abs((new Date(b) - new Date(a)) / 86400000);
}

// setPeriod reloads every view over a new window: symbols are refetched, and
// strategies are replayed from their already-compiled code, so changing the
// period costs no model call.
// setPeriod reloads every view over a new window. Passing null for both means
// "as much history as the data allows", which is the default.
async function setPeriod(from, to) {
  if (from && to && from >= to) {
    setPeriodStatus('the start date must be before the end date', 'warn');
    return;
  }
  state.period = { from: from || null, to: to || null };
  setPeriodStatus(from ? 'reloading…' : 'loading full history…', 'busy');

  const problems = [];
  state.bulk = true;
  await Promise.all(state.views.map(async (v) => {
    try {
      if (v.kind === 'symbol') await refetchSymbolView(v, from, to);
      else await replayStrategyView(v, from, to);
    } catch (err) {
      problems.push(`${v.label}: ${err.message}`);
      v.status = 'error';
      v.error = err.message;
    }
  }));
  state.bulk = false;

  state.baselineDate = null;
  redrawSeries();
  fitAll();
  syncPeriodInputs();

  if (problems.length) {
    setPeriodStatus(`${problems.length} view${problems.length > 1 ? 's' : ''} had no data for this period`, 'warn');
  } else {
    const span = loadedSpan();
    setPeriodStatus(state.period.from && span.from ? `${span.from} → ${span.to}` : '', '');
  }
}

function setPeriodStatus(text, cls) {
  const n = $('periodStatus');
  n.textContent = text;
  n.className = `period-status ${cls || ''}`;
}

async function refetchSymbolView(v, from, to) {
  const params = new URLSearchParams({
    symbols: v.symbol,
    base: String(Number($('optCash').value) || 100000),
    from: from || EARLIEST,
    to: to || isoToday(),
  });
  const data = await api(`/api/series?${params}`);
  const s = (data.series || [])[0];
  if (!s || !s.curve.length) {
    const why = data.failed && data.failed[v.symbol];
    throw new Error(why ? `no data (${why})` : 'no data for this period');
  }
  v.curve = s.curve;
  v.metrics = s.metrics;
  v.status = 'done';
  v.error = null;
}

// replayStrategyView re-runs a strategy over a new period using the code it
// was already compiled to. No model call, so changing the period is fast and
// free even for AI strategies.
async function replayStrategyView(v, from, to) {
  if (!v.plan || !v.plan.code) throw new Error('no compiled code to replay');

  v.status = 'running';
  v.stage = 'replaying';
  v.progress = 0;
  renderViews();

  const { id } = await api('/api/runs', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({
      code: v.plan.code,
      name: v.label,
      universe: v.plan.universe,
      warmup: v.plan.warmup,
      allow_short: v.plan.allow_short,
      ...runOptions(),
      start: from || undefined,
      end: to || undefined,
    }),
  });
  v.runId = id;

  const original = v.plan;
  await streamRun(id, v);
  // The replay reports only the mechanical plan; keep the original prose so
  // the Notes tab still explains how the prompt was interpreted.
  v.plan = { ...original, code: original.code };
}

/* ── history, examples, health ───────────────────────────── */

async function loadRun(id) {
  try {
    const run = await api(`/api/runs/${id}`);
    if (!run.result) throw new Error('that run has no stored result');
    const v = addView({ kind: 'strategy', label: run.label || 'run', prompt: run.prompt, runId: id, status: 'done' });
    applyRunResult(v, run);
  } catch (err) {
    const h = $('composeHint');
    h.className = 'hint error';
    h.textContent = err.message;
  }
}

async function loadExamples() {
  try {
    const list = await api('/api/examples');
    const render = (container, items, compact) => {
      container.textContent = '';
      for (const ex of items) {
        const b = el('button', 'example');
        b.appendChild(el('strong', '', ex.title));
        if (!compact) b.appendChild(document.createTextNode(ex.prompt.slice(0, 92) + (ex.prompt.length > 92 ? '…' : '')));
        b.onclick = () => {
          openComposer('strategy', ex.prompt);
          $('promptInput').focus();
        };
        container.appendChild(b);
      }
    };
    render($('emptyExamples'), list.slice(0, 6), false);
  } catch { /* examples are static */ }
}

async function loadHealth() {
  try {
    const h = await api('/api/health');
    const box = $('healthStatus');
    box.textContent = '';
    const add = (cls, text) => {
      const s = el('span');
      s.appendChild(el('span', `dot ${cls}`));
      s.appendChild(document.createTextNode(text));
      box.appendChild(s);
    };
    const enabled = (h.providers || []).filter((p) => p.enabled);
    if (enabled.length) add('dot-ok', `${enabled.map((p) => p.name).join(', ')} connected`);
    else add('dot-warn', 'no model key — set OPENAI_API_KEY, CEREBRAS_API_KEY or KIMI_API_KEY');
    add(h.offline_mode ? 'dot-warn' : 'dot-ok', h.offline_mode ? 'offline (synthetic data)' : `data: ${h.data_provider}`);
    if (h.cached_symbols) add('dot-off', `${h.cached_symbols} symbols cached`);
  } catch { /* informational */ }
}

/* ── tabs ────────────────────────────────────────────────── */

function switchTab(name) {
  state.activeTab = name;
  for (const t of document.querySelectorAll('.tab')) t.classList.toggle('active', t.dataset.tab === name);
  for (const p of document.querySelectorAll('.tabpanel')) p.hidden = p.id !== `tab-${name}`;
  if (name === 'day') renderDayDetail();
  if (name === 'search') { loadObjectives(); renderSearchStrategies(); renderSearch(); }
  if (name === 'trust') renderTrust();
}

/* ── symbol autocomplete ─────────────────────────────────── */

let suggestTimer = null;
function onSymbolInput() {
  clearTimeout(suggestTimer);
  const q = $('symbolInput').value.trim();
  if (q.length < 2) { $('symbolSuggest').hidden = true; return; }
  suggestTimer = setTimeout(async () => {
    try {
      const results = await api(`/api/symbols?q=${encodeURIComponent(q)}`);
      const box = $('symbolSuggest');
      box.textContent = '';
      if (!results.length) { box.hidden = true; return; }
      for (const r of results.slice(0, 8)) {
        const d = el('div');
        d.appendChild(el('span', 'sym', r.symbol));
        d.appendChild(document.createTextNode(r.name || ''));
        d.onclick = () => {
          $('symbolInput').value = '';
          box.hidden = true;
          closeComposer();
          addSymbolView(r.symbol).catch(() => {});
        };
        box.appendChild(d);
      }
      box.hidden = false;
    } catch { $('symbolSuggest').hidden = true; }
  }, 220);
}

/* ── wiring ──────────────────────────────────────────────── */

function init() {
  loadPrefs();

  $('btnAddView').onclick = () => {
    if ($('composer').hidden) openComposer(state.composerKind);
    else closeComposer();
  };
  $('btnCreateView').onclick = submitComposer;
  $('btnCancelCompose').onclick = closeComposer;

  for (const b of $('composerKind').querySelectorAll('button')) {
    b.onclick = () => setComposerKind(b.dataset.kind);
  }
  $('promptInput').addEventListener('keydown', (e) => {
    if ((e.metaKey || e.ctrlKey) && e.key === 'Enter') submitComposer();
  });
  $('symbolInput').addEventListener('input', onSymbolInput);
  $('symbolInput').addEventListener('keydown', (e) => {
    if (e.key === 'Enter') { e.preventDefault(); submitComposer(); }
  });
  for (const b of $('quickAdd').querySelectorAll('button')) {
    b.onclick = () => addSymbolView(b.dataset.sym);
  }

  for (const b of $('rangeButtons').querySelectorAll('button')) {
    b.onclick = () => {
      if (b.dataset.range === 'custom') { openPeriodDialog(); return; }
      for (const o of $('rangeButtons').querySelectorAll('button')) o.classList.remove('active');
      b.classList.add('active');
      applyRange(b.dataset.range);
    };
  }

  $('btnApplyPeriod').onclick = async () => {
    const from = $('periodFrom').value, to = $('periodTo').value;
    if (!from || !to || from >= to) {
      $('periodHint').className = 'hint error';
      $('periodHint').textContent = 'The start date must be before the end date.';
      return;
    }
    $('periodDialog').close();
    markCustomRange(true);
    await setPeriod(from, to);
  };
  $('btnMaxPeriod').onclick = async () => {
    $('periodDialog').close();
    markCustomRange(false);
    await setPeriod(null, null);
  };
  $('btnClosePeriod').onclick = () => $('periodDialog').close();

  for (const id of TOGGLES) {
    $(id).onchange = () => {
      savePrefs();
      if (id === 'tglPercent') state.baselineDate = null;
      // Log and % both change the shape of the plotted values, so the price
      // scale has to be re-derived rather than merely re-drawn.
      redrawSeries({ keepRange: true });
      state.chart.priceScale('right').applyOptions({ autoScale: true });
      if (id === 'tglPercent') scheduleRebase();
    };
  }

  for (const t of document.querySelectorAll('.tab')) t.onclick = () => switchTab(t.dataset.tab);

  $('btnRunSearch').onclick = () => { runSearch().catch(() => {}); };
  // Walk-forward and a plain sweep answer different questions, so the hint
  // changes with the switch rather than describing whichever one is off.
  $('searchWalkForward').onchange = () => {
    $('searchHint').textContent = $('searchWalkForward').checked
      ? 'Parameters are chosen on each training window and reported on the window that follows, '
        + 'which is the only arrangement whose numbers mean what a reader assumes they mean.'
      : 'A single backtest tells you how one configuration did. A search tells you whether the '
        + 'idea works or whether that one number fitted the sample.';
  };

  $('codeSelect').onchange = showCode;
  $('btnCopyCode').onclick = async () => {
    try {
      await navigator.clipboard.writeText($('codeView').value);
      $('btnCopyCode').textContent = 'Copied';
      setTimeout(() => ($('btnCopyCode').textContent = 'Copy'), 1400);
    } catch { /* clipboard may be blocked */ }
  };
  $('btnRerunCode').onclick = async () => {
    const code = $('codeView').value.trim();
    if (!code) return;
    const src = state.views.find((v) => String(v.id) === $('codeSelect').value);
    const view = addView({
      kind: 'strategy', label: (src ? src.label : 'Custom') + ' (edited)',
      status: 'running', stage: 'running', progress: 0,
    });
    try {
      const { id } = await api('/api/runs', {
        method: 'POST', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          code, name: view.label,
          universe: src && src.plan ? src.plan.universe : undefined,
          warmup: src && src.plan ? src.plan.warmup : undefined,
          ...runOptions(),
        }),
      });
      view.runId = id;
      await streamRun(id, view);
    } catch (err) {
      view.status = 'error';
      view.error = err.message;
      renderViews();
    }
  };

  $('btnApiDocs').onclick = async () => {
    try {
      const res = await fetch('/api/strategy-api');
      $('apiBody').textContent = await res.text();
      $('apiDialog').showModal();
    } catch { /* optional */ }
  };
  $('btnCloseApi').onclick = () => $('apiDialog').close();

  loadHealth();
  loadExamples();
  renderViews();

  // Deep links restore a whole workspace. Each part is independent: a link
  // may carry only symbols, only a run, or both.
  const params = new URLSearchParams(location.search);
  applyDeepLink(params).catch(() => {});
}

async function applyDeepLink(params) {
  const runId = params.get('run');
  if (runId) await loadRun(runId);

  const compare = params.get('compare');
  if (compare) {
    for (const s of compare.split(',')) await addSymbolView(s);
  }

  const from = params.get('from'), to = params.get('to');
  if (from && to) await setPeriod(from, to);

  const range = params.get('range');
  if (range) {
    const btn = $('rangeButtons').querySelector(`[data-range="${range}"]`);
    if (btn) btn.click();
  }
  // Views arrive one at a time, each fitting as it lands. Fit once more at
  // the end so the final view spans everything that was loaded, unless the
  // link asked for a specific window.
  if (!range && !(from && to)) fitAll();

  const day = params.get('day');
  if (day) selectDay(day);
  const tab = params.get('tab');
  if (tab) switchTab(tab);
}

document.addEventListener('DOMContentLoaded', init);

/* ── parameter search ────────────────────────────────────── */

// A search is many backtests over one strategy's declared parameters. The
// point is not to find the best cell: it is to find out whether the best cell
// means anything, which a single backtest structurally cannot say.

const searchState = { result: null, kind: null, running: false, objectives: [] };

async function loadObjectives() {
  if (searchState.objectives.length) return;
  try {
    const r = await api('/api/objectives');
    searchState.objectives = r.objectives || [];
  } catch {
    searchState.objectives = ['sharpe', 'cagr', 'calmar', 'total_return'];
  }
  const sel = $('searchObjective');
  sel.textContent = '';
  for (const o of searchState.objectives) {
    const opt = el('option', '', o.replace(/_/g, ' '));
    opt.value = o;
    if (o === 'sharpe') opt.selected = true;
    sel.appendChild(opt);
  }
}

function renderSearchStrategies() {
  const sel = $('searchStrategy');
  const prev = sel.value;
  sel.textContent = '';
  const strategies = state.views.filter((v) => v.kind === 'strategy' && v.plan);
  for (const v of strategies) {
    const opt = el('option', '', v.label);
    opt.value = v.id;
    sel.appendChild(opt);
  }
  if (prev && strategies.some((v) => String(v.id) === prev)) sel.value = prev;
  $('btnRunSearch').disabled = !strategies.length || searchState.running;
  if (!strategies.length) {
    $('searchBody').textContent = '';
    $('searchBody').appendChild(el('p', 'muted',
      'Run a strategy first, then search the space around it.'));
  }
}

async function runSearch() {
  const view = state.views.find((v) => String(v.id) === $('searchStrategy').value);
  if (!view || !view.plan) return;

  const walkForward = $('searchWalkForward').checked;
  searchState.running = true;
  searchState.result = null;
  searchState.kind = walkForward ? 'walkforward' : 'sweep';
  $('btnRunSearch').disabled = true;
  $('searchProgress').hidden = false;
  $('searchBar').style.width = '0%';
  $('searchBody').textContent = '';

  const body = {
    code: view.plan.code,
    name: view.label,
    universe: view.plan.universe || [],
    warmup: view.plan.warmup || 0,
    allow_short: !!view.plan.allow_short,
    objective: $('searchObjective').value,
    walk_forward: walkForward,
    ...runOptions(),
  };

  try {
    const { id } = await api('/api/sweeps', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body),
    });
    await streamSearch(id);
  } catch (e) {
    $('searchBody').textContent = '';
    $('searchBody').appendChild(el('p', 'error', shortReason(String(e && e.message || e))));
  } finally {
    searchState.running = false;
    $('searchProgress').hidden = true;
    $('btnRunSearch').disabled = false;
  }
}

// streamSearch follows the same SSE contract a backtest uses, so a long
// search reports progress rather than appearing to hang.
function streamSearch(id) {
  return new Promise((resolve) => {
    const es = new EventSource(`/api/runs/${id}/events`);
    const finish = () => { es.close(); resolve(); };

    es.onmessage = (ev) => {
      let msg;
      try { msg = JSON.parse(ev.data); } catch { return; }
      const run = msg.run;
      if (!run) return;

      if (msg.type === 'progress' || msg.type === 'status') {
        $('searchBar').style.width = `${run.progress || 0}%`;
        return;
      }
      if (msg.type === 'error') {
        $('searchBody').textContent = '';
        $('searchBody').appendChild(el('p', 'error', run.error || 'the search failed'));
        finish();
        return;
      }
      if (msg.type === 'done') {
        searchState.result = run.sweep || run.walk_forward || null;
        searchState.kind = run.walk_forward ? 'walkforward' : 'sweep';
        renderSearch();
        finish();
      }
    };
    es.onerror = finish;
  });
}

function renderSearch() {
  const body = $('searchBody');
  body.textContent = '';
  const res = searchState.result;
  if (!res) return;
  if (searchState.kind === 'walkforward') renderWalkForward(body, res);
  else renderSweep(body, res);
}

function renderSweep(body, res) {
  const head = el('p', 'muted',
    `${res.combos} combinations in ${(res.elapsed_ms / 1000).toFixed(1)}s, ranked by ${res.objective}`);
  body.appendChild(head);

  if ((res.axes || []).length >= 2) {
    body.appendChild(surfaceFor(res, res.axes[0], res.axes[1]));
  }
  body.appendChild(robustnessPanel(res.robustness, res.objective));

  const rows = [...res.rows].sort((a, b) => {
    const av = a.score === null, bv = b.score === null;
    if (av !== bv) return av ? 1 : -1;
    return b.score - a.score;
  });

  body.appendChild(el('div', 'surface-label', 'Every combination'));
  const wrap = el('div', 'table-scroll');
  const t = el('table', 'metrics');
  const h = t.insertRow();
  for (const c of ['Parameters', res.objective, 'Return', 'Drawdown', 'Trades', 'Win %']) {
    h.appendChild(el('th', '', c));
  }
  for (const r of rows.slice(0, 60)) {
    const tr = t.insertRow();
    tr.appendChild(el('td', '', r.label));
    if (r.error) {
      const td = el('td', 'muted', r.error);
      td.colSpan = 5;
      tr.appendChild(td);
      continue;
    }
    tr.appendChild(el('td', 'num', fmtNum(r.score, 3)));
    const ret = el('td', `num ${signClass(r.total_return)}`, fmtPct(r.total_return));
    tr.appendChild(ret);
    tr.appendChild(el('td', 'num neg', fmtPct(r.max_drawdown)));
    tr.appendChild(el('td', 'num', String(r.trades)));
    tr.appendChild(el('td', 'num', fmtPct(r.win_rate)));
  }
  wrap.appendChild(t);
  body.appendChild(wrap);
}

// surfaceFor builds the heatmap.
//
// This is the fastest overfitting detector in the tool. A broad warm region
// means the idea survives being specified slightly differently; one bright
// cell in a dark field means the number came from the search, not the market.
function surfaceFor(res, xAxis, yAxis) {
  const xs = res.grids[xAxis] || [];
  const ys = res.grids[yAxis] || [];
  const wrap = el('div', 'surface-wrap');
  if (!xs.length || !ys.length) return wrap;

  // Hold the other parameters at whatever the best row used: that is the
  // slice through the space the reported winner actually lives on.
  const scored = res.rows.filter((r) => !r.error && r.score !== null);
  if (!scored.length) return wrap;
  const best = scored.reduce((a, b) => (b.score > a.score ? b : a));

  const same = (a, b) => String(a) === String(b) || Number(a) === Number(b);
  const cellFor = (x, y) => scored.find((r) => {
    if (!same(r.params[xAxis], x) || !same(r.params[yAxis], y)) return false;
    return (res.axes || []).every((ax) =>
      ax === xAxis || ax === yAxis || same(r.params[ax], best.params[ax]));
  });

  let lo = Infinity, hi = -Infinity;
  for (const r of scored) { lo = Math.min(lo, r.score); hi = Math.max(hi, r.score); }
  if (!(hi > lo)) return wrap;

  wrap.appendChild(el('div', 'surface-label', `${xAxis} across, ${yAxis} down`));

  const grid = el('div', 'surface-grid');
  grid.style.gridTemplateColumns = `auto repeat(${xs.length}, minmax(34px, 1fr))`;
  grid.appendChild(el('div', 'surface-axis'));
  for (const x of xs) grid.appendChild(el('div', 'surface-axis', String(x)));

  for (const y of ys) {
    grid.appendChild(el('div', 'surface-axis y', String(y)));
    for (const x of xs) {
      const r = cellFor(x, y);
      if (!r) {
        grid.appendChild(el('div', 'surface-cell empty', '·'));
        continue;
      }
      const t = (r.score - lo) / (hi - lo);
      const cell = el('div', 'surface-cell', r.score.toFixed(2));
      cell.style.background = heatColor(t);
      // The ramp lightens as it rises, so the label has to darken with it or
      // the best cells — the ones anyone actually reads — become invisible.
      if (t > 0.55) cell.style.color = 'rgba(10, 16, 22, .88)';
      cell.title = `${r.label}\n${res.objective} ${fmtNum(r.score, 3)}\n` +
        `return ${fmtPct(r.total_return)}  drawdown ${fmtPct(r.max_drawdown)}  ${r.trades} trades`;
      if (r === best) cell.classList.add('best');
      grid.appendChild(cell);
    }
  }
  wrap.appendChild(grid);

  const scale = el('div', 'surface-scale');
  scale.appendChild(el('span', '', fmtNum(lo, 2)));
  const ramp = el('div', 'ramp');
  for (let i = 0; i < 24; i++) {
    const s = el('span');
    s.style.background = heatColor(i / 23);
    ramp.appendChild(s);
  }
  scale.appendChild(ramp);
  scale.appendChild(el('span', '', fmtNum(hi, 2)));
  wrap.appendChild(scale);
  return wrap;
}

// heatColor maps 0..1 onto a perceptually monotonic dark-to-warm ramp, which
// reads correctly in both themes and does not rely on hue alone.
function heatColor(t) {
  const c = Math.max(0, Math.min(1, t));
  const h = 250 - 210 * c;          // deep blue through to warm amber
  const l = 22 + 40 * c;
  const s = 45 + 30 * c;
  return `hsl(${h}, ${s}%, ${l}%)`;
}

function robustnessPanel(r, objective) {
  const box = el('div');
  if (!r) return box;
  box.appendChild(el('div', 'surface-label', 'How much of this is real?'));

  const wrap = el('div', 'table-scroll');
  const t = el('table', 'metrics');
  const row = (label, value, note) => {
    const tr = t.insertRow();
    tr.appendChild(el('td', '', label));
    tr.appendChild(el('td', 'num', value));
    tr.appendChild(el('td', 'note', note || ''));
  };
  row(`Best ${objective}`, fmtNum(r.best_score, 3), '');
  row(`Median ${objective}`, fmtNum(r.median_score, 3), '');
  if (r.expected_max_score) {
    row('Expected best from luck alone', fmtNum(r.expected_max_score, 3),
      'what the top of this many trials scores with no skill at all');
  }
  row('Combinations above zero', fmtPct(r.positive_share), '');
  if (r.plateau_ratio !== null && r.plateau_ratio !== undefined) {
    row('Neighbour support', fmtPct(r.plateau_ratio),
      'how well the cells next to the winner scored');
  }
  if (r.pbo !== null && r.pbo !== undefined) {
    row('Probability of overfitting', fmtPct(r.pbo),
      `${r.pbo_splits} train/test splits; 50% is a coin flip`);
  }
  if (r.deflated_sharpe !== null && r.deflated_sharpe !== undefined) {
    row('Deflated Sharpe', fmtPct(r.deflated_sharpe),
      'confidence the edge survives the number of trials');
  }
  wrap.appendChild(t);
  box.appendChild(wrap);

  if (r.verdict) {
    const v = el('div', 'verdict');
    v.appendChild(el('strong', '', 'Verdict'));
    v.appendChild(document.createTextNode(r.verdict));
    box.appendChild(v);
  }
  return box;
}

function renderWalkForward(body, res) {
  body.appendChild(el('p', 'muted',
    `${res.folds.length} folds, parameters chosen on each training window and reported on the test window that follows it`));

  const m = res.stitched_metrics || {};
  const sum = el('div', 'table-scroll');
  const st = el('table', 'metrics');
  const srow = (l, v, cls) => {
    const tr = st.insertRow();
    tr.appendChild(el('td', '', l));
    tr.appendChild(el('td', `num ${cls || ''}`, v));
  };
  srow('Out-of-sample total return', fmtPct(m.total_return), signClass(m.total_return));
  srow('Out-of-sample CAGR', fmtPct(m.cagr), signClass(m.cagr));
  srow('Out-of-sample Sharpe', fmtNum(m.sharpe, 2));
  srow('Out-of-sample max drawdown', fmtPct(m.max_drawdown), 'neg');
  srow('Mean in-sample return', fmtPct(res.in_sample_return));
  srow('Mean out-of-sample return', fmtPct(res.out_of_sample_return));
  if (res.efficiency !== null && res.efficiency !== undefined) {
    srow('Walk-forward efficiency', fmtPct(res.efficiency));
  }
  srow('Positive test windows', `${res.consistent_folds} / ${res.folds.length}`);
  srow('Parameter stability', fmtPct(res.param_stability));
  sum.appendChild(st);
  body.appendChild(el('div', 'surface-label',
    'Stitched out-of-sample equity — the only curve here that was never fitted to'));
  body.appendChild(sum);

  if (res.verdict) {
    const v = el('div', 'verdict');
    v.appendChild(el('strong', '', 'Verdict'));
    v.appendChild(document.createTextNode(res.verdict));
    body.appendChild(v);
  }

  body.appendChild(el('div', 'surface-label', 'Fold by fold'));
  const wrap = el('div', 'table-scroll');
  const t = el('table', 'metrics');
  const h = t.insertRow();
  for (const c of ['Test window', 'Chosen', 'In-sample', 'Out-of-sample']) h.appendChild(el('th', '', c));
  for (const f of res.folds) {
    const tr = t.insertRow();
    tr.appendChild(el('td', '', `${f.test_start} → ${f.test_end}`));
    if (f.error) {
      const td = el('td', 'muted', f.error);
      td.colSpan = 3;
      tr.appendChild(td);
      continue;
    }
    tr.appendChild(el('td', '', paramLabel(f.best_params)));
    tr.appendChild(el('td', `num ${signClass(f.train_metrics.total_return)}`, fmtPct(f.train_metrics.total_return)));
    tr.appendChild(el('td', `num ${signClass(f.test_metrics.total_return)}`, fmtPct(f.test_metrics.total_return)));
  }
  wrap.appendChild(t);
  body.appendChild(wrap);
}

function paramLabel(params) {
  if (!params) return '';
  return Object.keys(params).sort().map((k) => `${k}=${params[k]}`).join(' ');
}

/* ── critique ────────────────────────────────────────────── */

// Every result carries an assessment of itself. It is shown by default rather
// than tucked behind a control: a backtesting tool that oversells itself is
// worse than useless, and this is that principle made visible.
function renderTrust() {
  const body = $('trustBody');
  body.textContent = '';
  const strategies = state.views.filter(
    (v) => v.kind === 'strategy' && v.critique && v.visible !== false);

  if (!strategies.length) {
    body.appendChild(el('p', 'muted', 'Run a strategy to see what is wrong with the result.'));
    return;
  }

  for (const v of strategies) {
    const c = v.critique;
    const head = el('div', 'trust-score');
    const dot = el('span', 'series-dot');
    dot.style.background = v.color;
    head.appendChild(dot);
    head.appendChild(el('b', '', `${c.trust_score}`));
    head.appendChild(el('span', 'muted', `/ 100 — ${v.label}`));
    body.appendChild(head);

    if (!c.findings || !c.findings.length) {
      body.appendChild(el('p', 'muted', c.headline || 'nothing flagged'));
      continue;
    }
    for (const f of c.findings) {
      const d = el('div', `finding ${f.severity}`);
      d.appendChild(el('div', 'sev', f.severity));
      d.appendChild(el('h4', '', f.title));
      d.appendChild(el('p', '', f.detail));
      body.appendChild(d);
    }
  }
}
