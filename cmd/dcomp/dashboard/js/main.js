/* Shell: the fetch loop, system tabs, connectivity word, and the camera.
   The stage is redrawn only when the observed document changes. */

import { state } from "./state.js";
import { render, applyViewBox } from "./render.js";

async function fetchSystems() {
  try {
    const response = await fetch("/api/v2/systems");
    if (!response.ok) throw new Error(response.statusText);
    const body = await response.json();
    state.systems = body.systems || [];
    if (!state.current && state.systems.length) {
      state.current = state.systems[0];
    }
    renderTabs();
  } catch (error) {
    /* the view poll reports connectivity */
  }
}

async function fetchView() {
  if (!state.current) return;
  try {
    const response = await fetch(
      "/api/v2/view/" + encodeURIComponent(state.current));
    if (!response.ok) throw new Error(response.statusText);
    const text = await response.text();
    state.lost = false;
    state.lastSeen = Date.now();
    if (text !== state.rendered) {
      state.rendered = text;
      state.doc = JSON.parse(text);
      render();
    }
    renderObserving();
  } catch (error) {
    state.lost = true;
    renderObserving();
  }
}

function renderTabs() {
  const nav = document.getElementById("systems");
  nav.innerHTML = "";
  for (const name of state.systems) {
    const button = document.createElement("button");
    button.textContent = name;
    button.className = name === state.current ? "current" : "";
    button.onclick = () => {
      state.current = name;
      state.selection = null;
      state.viewBox = null;
      state.rendered = "";
      renderTabs();
      fetchView();
    };
    nav.appendChild(button);
  }
}

function renderObserving() {
  const element = document.getElementById("observing");
  if (state.lost) {
    const age = state.lastSeen ?
      Math.round((Date.now() - state.lastSeen) / 1000) + "S AGO" : "NEVER";
    element.textContent = "UNREACHABLE · LAST " + age;
    element.className = "observing lost";
  } else {
    element.textContent = "OBSERVING";
    element.className = "observing";
  }
  document.getElementById("stage").classList.toggle("lost", state.lost);
}

/* Pan and zoom: the camera moves, the world does not. */
function bindCamera() {
  const stage = document.getElementById("stage");
  let dragging = null;
  stage.addEventListener("mousedown", event => {
    dragging = { x: event.clientX, y: event.clientY };
    stage.classList.add("panning");
  });
  window.addEventListener("mouseup", () => {
    dragging = null;
    stage.classList.remove("panning");
  });
  window.addEventListener("mousemove", event => {
    if (!dragging || !state.viewBox) return;
    if (Math.abs(event.clientX - dragging.x) +
        Math.abs(event.clientY - dragging.y) > 3) {
      state.dragged = true;
    }
    const scale = state.viewBox[2] / stage.clientWidth;
    state.viewBox[0] -= (event.clientX - dragging.x) * scale;
    state.viewBox[1] -= (event.clientY - dragging.y) * scale;
    dragging = { x: event.clientX, y: event.clientY };
    applyViewBox();
  });
  stage.addEventListener("wheel", event => {
    if (!state.viewBox) return;
    event.preventDefault();
    const factor = event.deltaY > 0 ? 1.12 : 1 / 1.12;
    const box = state.viewBox;
    const rect = stage.getBoundingClientRect();
    const px = box[0] + (event.clientX - rect.left) / rect.width * box[2];
    const py = box[1] + (event.clientY - rect.top) / rect.height * box[3];
    box[0] = px - (px - box[0]) * factor;
    box[1] = py - (py - box[1]) * factor;
    box[2] *= factor;
    box[3] *= factor;
    applyViewBox();
  }, { passive: false });
  window.addEventListener("keydown", event => {
    if (event.key === "Escape" && state.doc) {
      state.selection = null;
      render();
    }
  });
}

bindCamera();
fetchSystems();
fetchView();
setInterval(fetchSystems, 10000);
setInterval(fetchView, 1500);
