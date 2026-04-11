/* metrics.js — Decode statistics modal */
'use strict';

// Inherit BASE_PATH set by app.js (both scripts share the same window scope).
// Falls back to '' when accessed directly without the proxy.
const _metricsBasePath = (typeof BASE_PATH === 'string') ? BASE_PATH : '';

// ---------------------------------------------------------------------------
// State
// ---------------------------------------------------------------------------
let metricsChart = null;
let metricsCurrentPeriod = '24h';

// ---------------------------------------------------------------------------
// Open / close
// ---------------------------------------------------------------------------
function openMetricsModal() {
  const modal = document.getElementById('metrics-modal');
  if (!modal) return;
  modal.classList.add('open');
  document.body.style.overflow = 'hidden';
  fetchMetrics(metricsCurrentPeriod);
}

function closeMetricsModal() {
  const modal = document.getElementById('metrics-modal');
  if (!modal) return;
  modal.classList.remove('open');
  document.body.style.overflow = '';
}

// ---------------------------------------------------------------------------
// Fetch + render
// ---------------------------------------------------------------------------
function fetchMetrics(period) {
  metricsCurrentPeriod = period;

  // Update active tab
  document.querySelectorAll('.period-tab').forEach(btn => {
    btn.classList.toggle('active', btn.dataset.period === period);
  });

  // Clear summary while loading
  ['metrics-total', 'metrics-complete', 'metrics-partial', 'metrics-snr'].forEach(id => {
    const el = document.getElementById(id);
    if (el) el.textContent = '…';
  });

  fetch(_metricsBasePath + '/api/metrics?period=' + encodeURIComponent(period))
    .then(r => {
      if (!r.ok) throw new Error('HTTP ' + r.status);
      return r.json();
    })
    .then(data => renderMetrics(data))
    .catch(err => {
      console.error('metrics fetch:', err);
      ['metrics-total', 'metrics-complete', 'metrics-partial', 'metrics-snr'].forEach(id => {
        const el = document.getElementById(id);
        if (el) el.textContent = '—';
      });
    });
}

function renderMetrics(data) {
  // Summary stats
  const totalEl    = document.getElementById('metrics-total');
  const completeEl = document.getElementById('metrics-complete');
  const partialEl  = document.getElementById('metrics-partial');
  const snrEl      = document.getElementById('metrics-snr');

  if (totalEl)    totalEl.textContent    = data.total    != null ? data.total    : '—';
  if (completeEl) completeEl.textContent = data.complete != null ? data.complete : '—';
  if (partialEl)  partialEl.textContent  = data.partial  != null ? data.partial  : '—';
  if (snrEl) {
    if (data.avg_snr_db) {
      snrEl.textContent  = data.avg_snr_db.toFixed(1) + ' dB';
      snrEl.style.color  = snrColor(data.avg_snr_db);
    } else {
      snrEl.textContent  = '—';
      snrEl.style.color  = '';
    }
  }

  // By-mode chips
  const modeEl = document.getElementById('metrics-by-mode');
  if (modeEl) {
    modeEl.innerHTML = '';
    if (data.by_mode && Object.keys(data.by_mode).length > 0) {
      // Sort by count descending
      const sorted = Object.entries(data.by_mode).sort((a, b) => b[1] - a[1]);
      for (const [mode, count] of sorted) {
        const chip = document.createElement('span');
        chip.className = 'metrics-mode-chip';
        chip.textContent = mode + ' × ' + count;
        modeEl.appendChild(chip);
      }
    } else {
      modeEl.innerHTML = '<span style="color:#555;font-size:0.8rem">No decodes in this period</span>';
    }
  }

  // Chart
  renderMetricsChart(data);
}

function renderMetricsChart(data) {
  const canvas = document.getElementById('metrics-chart');
  if (!canvas) return;

  if (metricsChart) {
    metricsChart.destroy();
    metricsChart = null;
  }

  const buckets = data.by_hour || [];

  if (buckets.length === 0) {
    // Nothing to show — draw a placeholder message
    const ctx = canvas.getContext('2d');
    ctx.clearRect(0, 0, canvas.width, canvas.height);
    ctx.fillStyle = '#555';
    ctx.font = '14px sans-serif';
    ctx.textAlign = 'center';
    ctx.fillText('No decodes in this period', canvas.width / 2, canvas.height / 2);
    return;
  }

  const completeData = buckets.map(b => ({ x: b.t, y: b.complete }));
  const partialData  = buckets.map(b => ({ x: b.t, y: b.partial  }));

  // Choose time unit based on period
  let timeUnit = 'hour';
  let displayFormat = { hour: 'HH:mm', day: 'MMM d' };
  if (metricsCurrentPeriod === '7d' || metricsCurrentPeriod === '30d') {
    timeUnit = 'day';
  }

  metricsChart = new Chart(canvas, {
    type: 'bar',
    data: {
      datasets: [
        {
          label: 'Complete',
          data: completeData,
          backgroundColor: 'rgba(111, 207, 151, 0.75)',
          borderColor: '#6fcf97',
          borderWidth: 1,
          stack: 'decodes',
        },
        {
          label: 'Partial',
          data: partialData,
          backgroundColor: 'rgba(242, 201, 76, 0.65)',
          borderColor: '#f2c94c',
          borderWidth: 1,
          stack: 'decodes',
        },
      ],
    },
    options: {
      responsive: true,
      maintainAspectRatio: false,
      animation: false,
      parsing: false,
      plugins: {
        legend: {
          labels: { color: '#aaa', font: { size: 11 }, boxWidth: 12 },
        },
        tooltip: {
          callbacks: {
            title: ctx => {
              const d = new Date(ctx[0].parsed.x);
              if (timeUnit === 'day') {
                return d.toLocaleDateString(undefined, { weekday: 'short', month: 'short', day: 'numeric' });
              }
              return d.toLocaleString(undefined, { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit' });
            },
            label: ctx => `${ctx.dataset.label}: ${ctx.parsed.y}`,
          },
        },
      },
      scales: {
        x: {
          type: 'time',
          time: {
            unit: timeUnit,
            displayFormats: displayFormat,
          },
          stacked: true,
          ticks: { color: '#888', font: { size: 10 }, maxTicksLimit: 12 },
          grid: { color: '#1e2d4a' },
        },
        y: {
          stacked: true,
          beginAtZero: true,
          ticks: {
            color: '#888',
            font: { size: 10 },
            stepSize: 1,
            precision: 0,
          },
          grid: { color: '#1e2d4a' },
          title: { display: true, text: 'Decodes', color: '#888', font: { size: 10 } },
        },
      },
    },
  });
}

// ---------------------------------------------------------------------------
// Boot — wire up once DOM is ready
// ---------------------------------------------------------------------------
document.addEventListener('DOMContentLoaded', () => {
  // Open button
  const btn = document.getElementById('metrics-btn');
  if (btn) btn.addEventListener('click', openMetricsModal);

  // Close button
  const closeBtn = document.getElementById('metrics-modal-close');
  if (closeBtn) closeBtn.addEventListener('click', closeMetricsModal);

  // Backdrop click
  const modal = document.getElementById('metrics-modal');
  if (modal) {
    modal.addEventListener('click', e => {
      if (e.target === modal) closeMetricsModal();
    });
  }

  // Escape key
  document.addEventListener('keydown', e => {
    if (e.key === 'Escape') {
      const m = document.getElementById('metrics-modal');
      if (m && m.classList.contains('open')) closeMetricsModal();
    }
  });

  // Period tabs
  document.querySelectorAll('.period-tab').forEach(tab => {
    tab.addEventListener('click', () => fetchMetrics(tab.dataset.period));
  });
});
