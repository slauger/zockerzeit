(function () {
  "use strict";

  const root = document.getElementById("stats");
  if (!root || typeof Chart === "undefined") return;

  const css = getComputedStyle(document.documentElement);
  const v = (name) => css.getPropertyValue(name).trim();
  const palette = [v("--series-1"), v("--series-2"), v("--series-3"), v("--series-4"), v("--series-5"), v("--series-6")];
  const text = v("--text-muted");
  const grid = v("--border");

  Chart.defaults.color = text;
  Chart.defaults.borderColor = grid;
  Chart.defaults.font.family = getComputedStyle(document.body).fontFamily;

  const fmtMin = (m) => {
    m = Math.round(m);
    return m < 60 ? m + " Min" : Math.floor(m / 60) + ":" + String(m % 60).padStart(2, "0") + " Std";
  };

  const params = new URLSearchParams({ profile: root.dataset.profile, days: root.dataset.days });
  fetch("/api/stats?" + params, { credentials: "same-origin" })
    .then((r) => (r.ok ? r.json() : Promise.reject(r.status)))
    .then(render)
    .catch((err) => {
      root.insertAdjacentHTML("afterbegin", '<div class="banner err">Statistik konnte nicht geladen werden (' + err + ").</div>");
    });

  // Each console keeps the same color across all charts.
  const colors = {};
  const colorFor = (name) => {
    if (!(name in colors)) colors[name] = palette[Object.keys(colors).length % palette.length];
    return colors[name];
  };

  function render(data) {
    (data.daily.series || []).forEach((s) => colorFor(s.name));
    (data.consoles || []).forEach((s) => colorFor(s.name));
    daily(data);
    heatmap(data.heatmap);
    doughnut("chart-consoles", data.consoles, (s) => colorFor(s.name));
  }

  function daily(data) {
    const d = data.daily;
    const datasets = (d.series || []).map((s, i) => ({
      type: "bar",
      label: s.name,
      data: s.data.map((x) => Math.round(x)),
      backgroundColor: colorFor(s.name),
      borderRadius: 4,
      stack: "consoles",
    }));
    datasets.push({
      type: "line",
      label: "Gesamt",
      data: d.total.map((x) => Math.round(x)),
      borderColor: v("--text"),
      backgroundColor: v("--text"),
      pointRadius: 3,
      stack: "total",
    });
    if (data.limit > 0) {
      datasets.push({
        type: "line",
        label: "Tageslimit",
        data: d.labels.map(() => data.limit),
        borderColor: v("--danger"),
        borderDash: [6, 4],
        pointRadius: 0,
        stack: "limit",
      });
    }
    new Chart(document.getElementById("chart-daily"), {
      data: { labels: d.labels, datasets },
      options: {
        maintainAspectRatio: false,
        interaction: { mode: "index", intersect: false },
        scales: {
          x: { stacked: true, grid: { display: false } },
          y: { stacked: true, beginAtZero: true, title: { display: true, text: "Minuten" } },
        },
        plugins: { tooltip: { callbacks: { label: (c) => c.dataset.label + ": " + fmtMin(c.parsed.y) } } },
      },
    });
  }

  function heatmap(hm) {
    const el = document.getElementById("heatmap");
    const days = ["Mo", "Di", "Mi", "Do", "Fr", "Sa", "So"];
    const max = Math.max(1, ...hm.flat());
    let html = '<div class="hm-row hm-head" role="row"><span></span>';
    for (let h = 0; h < 24; h++) html += '<span role="columnheader">' + (h % 3 === 0 ? h : "") + "</span>";
    html += "</div>";
    hm.forEach((row, d) => {
      html += '<div class="hm-row" role="row"><span class="hm-day" role="rowheader">' + days[d] + "</span>";
      row.forEach((m, h) => {
        const a = m > 0 ? 0.15 + 0.85 * (m / max) : 0;
        const title = days[d] + " " + h + ":00–" + (h + 1) + ":00: " + fmtMin(m);
        html += '<span class="hm-cell" role="cell" title="' + title + '" style="--a:' + a.toFixed(2) + '"></span>';
      });
      html += "</div>";
    });
    el.innerHTML = html;
  }

  function doughnut(id, slices, color) {
    slices = slices || [];
    const canvas = document.getElementById(id);
    if (!slices.length) {
      canvas.parentElement.innerHTML = '<p class="muted">Noch keine Daten.</p>';
      return;
    }
    new Chart(canvas, {
      type: "doughnut",
      data: {
        labels: slices.map((s) => s.name),
        datasets: [{ data: slices.map((s) => Math.round(s.minutes)), backgroundColor: slices.map(color), borderColor: v("--surface"), borderWidth: 2 }],
      },
      options: {
        maintainAspectRatio: false,
        cutout: "62%",
        plugins: {
          legend: { position: "right" },
          tooltip: { callbacks: { label: (c) => c.label + ": " + fmtMin(c.parsed) } },
        },
      },
    });
  }
})();
