(function () {
  "use strict";

  // Suggests installing Zockerzeit as an app on phones, until it runs as
  // one or the hint was closed.
  const key = "zockerzeit-install-hint";
  const standalone = window.matchMedia("(display-mode: standalone)").matches || navigator.standalone === true;
  let closed = false;
  try {
    closed = localStorage.getItem(key) === "closed";
  } catch (e) {
    // Storage blocked: show the hint anyway.
  }
  if (standalone || closed) return;

  const ios = /iPhone|iPad|iPod/.test(navigator.userAgent) || (navigator.platform === "MacIntel" && navigator.maxTouchPoints > 1);
  const main = document.querySelector("main");
  if (!main) return;

  function show(text, action) {
    const box = document.createElement("div");
    box.className = "install-hint";
    box.setAttribute("role", "note");
    const p = document.createElement("p");
    p.innerHTML = text;
    box.append(p);
    if (action) box.append(action);
    const close = document.createElement("button");
    close.type = "button";
    close.className = "link";
    close.textContent = "Schließen";
    close.addEventListener("click", () => {
      box.remove();
      try {
        localStorage.setItem(key, "closed");
      } catch (e) {
        // Not remembered, shown again next time.
      }
    });
    box.append(close);
    main.prepend(box);
  }

  if (ios) {
    show("<strong>Als App installieren:</strong> unten auf <em>Teilen</em> tippen und <em>Zum Home-Bildschirm</em> wählen.");
    return;
  }
  window.addEventListener("beforeinstallprompt", (event) => {
    event.preventDefault();
    const button = document.createElement("button");
    button.type = "button";
    button.className = "primary";
    button.textContent = "Installieren";
    button.addEventListener("click", () => {
      event.prompt();
      button.closest(".install-hint").remove();
    });
    show("<strong>Zockerzeit als App:</strong> mit eigenem Symbol auf dem Startbildschirm, ohne Browserleiste.", button);
  });
})();
