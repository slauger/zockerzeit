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

  const params = new URLSearchParams({ profile: root.dataset.profile, days: root.dataset.days, day: root.dataset.day });
  fetch("/api/stats?" + params, { credentials: "same-origin" })
    .then((r) => (r.ok ? r.json() : Promise.reject(r.status)))
    .then(render)
    .catch((err) => {
      root.insertAdjacentHTML("afterbegin", '<div class="banner err">Statistik konnte nicht geladen werden (' + err + ").</div>");
    });

  // Each device keeps the same color across all charts.
  const colors = {};
  const colorFor = (name) => {
    if (!(name in colors)) colors[name] = palette[Object.keys(colors).length % palette.length];
    return colors[name];
  };

  function render(data) {
    (data.daily.series || []).forEach((s) => colorFor(s.name));
    (data.devices || []).forEach((s) => colorFor(s.name));
    daily(data);
    timeline(data);
    heatmap(data.heatmap);
    doughnut("chart-locations", data.locations, (_, i) => palette[palette.length - 1 - (i % palette.length)]);
    doughnut("chart-devices", data.devices, (s) => colorFor(s.name));
  }

  function daily(data) {
    const d = data.daily;
    const datasets = (d.series || []).map((s, i) => ({
      type: "bar",
      label: s.name,
      data: s.data.map((x) => Math.round(x)),
      backgroundColor: colorFor(s.name),
      borderRadius: 4,
      stack: "devices",
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

  function timeline(data) {
    const sessions = data.sessions || [];
    document.getElementById("timeline-day").textContent = new Date(data.day + "T12:00:00").toLocaleDateString("de-DE", { weekday: "long", day: "2-digit", month: "2-digit" });
    if (!sessions.length) {
      document.getElementById("timeline-wrap").hidden = true;
      document.getElementById("timeline-empty").hidden = false;
      return;
    }
    const hours = (iso) => {
      const t = new Date(iso);
      return t.getHours() + t.getMinutes() / 60;
    };
    const devices = [...new Set(sessions.map((s) => s.device))];
    const datasets = devices.map((dev) => ({
      label: dev,
      data: sessions.filter((s) => s.device === dev).map((s) => ({ x: [hours(s.start), Math.max(hours(s.end), hours(s.start) + 0.05)], y: dev, ap: s.ap, start: s.start, end: s.end })),
      backgroundColor: colorFor(dev),
      borderRadius: 3,
      barPercentage: 0.6,
    }));
    const min = Math.max(0, Math.floor(Math.min(...sessions.map((s) => hours(s.start)))) - 1);
    const max = Math.min(24, Math.ceil(Math.max(...sessions.map((s) => hours(s.end)))) + 1);
    document.getElementById("timeline-wrap").style.height = Math.max(140, devices.length * 56 + 60) + "px";
    const clock = (iso) => new Date(iso).toLocaleTimeString("de-DE", { hour: "2-digit", minute: "2-digit" });
    new Chart(document.getElementById("chart-timeline"), {
      type: "bar",
      data: { labels: devices, datasets },
      options: {
        indexAxis: "y",
        maintainAspectRatio: false,
        parsing: { xAxisKey: "x", yAxisKey: "y" },
        scales: {
          x: { min, max, ticks: { stepSize: 1, callback: (h) => h + ":00" } },
          y: { stacked: true, grid: { display: false } },
        },
        plugins: {
          legend: { display: false },
          tooltip: {
            callbacks: {
              label: (c) => {
                const r = c.raw;
                const mins = (new Date(r.end) - new Date(r.start)) / 60000;
                return clock(r.start) + "–" + clock(r.end) + " (" + fmtMin(mins) + ")" + (r.ap ? " · " + r.ap : "");
              },
            },
          },
        },
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
