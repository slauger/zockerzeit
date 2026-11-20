(function () {
  "use strict";

  // Mirrors rules.ParseWindows closely enough for a live preview; the server
  // validates the real input.
  function parse(value) {
    const out = [];
    for (const part of value.split(",")) {
      const m = part.trim().match(/^(\d{1,2}):(\d{2})\s*-\s*(\d{1,2}):(\d{2})$/);
      if (!m) {
        if (part.trim() !== "") return null;
        continue;
      }
      const start = +m[1] * 60 + +m[2];
      const end = +m[3] * 60 + +m[4];
      if (end <= start || end > 1440) return null;
      out.push([start, end]);
    }
    return out;
  }

  function render(input) {
    const track = input.parentElement.querySelector(".track");
    const windows = parse(input.value);
    input.classList.toggle("invalid", windows === null);
    if (windows === null) return;
    track.replaceChildren(
      ...windows.map(([start, end]) => {
        const s = document.createElement("span");
        s.className = "slot";
        s.style.left = (start / 1440) * 100 + "%";
        s.style.width = ((end - start) / 1440) * 100 + "%";
        return s;
      })
    );
  }

  document.querySelectorAll("input.win").forEach((input) => {
    input.addEventListener("input", () => render(input));
  });

  document.querySelectorAll(".copy-weekdays").forEach((button) => {
    button.addEventListener("click", () => {
      const set = button.closest(".ruleset");
      const monday = set.querySelector('input.win[data-day="1"]');
      ["2", "3", "4", "5"].forEach((day) => {
        const input = set.querySelector('input.win[data-day="' + day + '"]');
        input.value = monday.value;
        render(input);
      });
    });
  });
})();
