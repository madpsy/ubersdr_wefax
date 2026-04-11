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
  while (sel.options.length > 1) sel.remove(1);
  for (const ch of channels) {
    const opt = document.createElement('option');
    opt.value = ch.label;
    opt.textContent = `${fmtFreq(ch.freq_hz)} ${ch.audio_mode.toUpperCase()}`;
    sel.appendChild(opt);
  }
  // Mirror the main channel selector (no audio restart — just keep in sync).
  sel.value = activeLabel || '';
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
    badge.style.cursor = 'pointer';
    badge.addEventListener('click', () => {
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
  // Clear the live drawing lock so the next channel_start can take over.
  liveDrawingLabel = null;
  resetLiveCanvas();
  document.getElementById('live-label').textContent = 'Waiting for signal…';
  // Sync audio preview dropdown and restart stream if playing.
  syncAudioToChannel(activeLabel);
  resetGallery();
  loadMoreImages();
  reconnectSSE();
});

// ---------------------------------------------------------------------------
// Gallery
// ---------------------------------------------------------------------------

function resetGallery() {
  galleryRecords = [];
  galleryOffset = 0;
  galleryExhausted = false;
  document.getElementById('gallery-list').innerHTML = '';
  document.getElementById('gallery-count').textContent = '';
}

async function loadMoreImages() {
  if (galleryExhausted) return;
  const params = new URLSearchParams({ limit: GALLERY_PAGE, offset: galleryOffset });
  if (activeLabel) params.set('label', activeLabel);
  try {
    const resp = await fetch(BASE_PATH + '/api/images?' + params);
    if (!resp.ok) return;
    const data = await resp.json();
    const imgs = data.images || [];
    if (imgs.length < GALLERY_PAGE) galleryExhausted = true;
    for (const rec of imgs) {
      galleryRecords.push(rec);
      appendThumbCard(rec);
    }
    galleryOffset += imgs.length;
    updateGalleryCount();
    document.getElementById('btn-load-more').disabled = galleryExhausted;
  } catch (e) {
    console.warn('loadMoreImages:', e);
  }
}

function updateGalleryCount() {
  const el = document.getElementById('gallery-count');
  el.textContent = galleryRecords.length + (galleryExhausted ? '' : '+') + ' images';
}

function appendThumbCard(rec) {
  const list = document.getElementById('gallery-list');
  const card = buildThumbCard(rec);
  list.appendChild(card);
}

function prependThumbCard(rec) {
  const list = document.getElementById('gallery-list');
  const card = buildThumbCard(rec);
  list.insertBefore(card, list.firstChild);
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

document.getElementById('btn-load-more').addEventListener('click', loadMoreImages);

// ---------------------------------------------------------------------------
// Detail view
// ---------------------------------------------------------------------------

function selectRecord(id) {
  selectedID = id;
  const rec = galleryRecords.find(r => r.id === id);
  if (!rec) return;

  // Highlight selected card.
  document.querySelectorAll('.thumb-card').forEach(c => c.classList.remove('selected'));
  const card = document.querySelector(`.thumb-card[data-id="${id}"]`);
  if (card) card.classList.add('selected');

  // Show detail panel.
  document.getElementById('live-panel').classList.add('hidden');
  const dv = document.getElementById('detail-view');
  dv.classList.remove('hidden');

  document.getElementById('detail-img').src = BASE_PATH + '/images/' + rec.filename;

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
  if (card) card.remove();
  updateGalleryCount();
}

// Lightbox on image click.
document.getElementById('detail-img').addEventListener('click', function () {
  const lb = document.getElementById('lightbox');
  document.getElementById('lightbox-img').src = this.src;
  lb.classList.remove('hidden');
});
document.getElementById('lightbox-close').addEventListener('click', () => {
  document.getElementById('lightbox').classList.add('hidden');
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
  document.getElementById('btn-audio-play').disabled = true;
  document.getElementById('btn-audio-stop').disabled = false;

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
  document.getElementById('btn-audio-play').disabled = false;
  document.getElementById('btn-audio-stop').disabled = true;
}

document.getElementById('btn-audio-play').addEventListener('click', () => {
  const label = document.getElementById('audio-channel-select').value;
  if (!label) { alert('Select a channel first'); return; }
  startAudioPreview(label);
});

document.getElementById('btn-audio-stop').addEventListener('click', stopAudioPreview);

// When the audio channel dropdown changes (user interaction), restart preview.
document.getElementById('audio-channel-select').addEventListener('change', function () {
  syncAudioToChannel(this.value);
});

// syncAudioToChannel sets the audio dropdown to label and, if audio is
// currently playing, stops the old stream and starts the new one.
function syncAudioToChannel(label) {
  const sel = document.getElementById('audio-channel-select');
  sel.value = label || '';
  if (audioPlaying) {
    if (label) {
      startAudioPreview(label);
    } else {
      stopAudioPreview();
    }
  }
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
  await loadChannels();
  await loadMoreImages();
  connectSSE();
})();
