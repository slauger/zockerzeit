(function () {
  "use strict";

  const minutes = (v) => {
    const m = v.trim().match(/^(\d{1,2}):(\d{2})$/);
    return m ? +m[1] * 60 + +m[2] : null;
  };

  // Mirrors the plan form closely enough for a live preview; the server
  // validates the real input.
  function render(row) {
    const from = row.querySelector(".from").value;
    const to = row.querySelector(".to").value;
    const limit = row.querySelector(".limit").value.trim();
    const track = row.querySelector(".track");
    let start = 0;
    let end = 1440;
    if (from !== "" || to !== "") {
      start = minutes(from);
      end = minutes(to);
    }
    const valid = start !== null && end !== null && end > start;
    row.querySelector(".from").classList.toggle("invalid", !valid);
    row.querySelector(".to").classList.toggle("invalid", !valid);
    track.replaceChildren();
    if (!valid || limit === "0") return;
    const s = document.createElement("span");
    s.className = "slot";
    s.style.left = (start / 1440) * 100 + "%";
    s.style.width = ((end - start) / 1440) * 100 + "%";
    track.append(s);
  }

  document.querySelectorAll(".plan-row[data-day]").forEach((row) => {
    row.querySelectorAll("input").forEach((input) => input.addEventListener("input", () => render(row)));
  });

  document.querySelectorAll(".copy-weekdays").forEach((button) => {
    button.addEventListener("click", () => {
      const set = button.closest(".plan-days");
      const monday = set.querySelector('.plan-row[data-day="1"]');
      ["2", "3", "4", "5"].forEach((day) => {
        const row = set.querySelector('.plan-row[data-day="' + day + '"]');
        ["from", "to", "limit"].forEach((c) => (row.querySelector("." + c).value = monday.querySelector("." + c).value));
        render(row);
      });
    });
  });
})();
