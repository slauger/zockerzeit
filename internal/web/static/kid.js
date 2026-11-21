(function () {
  "use strict";

  const root = document.querySelector("[data-kid]");
  if (!root) return;

  if ("serviceWorker" in navigator) {
    navigator.serviceWorker.register("/kind/sw.js", { scope: "/kind/" }).catch(() => {});
  }

  const stamp = document.getElementById("kid-stamp");
  const clock = () => new Date().toLocaleTimeString("de-DE", { hour: "2-digit", minute: "2-digit" });

  // Re-render by fetching the page itself, so the server templates stay the
  // single source of truth for the layout.
  async function refresh() {
    try {
      const res = await fetch(location.pathname + location.search, { cache: "no-store" });
      if (!res.ok) throw new Error(res.status);
      const doc = new DOMParser().parseFromString(await res.text(), "text/html");
      const next = doc.querySelector("[data-kid]");
      if (next) root.replaceChildren(...next.childNodes);
      document.documentElement.classList.remove("is-offline");
      if (stamp) stamp.textContent = "Stand " + clock() + " Uhr";
    } catch (e) {
      document.documentElement.classList.add("is-offline");
      if (stamp) stamp.textContent = "Keine Verbindung, Stand von vorhin";
    }
  }

  setInterval(refresh, 30000);
  document.addEventListener("visibilitychange", () => {
    if (document.visibilityState === "visible") refresh();
  });
})();
