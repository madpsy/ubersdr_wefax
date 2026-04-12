/* ubersdr_wefax — multi-channel HF weather-fax decoder UI
 *
 * Architecture:
 *  - SSE /api/live  → fax_line, new_image, channel_start, image_deleted events
 *  - GET /api/channels → list of configured channels + status
 *  - GET /api/images?label=&limit=&offset= → paginated gallery
 *  - GET /api/audio/preview?label= → streaming WAV for audio preview
 *  - DELETE /api/images/{id} → delete image
 */

'use strict';

// Base path injected by the server (empty string when accessed directly,
// e.g. "/addon/wefax" when behind the ka9q_ubersdr addon proxy).
const BASE_PATH = (typeof window.BASE_PATH === 'string') ? window.BASE_PATH : '';

// ---------------------------------------------------------------------------
// State
// ---------------------------------------------------------------------------

let channels = [];          // [{label, freq_hz, audio_mode, status, decoding, …}]
let activeLabel = '';       // currently selected channel filter ('' = all)

// Gallery
let galleryRecords = [];    // [{id, label, freq_hz, filename, thumb_file, lines, width, saved_at, …}]
let galleryOffset = 0;
const GALLERY_PAGE = 50;
let galleryExhausted = false;

// Live canvas
let liveCanvas = null;
let liveCtx = null;
let liveWidth = 0;
let liveLineCount = 0;
let liveLabel = '';

// The channel label currently being drawn on the live canvas (null = none locked).
// When "all" is selected, the first channel_start locks this; subsequent starts
// from other channels are ignored until the current one finishes.
let liveDrawingLabel = null;

// Detail view
let selectedID = null;

// Audio preview
let audioCtx = null;
let audioSource = null;
let audioReader = null;
let audioPlaying = false;
let audioMuted = true;   // start muted; user clicks the mute button to hear audio

// Audio panel — spectrum / waterfall
const audioPanel = {
  maxDb:   -25,  // top of dB scale  (must match #ctrl-maxdb value attr)
  range:    60,  // dB span          (must match #ctrl-range value attr)
  // WEFAX marker frequencies (Hz) — IOC576 carrier + sidebands
  markers: [1500, 1900, 2300],
  fftLow:   200,
  fftHigh: 2900,
};
let waterfallImg = null;   // ImageData
let waterfallCtx = null;   // CanvasRenderingContext2D for waterfall-canvas
let fftES = null;          // EventSource for /api/fft
let fftLabel = '';

// SNR SSE
let snrES = null;
let snrLabel = '';

// SSE
let sseSource = null;

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

function fmtFreq(hz) {
  if (hz >= 1e6) return (hz / 1e6).toFixed(3) + ' MHz';
  if (hz >= 1e3) return (hz / 1e3).toFixed(1) + ' kHz';
  return hz + ' Hz';
}

function fmtTime(iso) {
  if (!iso) return '—';
  const d = new Date(iso);
  return d.toLocaleString();
}

function fmtDuration(startIso, endIso) {
  if (!startIso || !endIso) return '—';
  const s = Math.round((new Date(endIso) - new Date(startIso)) / 1000);
  if (s < 60) return s + 's';
  return Math.floor(s / 60) + 'm ' + (s % 60) + 's';
}

// ---------------------------------------------------------------------------
// Channel selector
// ---------------------------------------------------------------------------

async function loadChannels() {
  try {
    const resp = await fetch(BASE_PATH + '/api/channels');
    if (!resp.ok) return;
    channels = await resp.json();
    if (!Array.isArray(channels)) channels = [];
    // Always display channels in ascending frequency order.
    channels.sort((a, b) => a.freq_hz - b.freq_hz);
  } catch (e) {
    console.warn('loadChannels:', e);
    return;
  }
  renderChannelSelector();
  renderStatusBadges();
  renderAudioChannelSelector();
}

function renderChannelSelector() {
  const sel = document.getElementById('channel-select');
  const prev = sel.value;
  // Keep the "all" option, rebuild the rest.
  while (sel.options.length > 1) sel.remove(1);
  for (const ch of channels) {
    const opt = document.createElement('option');
    opt.value = ch.label;
    opt.textContent = `${fmtFreq(ch.freq_hz)} ${ch.audio_mode.toUpperCase()} [${ch.status}]`;
    sel.appendChild(opt);
  }
  if (prev) sel.value = prev;
}

function renderAudioChannelSelector() {
  const sel = document.getElementById('audio-channel-select');
  const prev = sel.value;
  while (sel.options.length > 1) sel.remove(1);
  for (const ch of channels) {
    const opt = document.createElement('option');
    opt.value = ch.label;
    opt.textContent = `${fmtFreq(ch.freq_hz)} ${ch.audio_mode.toUpperCase()}`;
    sel.appendChild(opt);
  }
  // Restore the user's previous audio channel selection (independent of the
  // main channel filter — do NOT mirror activeLabel here).
  if (prev) sel.value = prev;
}

function renderStatusBadges() {
  const el = document.getElementById('status-badges');
  el.innerHTML = '';
  for (const ch of channels) {
    const badge = document.createElement('span');
    // Use 'receiving' class when actively decoding an image, otherwise use
    // the connection status ('running', 'reconnecting', 'stopped').
    const stateClass = ch.decoding ? 'receiving' : (ch.status || 'unknown');
    badge.className = 'status-badge status-' + stateClass;
    badge.dataset.label = ch.label;
    badge.textContent = `${fmtFreq(ch.freq_hz)} ${ch.decoding ? 'receiving' : ch.status}`;
    badge.title = ch.label;
    // Click badge to switch channel filter (dispatches 'change' which also
    // calls syncAudioToChannel via the channel-select change handler).
    // Also close any open detail view so the live panel is shown.
    badge.style.cursor = 'pointer';
    badge.addEventListener('click', () => {
      closeDetail();
      const sel = document.getElementById('channel-select');
      sel.value = ch.label;
      sel.dispatchEvent(new Event('change'));
    });
    el.appendChild(badge);
  }
}

// Update a single badge's state without a full re-render.
function updateBadgeState(label, decoding, status) {
  const badge = document.querySelector(`.status-badge[data-label="${label}"]`);
  if (!badge) return;
  const stateClass = decoding ? 'receiving' : (status || 'unknown');
  badge.className = 'status-badge status-' + stateClass;
  badge.textContent = fmtFreq(channels.find(c => c.label === label)?.freq_hz || 0) +
    ' ' + (decoding ? 'receiving' : (status || ''));
}

document.getElementById('channel-select').addEventListener('change', function () {
  activeLabel = this.value;
  resetLiveCanvas();

  if (activeLabel) {
    // If this channel is currently receiving, lock the live canvas onto it
    // immediately so incoming lines start drawing without waiting for the
    // next channel_start event.
    const ch = channels.find(c => c.label === activeLabel);
    if (ch && ch.decoding) {
      liveDrawingLabel = activeLabel;
      document.getElementById('live-label').textContent =
        `Live: ${fmtFreq(ch.freq_hz)} — ${ch.label}`;
      // Replay buffered rows so the user sees the image so far.
      replayLiveCanvas(activeLabel);
    } else {
      liveDrawingLabel = null;
      document.getElementById('live-label').textContent = 'Waiting for signal…';
    }
  } else {
    // "all channels" — clear lock; next channel_start will take over.
    liveDrawingLabel = null;
    document.getElementById('live-label').textContent = 'Waiting for signal…';
  }

  // Sync the audio preview dropdown to follow the main channel selector.
  // (syncAudioToChannel only updates sel.value and restarts audio if playing;
  // it does NOT cause the periodic renderAudioChannelSelector() to reset it.)
  syncAudioToChannel(activeLabel);
  // Connect SNR stream for the selected channel (or disconnect if "all").
  connectSNR(activeLabel);
  resetGallery();
  // Signal loadMoreImages() to auto-open the most recent image after the
  // first page loads, provided the channel is not currently decoding.
  autoSelectAfterLoad = true;
  loadMoreImages();
  reconnectSSE();
});

// Fetch buffered rows from the server for a mid-receive channel and draw them
// onto the live canvas.  Called when the user switches to a channel that is
// already decoding so they see the image so far rather than a blank canvas.
async function replayLiveCanvas(label) {
  try {
    const resp = await fetch(BASE_PATH + '/api/live/replay?label=' + encodeURIComponent(label));
    if (!resp.ok) return;
    const body = await resp.json();
    if (!body.rows || body.rows.length === 0) return;
    // Only replay if we are still watching this channel.
    if (liveDrawingLabel !== label) return;
    for (const b64 of body.rows) {
      // Stop replaying if the user switched away mid-replay.
      if (liveDrawingLabel !== label) break;
      const bin = atob(b64);
      const pixels = new Uint8Array(bin.length);
      for (let i = 0; i < bin.length; i++) pixels[i] = bin.charCodeAt(i);
      appendLiveLine(pixels);
    }
  } catch (e) {
    console.warn('[replay]', e);
  }
}

// ---------------------------------------------------------------------------
// Gallery
// ---------------------------------------------------------------------------

// Set to true by the channel-select change handler so that the first
// loadMoreImages() call after a channel switch auto-selects the most recent
// image when the channel is not currently decoding.
let autoSelectAfterLoad = false;

function resetGallery() {
  galleryRecords = [];
  galleryOffset = 0;
  galleryExhausted = false;
  // Remove all day-groups but keep the sentinel in place.
  const grid = document.getElementById('gallery-grid');
  grid.querySelectorAll('.day-group').forEach(g => g.remove());
  document.getElementById('gallery-count').textContent = '';
}

async function loadMoreImages() {
  if (galleryExhausted) return;
  const params = new URLSearchParams({ limit: GALLERY_PAGE, offset: galleryOffset });
  if (activeLabel) params.set('label', activeLabel);
  const isFirstLoad = autoSelectAfterLoad;
  autoSelectAfterLoad = false; // consume the flag
  try {
    const resp = await fetch(BASE_PATH + '/api/images?' + params);
    if (!resp.ok) return;
    const data = await resp.json();
    const imgs = data.images || [];
    if (imgs.length < GALLERY_PAGE) galleryExhausted = true;
    for (const rec of imgs) {
      galleryRecords.push(rec);
      appendCardToGroup(rec);
    }
    galleryOffset += imgs.length;
    updateGalleryCount();

    // After the first page loads following a channel switch, auto-open the
    // most recent image in the detail view if the channel is not currently
    // decoding (i.e. there is no live canvas to show).
    if (isFirstLoad && galleryRecords.length > 0) {
      const ch = channels.find(c => c.label === activeLabel);
      const isDecoding = ch && ch.decoding;
      if (!isDecoding) {
        selectRecord(galleryRecords[0].id);
      }
    }
  } catch (e) {
    console.warn('loadMoreImages:', e);
  }
}

function updateGalleryCount() {
  const el = document.getElementById('gallery-count');
  el.textContent = galleryRecords.length + (galleryExhausted ? '' : '+') + ' images';
}

// ---------------------------------------------------------------------------
// Day-group helpers
// ---------------------------------------------------------------------------

function recDateKey(rec) {
  // Use saved_at (ISO string) — slice to 'YYYY-MM-DD'.
  if (!rec.saved_at) return 'Unknown';
  return rec.saved_at.slice(0, 10);
}

function fmtDateLabel(key) {
  if (key === 'Unknown') return 'Unknown date';
  const d = new Date(key + 'T00:00:00Z');
  return d.toLocaleDateString(undefined, {
    weekday: 'long', year: 'numeric', month: 'long', day: 'numeric', timeZone: 'UTC',
  });
}

// Returns the existing .day-group for dateKey, or creates one before the sentinel.
function getOrCreateDayGroup(dateKey) {
  const grid = document.getElementById('gallery-grid');
  const existing = grid.querySelector(`.day-group[data-date="${dateKey}"]`);
  if (existing) return existing;
  const group = document.createElement('div');
  group.className = 'day-group';
  group.dataset.date = dateKey;
  group.innerHTML =
    `<div class="day-group-label">${fmtDateLabel(dateKey)}<span class="day-group-count"></span></div>` +
    `<div class="day-group-grid"></div>`;
  const sentinel = document.getElementById('gallery-sentinel');
  grid.insertBefore(group, sentinel);
  return group;
}

function updateGroupCount(group) {
  const badge = group.querySelector('.day-group-count');
  if (!badge) return;
  const n = group.querySelectorAll('.thumb-card').length;
  badge.textContent = n > 0 ? String(n) : '';
}

// Append a card to the correct day-group (used when loading older pages).
function appendCardToGroup(rec) {
  const dateKey = recDateKey(rec);
  const group   = getOrCreateDayGroup(dateKey);
  const inner   = group.querySelector('.day-group-grid');
  inner.appendChild(buildThumbCard(rec));
  updateGroupCount(group);
}

// Prepend a card to the top of its day-group (used for newly received images).
function prependThumbCard(rec) {
  const dateKey = recDateKey(rec);
  const grid    = document.getElementById('gallery-grid');
  let group     = grid.querySelector(`.day-group[data-date="${dateKey}"]`);
  if (!group) {
    // New day — insert a fresh group at the very top (before any existing groups).
    group = document.createElement('div');
    group.className = 'day-group';
    group.dataset.date = dateKey;
    group.innerHTML =
      `<div class="day-group-label">${fmtDateLabel(dateKey)}<span class="day-group-count"></span></div>` +
      `<div class="day-group-grid"></div>`;
    grid.insertBefore(group, grid.firstChild);
  }
  const inner = group.querySelector('.day-group-grid');
  inner.insertBefore(buildThumbCard(rec), inner.firstChild);
  updateGroupCount(group);
}

function buildThumbCard(rec) {
  const card = document.createElement('div');
  card.className = 'thumb-card';
  card.dataset.id = rec.id;

  const imgSrc = rec.thumb_file
    ? BASE_PATH + '/images/' + rec.thumb_file
    : BASE_PATH + '/images/' + rec.filename;

  card.innerHTML = `
    <div class="thumb-img-wrap">
      <img class="thumb-img" src="${imgSrc}" alt="fax" loading="lazy" />
    </div>
    <div class="thumb-meta">
      <div class="thumb-freq">${fmtFreq(rec.freq_hz)}</div>
      <div class="thumb-time">${fmtTime(rec.saved_at)}</div>
      <div class="thumb-size">${rec.width}×${rec.lines}px</div>
    </div>`;

  card.addEventListener('click', () => selectRecord(rec.id));
  return card;
}

// ---------------------------------------------------------------------------
// Infinite scroll — load more images when the sentinel enters the viewport.
// ---------------------------------------------------------------------------

(function initInfiniteScroll() {
  const sentinel = document.getElementById('gallery-sentinel');
  if (!sentinel || typeof IntersectionObserver === 'undefined') return;
  const observer = new IntersectionObserver(entries => {
    if (entries[0].isIntersecting && !galleryExhausted) {
      loadMoreImages();
    }
  }, { rootMargin: '200px' });
  observer.observe(sentinel);
})();

// ---------------------------------------------------------------------------
// Detail view
// ---------------------------------------------------------------------------

// Detail-view rotation state
let detailRotation = 0;

function applyDetailRotation() {
  const img = document.getElementById('detail-img');
  img.style.transform = detailRotation ? `rotate(${detailRotation}deg)` : '';
  const transposed = detailRotation === 90 || detailRotation === 270;
  img.style.maxWidth  = transposed ? '100%' : '';
  img.style.maxHeight = transposed ? '100%' : '';
}

function selectRecord(id) {
  selectedID = id;
  const rec = galleryRecords.find(r => r.id === id);
  if (!rec) return;

  // Reset rotation for each new image.
  detailRotation = 0;
  applyDetailRotation();

  // Highlight selected card.
  document.querySelectorAll('.thumb-card').forEach(c => c.classList.remove('selected'));
  const card = document.querySelector(`.thumb-card[data-id="${id}"]`);
  if (card) card.classList.add('selected');

  // Show detail panel.
  document.getElementById('live-panel').classList.add('hidden');
  const dv = document.getElementById('detail-view');
  dv.classList.remove('hidden');

  document.getElementById('detail-img').src = BASE_PATH + '/images/' + rec.filename;

  // Update download link to point at the full-resolution PNG.
  const dlBtn = document.getElementById('btn-download');
  dlBtn.href = BASE_PATH + '/images/' + rec.filename;
  dlBtn.download = rec.filename;

  const table = document.getElementById('detail-meta-table');
  const snr = rec.snr || {};
  const snrRow = snr.count > 0
    ? `<tr><th>SNR avg</th><td>${snr.avg_db != null ? snr.avg_db.toFixed(1) + ' dB' : '—'}</td></tr>
       <tr><th>SNR min/max</th><td>${snr.min_db != null ? snr.min_db.toFixed(1) : '—'} / ${snr.max_db != null ? snr.max_db.toFixed(1) : '—'} dB</td></tr>
       <tr><th>Baseband</th><td>${snr.baseband_avg_dbfs != null ? snr.baseband_avg_dbfs.toFixed(1) + ' dBFS' : '—'}</td></tr>
       <tr><th>Noise floor</th><td>${snr.noise_avg_dbfs != null ? snr.noise_avg_dbfs.toFixed(1) + ' dBFS' : '—'}</td></tr>
       <tr><th>SNR samples</th><td>${snr.count}</td></tr>`
    : `<tr><th>SNR</th><td>—</td></tr>`;

  table.innerHTML = `
    <tr><th>Frequency</th><td>${fmtFreq(rec.freq_hz)}</td></tr>
    <tr><th>Mode</th><td>${rec.audio_mode.toUpperCase()}</td></tr>
    <tr><th>Channel</th><td>${rec.label}</td></tr>
    <tr><th>Started</th><td>${fmtTime(rec.started_at)}</td></tr>
    <tr><th>Saved</th><td>${fmtTime(rec.saved_at)}</td></tr>
    <tr><th>Duration</th><td>${fmtDuration(rec.started_at, rec.saved_at)}</td></tr>
    <tr><th>Size</th><td>${rec.width} × ${rec.lines} px</td></tr>
    ${snrRow}`;

  updateDetailNav();
}

function closeDetail() {
  selectedID = null;
  document.getElementById('detail-view').classList.add('hidden');
  document.getElementById('live-panel').classList.remove('hidden');
  document.querySelectorAll('.thumb-card').forEach(c => c.classList.remove('selected'));
}

function updateDetailNav() {
  const idx = galleryRecords.findIndex(r => r.id === selectedID);
  document.getElementById('btn-prev').disabled = idx <= 0;
  document.getElementById('btn-next').disabled = idx < 0 || idx >= galleryRecords.length - 1;
}

document.getElementById('btn-close-detail').addEventListener('click', closeDetail);

document.getElementById('btn-rotate-left').addEventListener('click', () => {
  detailRotation = (detailRotation - 90 + 360) % 360;
  applyDetailRotation();
});

document.getElementById('btn-rotate-right').addEventListener('click', () => {
  detailRotation = (detailRotation + 90) % 360;
  applyDetailRotation();
});

document.getElementById('btn-prev').addEventListener('click', () => {
  const idx = galleryRecords.findIndex(r => r.id === selectedID);
  if (idx > 0) selectRecord(galleryRecords[idx - 1].id);
});

document.getElementById('btn-next').addEventListener('click', () => {
  const idx = galleryRecords.findIndex(r => r.id === selectedID);
  if (idx >= 0 && idx < galleryRecords.length - 1) selectRecord(galleryRecords[idx + 1].id);
});

document.getElementById('btn-delete').addEventListener('click', async () => {
  if (!selectedID) return;
  if (!confirm('Delete this image?')) return;
  try {
    const resp = await fetch(BASE_PATH + '/api/images/' + selectedID, { method: 'DELETE' });
    if (!resp.ok) { alert('Delete failed'); return; }
    removeRecordLocally(selectedID);
    closeDetail();
  } catch (e) {
    alert('Delete error: ' + e);
  }
});

function removeRecordLocally(id) {
  const idx = galleryRecords.findIndex(r => r.id === id);
  if (idx >= 0) galleryRecords.splice(idx, 1);
  const card = document.querySelector(`.thumb-card[data-id="${id}"]`);
  if (card) {
    const inner = card.parentElement; // .day-group-grid
    card.remove();
    // Remove the day-group entirely if it is now empty.
    if (inner && inner.querySelectorAll('.thumb-card').length === 0) {
      const group = inner.closest('.day-group');
      if (group) group.remove();
    } else if (inner) {
      updateGroupCount(inner.closest('.day-group'));
    }
  }
  updateGalleryCount();
}

// Lightbox on image click.
let lightboxRotation = 0;

function applyLightboxRotation() {
  const img = document.getElementById('lightbox-img');
  img.style.transform = lightboxRotation ? `rotate(${lightboxRotation}deg)` : '';
  // When rotated 90/270 degrees the image is transposed — swap the visual
  // dimensions so it still fits within the viewport without clipping.
  const transposed = lightboxRotation === 90 || lightboxRotation === 270;
  img.style.maxWidth  = transposed ? '90vh' : '';
  img.style.maxHeight = transposed ? '90vw' : '';
}

document.getElementById('detail-img').addEventListener('click', function () {
  lightboxRotation = 0;
  const lb = document.getElementById('lightbox');
  const lbImg = document.getElementById('lightbox-img');
  lbImg.src = this.src;
  applyLightboxRotation();
  lb.classList.remove('hidden');
});
document.getElementById('lightbox-close').addEventListener('click', () => {
  document.getElementById('lightbox').classList.add('hidden');
});
document.getElementById('lightbox-rotate-left').addEventListener('click', () => {
  lightboxRotation = (lightboxRotation - 90 + 360) % 360;
  applyLightboxRotation();
});
document.getElementById('lightbox-rotate-right').addEventListener('click', () => {
  lightboxRotation = (lightboxRotation + 90) % 360;
  applyLightboxRotation();
});
document.getElementById('lightbox').addEventListener('click', function (e) {
  if (e.target === this) this.classList.add('hidden');
});

// ---------------------------------------------------------------------------
// Live canvas — scrolling fax strip
// ---------------------------------------------------------------------------

function initLiveCanvas(width) {
  liveCanvas = document.getElementById('live-canvas');
  liveCtx = liveCanvas.getContext('2d');
  liveWidth = width;
  liveLineCount = 0;

  // Canvas height grows dynamically; start with 200 px.
  liveCanvas.width = width;
  liveCanvas.height = 200;
  liveCtx.fillStyle = '#888';
  liveCtx.fillRect(0, 0, width, 200);
}

function appendLiveLine(pixels) {
  if (!liveCanvas) {
    initLiveCanvas(pixels.length);
  }

  // Grow canvas if needed.
  if (liveLineCount >= liveCanvas.height) {
    const newH = liveCanvas.height + 200;
    const tmp = document.createElement('canvas');
    tmp.width = liveCanvas.width;
    tmp.height = liveCanvas.height;
    tmp.getContext('2d').drawImage(liveCanvas, 0, 0);
    liveCanvas.height = newH;
    liveCtx.drawImage(tmp, 0, 0);
  }

  // Draw one row.
  const imgData = liveCtx.createImageData(pixels.length, 1);
  for (let x = 0; x < pixels.length; x++) {
    const v = pixels[x];
    imgData.data[x * 4 + 0] = v;
    imgData.data[x * 4 + 1] = v;
    imgData.data[x * 4 + 2] = v;
    imgData.data[x * 4 + 3] = 255;
  }
  liveCtx.putImageData(imgData, 0, liveLineCount);
  liveLineCount++;

  // Auto-scroll the canvas wrap.
  const wrap = document.getElementById('live-canvas-wrap');
  wrap.scrollTop = wrap.scrollHeight;

  document.getElementById('live-line-count').textContent = liveLineCount + ' lines';
}

function resetLiveCanvas() {
  liveCanvas = null;
  liveCtx = null;
  liveWidth = 0;
  liveLineCount = 0;
  const c = document.getElementById('live-canvas');
  c.width = 1;
  c.height = 1;
  document.getElementById('live-line-count').textContent = '';
}

// ---------------------------------------------------------------------------
// SSE
// ---------------------------------------------------------------------------

function reconnectSSE() {
  if (sseSource) {
    sseSource.close();
    sseSource = null;
  }
  connectSSE();
}

function connectSSE() {
  const url = activeLabel
    ? BASE_PATH + '/api/live?label=' + encodeURIComponent(activeLabel)
    : BASE_PATH + '/api/live';

  sseSource = new EventSource(url);

  sseSource.addEventListener('fax_line', e => {
    const ev = JSON.parse(e.data);
    const data = ev.data;
    if (!data) return;

    // Only draw lines from the channel currently locked for live display.
    if (data.label !== liveDrawingLabel) return;

    // Decode base64 pixels.
    const b64 = data.pixels_b64;
    if (!b64) return;
    const bin = atob(b64);
    const pixels = new Uint8Array(bin.length);
    for (let i = 0; i < bin.length; i++) pixels[i] = bin.charCodeAt(i);

    appendLiveLine(pixels);
  });

  sseSource.addEventListener('channel_start', e => {
    const ev = JSON.parse(e.data);
    const data = ev.data;
    if (!data) return;

    // Update badge for this channel immediately.
    updateBadgeState(data.label, true, 'running');

    // If a specific channel is selected, only respond to that one.
    if (activeLabel && data.label !== activeLabel) return;

    // If we are already drawing a different channel, ignore this start —
    // the user will see the badge turn 'receiving' and can switch if desired.
    if (liveDrawingLabel !== null && liveDrawingLabel !== data.label) return;

    // Lock onto this channel and start a fresh canvas.
    liveDrawingLabel = data.label;
    resetLiveCanvas();
    liveLabel = data.label;
    document.getElementById('live-label').textContent =
      `Live: ${fmtFreq(data.freq_hz)} — ${data.label}`;
    // Show live panel if detail is not open.
    if (!selectedID) {
      document.getElementById('live-panel').classList.remove('hidden');
    }
    // When "all" is selected, follow the live channel for SNR.
    if (!activeLabel) connectSNR(data.label);
  });

  sseSource.addEventListener('channel_stop', e => {
    const ev = JSON.parse(e.data);
    const data = ev.data;
    if (!data) return;

    // Update badge for this channel immediately.
    updateBadgeState(data.label, false, 'running');

    // Unlock live canvas if this was the channel being drawn.
    if (liveDrawingLabel === data.label) {
      liveDrawingLabel = null;
      document.getElementById('live-label').textContent = 'Waiting for signal…';
      // Disconnect SNR when "all" mode loses its live channel.
      if (!activeLabel) disconnectSNR();
    }
  });

  sseSource.addEventListener('new_image', e => {
    const ev = JSON.parse(e.data);
    const rec = ev.data;
    if (!rec) return;

    // Unlock live canvas when the image is saved (channel finished).
    if (liveDrawingLabel === rec.label) {
      liveDrawingLabel = null;
      document.getElementById('live-label').textContent = 'Waiting for signal…';
    }

    if (activeLabel && rec.label !== activeLabel) return;
    // Prepend to gallery.
    galleryRecords.unshift(rec);
    prependThumbCard(rec);
    galleryOffset++;
    updateGalleryCount();
  });

  sseSource.addEventListener('image_deleted', e => {
    const ev = JSON.parse(e.data);
    const data = ev.data;
    if (!data) return;
    removeRecordLocally(data.id);
  });

  sseSource.onerror = () => {
    // Browser will auto-reconnect for EventSource; just log.
    console.warn('[SSE] connection error, browser will retry');
  };
}

// ---------------------------------------------------------------------------
// Audio preview
// ---------------------------------------------------------------------------

async function startAudioPreview(label) {
  stopAudioPreview();
  if (!label) return;

  // Create a local context so the pump() closure below is not affected by
  // a subsequent stopAudioPreview() nulling the module-level audioCtx.
  let localCtx;
  try {
    localCtx = new (window.AudioContext || window.webkitAudioContext)();
  } catch (e) {
    console.warn('Web Audio API not available:', e);
    return;
  }
  if (!localCtx) return;
  audioCtx = localCtx;

  // Start muted by default — the stream and FFT still flow, but no sound.
  if (audioMuted) localCtx.suspend();

  let resp;
  try {
    resp = await fetch(BASE_PATH + '/api/audio/preview?label=' + encodeURIComponent(label));
  } catch (e) {
    localCtx.close();
    if (audioCtx === localCtx) audioCtx = null;
    return;
  }
  // If another startAudioPreview call replaced our context while we were
  // waiting for the fetch, bail out silently.
  if (audioCtx !== localCtx) {
    localCtx.close();
    return;
  }
  if (!resp.ok) {
    localCtx.close();
    audioCtx = null;
    return;
  }

  audioReader = resp.body.getReader();
  audioPlaying = true;
  connectFFT(label);
  updateMuteBtn();

  // WAV header is 44 bytes; skip it.
  let headerSkip = 44;
  let scheduledUntil = localCtx.currentTime;

  async function pump() {
    while (audioPlaying) {
      let result;
      try {
        result = await audioReader.read();
      } catch (e) {
        break;
      }
      if (result.done) break;
      // Stop if our context was replaced by a newer call.
      if (audioCtx !== localCtx) break;
      let chunk = result.value;

      if (headerSkip > 0) {
        if (chunk.length <= headerSkip) {
          headerSkip -= chunk.length;
          continue;
        }
        chunk = chunk.slice(headerSkip);
        headerSkip = 0;
      }

      // chunk is S16LE PCM; convert to float32.
      const samples = chunk.length / 2;
      const buf = localCtx.createBuffer(1, samples, 8000);
      const ch = buf.getChannelData(0);
      const view = new DataView(chunk.buffer, chunk.byteOffset, chunk.byteLength);
      for (let i = 0; i < samples; i++) {
        ch[i] = view.getInt16(i * 2, true) / 32768;
      }

      const src = localCtx.createBufferSource();
      src.buffer = buf;
      src.connect(localCtx.destination);
      const startAt = Math.max(scheduledUntil, localCtx.currentTime + 0.05);
      src.start(startAt);
      scheduledUntil = startAt + buf.duration;
    }
    // Only call stopAudioPreview if we are still the active context.
    if (audioCtx === localCtx) stopAudioPreview();
  }

  pump();
}

function stopAudioPreview() {
  audioPlaying = false;
  if (audioReader) {
    audioReader.cancel();
    audioReader = null;
  }
  if (audioCtx) {
    audioCtx.close();
    audioCtx = null;
  }
  updateMuteBtn();
  disconnectFFT();
}

// updateMuteBtn keeps the mute button label and disabled state in sync.
function updateMuteBtn() {
  const btn = document.getElementById('btn-audio-mute');
  if (!btn) return;
  if (!audioPlaying) {
    btn.disabled = true;
    btn.textContent = '🔇 Muted';
    btn.title = 'No channel selected';
    return;
  }
  btn.disabled = false;
  if (audioMuted) {
    btn.textContent = '🔇 Muted';
    btn.title = 'Click to unmute audio';
  } else {
    btn.textContent = '🔊 Unmute';
    btn.title = 'Click to mute audio';
  }
}

document.getElementById('btn-audio-mute').addEventListener('click', () => {
  if (!audioCtx) return;
  audioMuted = !audioMuted;
  if (audioMuted) {
    audioCtx.suspend();
  } else {
    audioCtx.resume();
  }
  updateMuteBtn();
});

// When the audio channel dropdown changes (user interaction), auto-start.
document.getElementById('audio-channel-select').addEventListener('change', function () {
  // User explicitly picked a channel in the audio dropdown — auto-start.
  if (this.value) {
    startAudioPreview(this.value);
  } else {
    stopAudioPreview();
  }
});

// syncAudioToChannel is called from any channel-selection method (main
// channel-select dropdown, badge clicks) to keep the audio dropdown in sync
// and auto-start the muted audio stream for the selected channel.
function syncAudioToChannel(label) {
  const sel = document.getElementById('audio-channel-select');
  sel.value = label || '';
  if (label) {
    startAudioPreview(label);
  } else {
    stopAudioPreview();
  }
}

// ---------------------------------------------------------------------------
// Audio panel — spectrum / waterfall / VU meter
// ---------------------------------------------------------------------------

// HSV → RGB helper (h in [0,360], s/v in [0,1]) → [r,g,b] in [0,255]
function hsvToRgb(h, s, v) {
  const c = v * s;
  const x = c * (1 - Math.abs((h / 60) % 2 - 1));
  const m = v - c;
  let r = 0, g = 0, b = 0;
  if      (h < 60)  { r = c; g = x; b = 0; }
  else if (h < 120) { r = x; g = c; b = 0; }
  else if (h < 180) { r = 0; g = c; b = x; }
  else if (h < 240) { r = 0; g = x; b = c; }
  else if (h < 300) { r = x; g = 0; b = c; }
  else              { r = c; g = 0; b = x; }
  return [Math.round((r + m) * 255), Math.round((g + m) * 255), Math.round((b + m) * 255)];
}

// Render one FFT frame onto the spectrum and waterfall canvases.
function renderFFTFrame(frame) {
  const bins = frame.bins;
  const nBins = bins.length;

  // ── Spectrum canvas ──────────────────────────────────────────────────────
  const specCanvas = document.getElementById('spectrum-canvas');
  if (specCanvas) {
    // Lazy-init backing dimensions from CSS layout.
    if (specCanvas.width !== specCanvas.offsetWidth && specCanvas.offsetWidth > 0) {
      specCanvas.width  = specCanvas.offsetWidth;
      specCanvas.height = specCanvas.offsetHeight || 80;
    }
    if (specCanvas.width > 0 && specCanvas.height > 0) {
      const ctx = specCanvas.getContext('2d');
      const w = specCanvas.width;
      const h = specCanvas.height;

      ctx.fillStyle = '#00007f';
      ctx.fillRect(0, 0, w, h);

      // Marker lines (red)
      ctx.strokeStyle = '#e94560';
      ctx.lineWidth = 1;
      const span = audioPanel.fftHigh - audioPanel.fftLow;
      for (const hz of audioPanel.markers) {
        const x = Math.round(((hz - audioPanel.fftLow) / span) * w);
        ctx.beginPath(); ctx.moveTo(x, 0); ctx.lineTo(x, h); ctx.stroke();
      }

      // Spectrum polyline (green)
      ctx.strokeStyle = '#00ff00';
      ctx.lineWidth = 1;
      ctx.beginPath();
      for (let j = 0; j < nBins; j++) {
        const x = Math.round((j / (nBins - 1)) * (w - 1));
        let t = (audioPanel.maxDb - bins[j]) / audioPanel.range;
        if (t < 0) t = 0; if (t > 1) t = 1;
        const y = Math.round(t * (h - 1));
        if (j === 0) ctx.moveTo(x, y); else ctx.lineTo(x, y);
      }
      ctx.stroke();
    }
  }

  // ── Waterfall canvas ─────────────────────────────────────────────────────
  if (!waterfallCtx || !waterfallImg) {
    const wc = document.getElementById('waterfall-canvas');
    if (wc && wc.offsetWidth > 0 && wc.offsetHeight > 0) {
      wc.width  = wc.offsetWidth;
      wc.height = wc.offsetHeight;
      waterfallCtx = wc.getContext('2d');
      waterfallImg = waterfallCtx.createImageData(wc.width, wc.height);
      for (let i = 0; i < waterfallImg.data.length; i += 4) {
        waterfallImg.data[i] = 0; waterfallImg.data[i+1] = 0;
        waterfallImg.data[i+2] = 0; waterfallImg.data[i+3] = 255;
      }
    }
  }
  if (waterfallCtx && waterfallImg) {
    const w = waterfallImg.width;
    const h = waterfallImg.height;
    const rowBytes = w * 4;
    // Scroll down by 1 row
    waterfallImg.data.copyWithin(rowBytes, 0, (h - 1) * rowBytes);
    // Write new top row
    for (let j = 0; j < w; j++) {
      const binIdx = Math.round((j / (w - 1)) * (nBins - 1));
      let t = 1 - (audioPanel.maxDb - bins[binIdx]) / audioPanel.range;
      if (t < 0) t = 0; if (t > 1) t = 1;
      const hue = 240 - t * 60;
      const [r, g, b] = hsvToRgb(hue, 1, t);
      const idx = j * 4;
      waterfallImg.data[idx]   = r;
      waterfallImg.data[idx+1] = g;
      waterfallImg.data[idx+2] = b;
      waterfallImg.data[idx+3] = 255;
    }
    waterfallCtx.putImageData(waterfallImg, 0, 0);
  }

  // ── VU meter ─────────────────────────────────────────────────────────────
  const vuBar = document.getElementById('vu-bar');
  if (vuBar) {
    const vdb = frame.volume_db;
    let pct = Math.max(0, Math.min(100, (vdb + 60) / 60 * 100));
    vuBar.style.width = pct + '%';
    vuBar.style.backgroundColor = pct > 85 ? '#eb5757' : pct > 60 ? '#f2c94c' : '#6fcf97';
  }
}

function drawAudioMarkers(canvas) {
  if (!canvas) return;
  const ctx = canvas.getContext('2d');
  const w = canvas.offsetWidth || canvas.width;
  canvas.width = w;
  ctx.clearRect(0, 0, w, canvas.height);
  const span = audioPanel.fftHigh - audioPanel.fftLow;
  for (const hz of audioPanel.markers) {
    const x = Math.round(((hz - audioPanel.fftLow) / span) * w);
    ctx.fillStyle = '#555';
    ctx.fillRect(x, 0, 1, canvas.height);
    ctx.fillStyle = '#888';
    ctx.font = '6px monospace';
    ctx.fillText((hz / 1000).toFixed(1) + 'k', x + 2, canvas.height - 1);
  }
}

function initAudioPanel() {
  const spectrumCanvas  = document.getElementById('spectrum-canvas');
  const waterfallCanvas = document.getElementById('waterfall-canvas');
  const markerSpectrum  = document.getElementById('marker-spectrum');
  const markerWaterfall = document.getElementById('marker-waterfall');
  const ctrlMaxDb = document.getElementById('ctrl-maxdb');
  const ctrlRange = document.getElementById('ctrl-range');

  if (ctrlMaxDb) {
    audioPanel.maxDb = parseFloat(ctrlMaxDb.value) || audioPanel.maxDb;
    ctrlMaxDb.addEventListener('input', () => { audioPanel.maxDb = parseFloat(ctrlMaxDb.value) || -25; });
  }
  if (ctrlRange) {
    audioPanel.range = parseFloat(ctrlRange.value) || audioPanel.range;
    ctrlRange.addEventListener('input', () => { audioPanel.range = parseFloat(ctrlRange.value) || 60; });
  }

  if (!spectrumCanvas || !waterfallCanvas) return;

  const ro = new ResizeObserver(() => {
    const w  = spectrumCanvas.offsetWidth;
    const sh = spectrumCanvas.offsetHeight;
    const wh = waterfallCanvas.offsetHeight;
    if (w <= 0 || sh <= 0 || wh <= 0) return;
    if (spectrumCanvas.width !== w || spectrumCanvas.height !== sh) {
      spectrumCanvas.width  = w;
      spectrumCanvas.height = sh;
    }
    if (waterfallCanvas.width !== w || waterfallCanvas.height !== wh) {
      waterfallCanvas.width  = w;
      waterfallCanvas.height = wh;
      waterfallCtx = waterfallCanvas.getContext('2d');
      waterfallImg = waterfallCtx.createImageData(w, wh);
      for (let i = 0; i < waterfallImg.data.length; i += 4) {
        waterfallImg.data[i] = 0; waterfallImg.data[i+1] = 0;
        waterfallImg.data[i+2] = 0; waterfallImg.data[i+3] = 255;
      }
    }
    drawAudioMarkers(markerSpectrum);
    drawAudioMarkers(markerWaterfall);
  });
  ro.observe(spectrumCanvas);
  ro.observe(waterfallCanvas);

  requestAnimationFrame(() => {
    drawAudioMarkers(markerSpectrum);
    drawAudioMarkers(markerWaterfall);
  });
}

function connectFFT(label) {
  if (fftES) { fftES.close(); fftES = null; }
  if (!label) return;
  fftLabel = label;
  const url = BASE_PATH + '/api/fft?label=' + encodeURIComponent(label);
  fftES = new EventSource(url);
  fftES.addEventListener('fft', e => {
    try { renderFFTFrame(JSON.parse(e.data)); }
    catch (err) { console.error('FFT parse error', err); }
  });
  fftES.onerror = () => {
    fftES.close(); fftES = null;
    // Reconnect after 5 s only if still playing.
    if (audioPlaying) setTimeout(() => connectFFT(fftLabel), 5000);
  };
}

function disconnectFFT() {
  if (fftES) { fftES.close(); fftES = null; }
}

// ---------------------------------------------------------------------------
// SNR colour: red at 30 dB → orange at 40 dB → green at 50+ dB
// Matches ubersdr_qsstv snrColor() exactly.
// ---------------------------------------------------------------------------
function snrColor(snrDB) {
  // Clamp to [30, 50] then map to hue [0°=red, 120°=green]
  const t = Math.max(0, Math.min(1, (snrDB - 30) / 20));
  const hue = Math.round(t * 120); // 0 → 120
  return `hsl(${hue}, 100%, 50%)`;
}

// ---------------------------------------------------------------------------
// SNR display — always active for the selected channel
// ---------------------------------------------------------------------------

function updateSNRDisplay(stats) {
  const valueEl = document.getElementById('snr-value');
  const barEl   = document.getElementById('snr-bar');
  if (!valueEl || !barEl) return;

  if (!stats || stats.count === 0) {
    valueEl.textContent = '—';
    barEl.style.width = '0%';
    barEl.style.backgroundColor = snrColor(30);
    return;
  }

  const snr = stats.avg_db;
  valueEl.textContent = snr.toFixed(1) + ' dB';

  // Map SNR to bar: 30 dB → 0%, 50 dB → 100% (matches snrColor scale)
  const pct = Math.max(0, Math.min(100, ((snr - 30) / 20) * 100));
  barEl.style.width = pct + '%';
  barEl.style.backgroundColor = snrColor(snr);
}

function connectSNR(label) {
  if (snrES) { snrES.close(); snrES = null; }
  // Reset display when no channel selected.
  if (!label) { updateSNRDisplay(null); return; }
  snrLabel = label;
  const url = BASE_PATH + '/api/snr?label=' + encodeURIComponent(label);
  snrES = new EventSource(url);
  snrES.addEventListener('snr', e => {
    try { updateSNRDisplay(JSON.parse(e.data)); }
    catch (err) { console.error('SNR parse error', err); }
  });
  snrES.onerror = () => {
    snrES.close(); snrES = null;
    setTimeout(() => { if (snrLabel) connectSNR(snrLabel); }, 5000);
  };
}

function disconnectSNR() {
  if (snrES) { snrES.close(); snrES = null; }
  updateSNRDisplay(null);
}

// ---------------------------------------------------------------------------
// Poll channel status periodically
// ---------------------------------------------------------------------------

setInterval(() => {
  loadChannels();
}, 10000);

// ---------------------------------------------------------------------------
// Init
// ---------------------------------------------------------------------------

(async function init() {
  initAudioPanel();
  await loadChannels();
  await loadMoreImages();
  connectSSE();
  // Connect SNR for the initially selected channel (if any).
  if (activeLabel) connectSNR(activeLabel);
})();
