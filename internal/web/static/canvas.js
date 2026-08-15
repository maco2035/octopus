// canvas.js — a small hand-rolled drag-and-drop DAG editor. PLAN.md called
// for a vendored SortableJS; this environment has no network access to pull
// third-party JS source into the repo verifiably, so this is a self-
// contained substitute covering the same interactions (drag a palette node
// onto the canvas, drag nodes to reposition, wire/delete edges, save) with
// zero dependencies and no build step.
(function () {
  "use strict";

  function initEditor(opts) {
    var canvas = document.getElementById("canvas");
    var dataTag = document.getElementById("pipeline-data");
    var initial = JSON.parse(dataTag.textContent || "{}");

    var state = {
      nodes: (initial.Nodes || []).map(function (n) {
        return { ID: n.ID, AgentType: n.AgentType, RequiresReview: !!n.RequiresReview, X: n.X || 0, Y: n.Y || 0 };
      }),
      edges: (initial.Edges || []).map(function (e) { return { From: e.From, To: e.To }; }),
    };

    var svg = document.createElementNS("http://www.w3.org/2000/svg", "svg");
    svg.setAttribute("class", "edges");
    canvas.appendChild(svg);

    var nodeCounter = 0;
    var wiringFrom = null; // node ID currently wiring an edge from, or null

    function nodeEl(id) {
      return canvas.querySelector('[data-node-id="' + cssEscape(id) + '"]');
    }

    function cssEscape(s) {
      return String(s).replace(/[^a-zA-Z0-9_-]/g, function (c) { return "\\" + c; });
    }

    function render() {
      canvas.querySelectorAll(".canvas-node").forEach(function (el) { el.remove(); });

      state.nodes.forEach(function (n) {
        var el = document.createElement("div");
        el.className = "canvas-node" + (n.RequiresReview ? " review" : "");
        el.dataset.nodeId = n.ID;
        el.style.left = n.X + "px";
        el.style.top = n.Y + "px";
        el.innerHTML =
          '<div class="node-delete" title="delete node">&times;</div>' +
          '<div class="node-type">' + escapeHTML(n.AgentType) + "</div>" +
          '<div class="node-id">' + escapeHTML(n.ID) + "</div>" +
          '<label><input type="checkbox" ' + (n.RequiresReview ? "checked" : "") + "> requires review</label>" +
          '<div class="node-port" title="drag to another node to wire an edge"></div>';

        el.querySelector(".node-delete").addEventListener("click", function (ev) {
          ev.stopPropagation();
          state.nodes = state.nodes.filter(function (x) { return x.ID !== n.ID; });
          state.edges = state.edges.filter(function (e) { return e.From !== n.ID && e.To !== n.ID; });
          render();
        });

        el.querySelector('input[type="checkbox"]').addEventListener("change", function (ev) {
          n.RequiresReview = ev.target.checked;
          el.classList.toggle("review", n.RequiresReview);
        });

        makeDraggable(el, n);

        el.querySelector(".node-port").addEventListener("mousedown", function (ev) {
          ev.stopPropagation();
          ev.preventDefault();
          wiringFrom = n.ID;
        });

        el.addEventListener("mouseup", function () {
          if (wiringFrom && wiringFrom !== n.ID) {
            var exists = state.edges.some(function (e) { return e.From === wiringFrom && e.To === n.ID; });
            if (!exists) state.edges.push({ From: wiringFrom, To: n.ID });
          }
          wiringFrom = null;
          renderEdges();
        });

        canvas.appendChild(el);
      });

      renderEdges();
    }

    function makeDraggable(el, n) {
      var dragging = false, offX = 0, offY = 0;
      el.addEventListener("mousedown", function (ev) {
        if (ev.target.classList.contains("node-port") || ev.target.classList.contains("node-delete")) return;
        dragging = true;
        offX = ev.clientX - n.X;
        offY = ev.clientY - n.Y;
        ev.preventDefault();
      });
      document.addEventListener("mousemove", function (ev) {
        if (!dragging) return;
        n.X = Math.round(Math.max(0, ev.clientX - offX));
        n.Y = Math.round(Math.max(0, ev.clientY - offY));
        el.style.left = n.X + "px";
        el.style.top = n.Y + "px";
        renderEdges();
      });
      document.addEventListener("mouseup", function () { dragging = false; });
    }

    function renderEdges() {
      while (svg.firstChild) svg.removeChild(svg.firstChild);
      state.edges.forEach(function (e, idx) {
        var fromEl = nodeEl(e.From), toEl = nodeEl(e.To);
        if (!fromEl || !toEl) return;
        var x1 = fromEl.offsetLeft + fromEl.offsetWidth, y1 = fromEl.offsetTop + fromEl.offsetHeight / 2;
        var x2 = toEl.offsetLeft, y2 = toEl.offsetTop + toEl.offsetHeight / 2;
        var mid = (x1 + x2) / 2;
        var path = document.createElementNS("http://www.w3.org/2000/svg", "path");
        path.setAttribute("d", "M" + x1 + "," + y1 + " C " + mid + "," + y1 + " " + mid + "," + y2 + " " + x2 + "," + y2);
        path.setAttribute("stroke", "currentColor");
        path.setAttribute("fill", "none");
        path.setAttribute("stroke-width", "2");
        path.style.pointerEvents = "stroke";
        path.addEventListener("click", function () {
          state.edges.splice(idx, 1);
          renderEdges();
        });
        svg.appendChild(path);
      });
    }

    document.querySelectorAll(".palette-item").forEach(function (item) {
      item.addEventListener("dragstart", function (ev) {
        ev.dataTransfer.setData("text/agent-type", item.dataset.agentType);
      });
    });

    canvas.addEventListener("dragover", function (ev) { ev.preventDefault(); });
    canvas.addEventListener("drop", function (ev) {
      ev.preventDefault();
      var agentType = ev.dataTransfer.getData("text/agent-type");
      if (!agentType) return;
      var rect = canvas.getBoundingClientRect();
      nodeCounter++;
      var id = agentType.replace(/[^a-zA-Z0-9]/g, "_") + "_" + nodeCounter;
      while (state.nodes.some(function (n) { return n.ID === id; })) {
        nodeCounter++;
        id = agentType.replace(/[^a-zA-Z0-9]/g, "_") + "_" + nodeCounter;
      }
      state.nodes.push({
        ID: id, AgentType: agentType, RequiresReview: false,
        X: Math.round(ev.clientX - rect.left + canvas.scrollLeft), Y: Math.round(ev.clientY - rect.top + canvas.scrollTop),
      });
      render();
    });

    document.getElementById("save-btn").addEventListener("click", function () {
      var statusEl = document.getElementById("save-status");
      var payload = {
        id: opts.defID || undefined,
        project_id: opts.projectID,
        name: document.getElementById("pipeline-name").value || "untitled",
        nodes: state.nodes,
        edges: state.edges,
      };
      statusEl.textContent = "Saving…";
      fetch(opts.saveURL, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(payload),
      })
        .then(function (res) {
          if (!res.ok) throw new Error("save failed: " + res.status);
          return res.json();
        })
        .then(function (saved) {
          statusEl.textContent = "Saved.";
          if (!opts.defID && saved.id) {
            window.location.href = "/projects/" + opts.projectID + "/pipelines/" + saved.id + "/edit";
          }
        })
        .catch(function (err) {
          statusEl.textContent = err.message;
        });
    });

    function escapeHTML(s) {
      var div = document.createElement("div");
      div.textContent = s;
      return div.innerHTML;
    }

    render();
  }

  window.Octopus = window.Octopus || {};
  window.Octopus.initEditor = initEditor;
})();
