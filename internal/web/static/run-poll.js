// run-poll.js — polls a run's status fragment and swaps it in, so the
// status/review/output view stays live without a manual refresh. PLAN.md
// called for htmx SSE/polling; this is a minimal hand-rolled equivalent
// (same reasoning as canvas.js: no network access to vendor htmx here).
(function () {
  "use strict";

  var TERMINAL = { COMPLETED: 1, FAILED: 1, REJECTED: 1, CANCELLED: 1 };

  function pollRun(runID, fragmentURL) {
    var timer = null;

    function tick() {
      fetch(fragmentURL)
        .then(function (res) {
          if (!res.ok) throw new Error("poll failed: " + res.status);
          return res.text();
        })
        .then(function (html) {
          var container = document.getElementById("run-fragment");
          if (!container) return;
          var parent = container.parentNode;
          var wrapper = document.createElement("div");
          wrapper.innerHTML = html;
          var next = wrapper.firstElementChild;
          parent.replaceChild(next, container);
          var status = next.dataset.status;
          if (TERMINAL[status]) {
            clearInterval(timer);
          }
        })
        .catch(function () { /* transient network hiccup — next tick retries */ });
    }

    timer = setInterval(tick, 2000);
  }

  window.Octopus = window.Octopus || {};
  window.Octopus.pollRun = pollRun;
})();
