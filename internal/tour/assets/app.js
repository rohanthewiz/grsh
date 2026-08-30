// The tour's browser half.
//
// Two inputs, two surfaces. The SSE stream carries `out` (bytes the shell
// and the engine wrote, escape codes intact) and `state` (the lesson as
// data). The transcript renders the first; the sidebar renders the second.
// Neither ever reads the other — the sidebar in particular is drawn from
// the View, never scraped out of the text, so rewording a lesson cannot
// break the UI.

const $ = (id) => document.getElementById(id);

const transcript = $("transcript");
const lineInput = $("line");
const marker = $("marker");
const lane = $("lane");

let view = null;          // the last View we were given
let busy = false;         // a line is in flight; the input is disabled
const history = [];       // submitted lines, for arrow-key recall
let histPos = 0;

// ── ANSI ──────────────────────────────────────────────────────────
//
// The engine writes escape codes because its first host was a terminal,
// and translating them here rather than asking it for plain text is what
// keeps ONE renderer in the engine. Only SGR is understood; every other
// escape is dropped rather than shown, since a stray control sequence
// printed literally is worse than a missing effect.

const SGR = /\x1b\[([0-9;]*)m/g;
const OTHER_ESC = /\x1b\[[0-9;?]*[A-Za-z]|\x1b[()][A-Za-z0-9]|\x1b[=>]/g;

function escapeHTML(s) {
  return s.replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;");
}

// classesFor turns an SGR parameter list into the classes app.css defines.
// State is carried across calls by the caller, because a single write may
// open a colour that a later write closes.
function applySGR(params, state) {
  const codes = params === "" ? [0] : params.split(";").map((n) => parseInt(n || "0", 10));
  for (const c of codes) {
    if (c === 0) { state.bold = false; state.dim = false; state.fg = 0; }
    else if (c === 1) state.bold = true;
    else if (c === 2) state.dim = true;
    else if (c === 22) { state.bold = false; state.dim = false; }
    else if (c >= 30 && c <= 37) state.fg = c;
    else if (c === 39) state.fg = 0;
    else if (c >= 90 && c <= 97) state.fg = c - 60;
  }
}

function spanFor(state) {
  const cls = [];
  if (state.bold) cls.push("b");
  if (state.dim) cls.push("d");
  if (state.fg) cls.push("f" + state.fg);
  return cls;
}

const ansiState = { bold: false, dim: false, fg: 0 };

function ansiToHTML(text) {
  let out = "";
  let last = 0;
  SGR.lastIndex = 0;
  for (let m; (m = SGR.exec(text)) !== null; ) {
    out += wrap(text.slice(last, m.index), ansiState);
    applySGR(m[1], ansiState);
    last = SGR.lastIndex;
  }
  out += wrap(text.slice(last), ansiState);
  return out;
}

function wrap(chunk, state) {
  if (chunk === "") return "";
  const body = escapeHTML(chunk.replace(OTHER_ESC, ""));
  const cls = spanFor(state);
  return cls.length ? `<span class="${cls.join(" ")}">${body}</span>` : body;
}

function append(text) {
  // Sticking to the bottom only when the reader is already there: a
  // student scrolled up to re-read a hint should not be yanked back down
  // by a background job's notification.
  const atBottom = transcript.scrollHeight - transcript.scrollTop - transcript.clientHeight < 40;
  transcript.insertAdjacentHTML("beforeend", ansiToHTML(text));
  if (atBottom) transcript.scrollTop = transcript.scrollHeight;
}

// ── the stream ────────────────────────────────────────────────────

function connect() {
  const es = new EventSource("/events");
  es.addEventListener("out", (e) => {
    const msg = JSON.parse(e.data);
    if (msg.replay) {
      // A replay IS the transcript, not an addition to it: a reconnect
      // that appended would double everything already on screen.
      transcript.innerHTML = "";
      ansiState.bold = false; ansiState.dim = false; ansiState.fg = 0;
    }
    append(msg.text);
  });
  es.addEventListener("state", (e) => render(JSON.parse(e.data)));
  es.addEventListener("clear", () => { transcript.innerHTML = ""; });
  es.onerror = () => { /* EventSource reconnects on its own */ };
}

// ── the sidebar ───────────────────────────────────────────────────

// inline renders the two marks a lesson's prose may use. It is the HTML
// twin of the engine's style.inline: backticks become code, **stars**
// become bold, and an unpaired mark is left as written rather than
// swallowing the rest of the line.
function inline(text) {
  let out = "";
  let rest = escapeHTML(text);
  for (;;) {
    const tick = rest.indexOf("`");
    const star = rest.indexOf("**");
    if (tick < 0 && star < 0) return out + rest;
    if (star < 0 || (tick >= 0 && tick < star)) {
      const end = rest.indexOf("`", tick + 1);
      if (end < 0) return out + rest;
      out += rest.slice(0, tick) + "<code>" + rest.slice(tick + 1, end) + "</code>";
      rest = rest.slice(end + 1);
    } else {
      const end = rest.indexOf("**", star + 2);
      if (end < 0) return out + rest;
      out += rest.slice(0, star) + "<strong>" + rest.slice(star + 2, end) + "</strong>";
      rest = rest.slice(end + 2);
    }
  }
}

// prose joins the step's lines into paragraphs: a blank line in a lesson
// file is a paragraph break, and rendering each line as its own block
// would lose the shape the author wrote.
function renderProse(lines) {
  const paras = [];
  let cur = [];
  for (const line of lines || []) {
    if (line.trim() === "") { if (cur.length) { paras.push(cur); cur = []; } continue; }
    cur.push(line);
  }
  if (cur.length) paras.push(cur);
  return paras.map((p) => "<p>" + p.map(inline).join(" ") + "</p>").join("");
}

function renderTOC(v) {
  const toc = $("toc");
  toc.innerHTML = "";
  v.chapters.forEach((ch, i) => {
    const li = document.createElement("li");
    // "done" is what the student finished, not what they walked past: the
    // View says so per chapter, because jumping to 6 finishes nothing.
    li.className = (ch.done ? "done " : "") + (i === v.chapter ? "here" : "");
    li.innerHTML = `<span class="n">${i + 1}</span><span>${escapeHTML(ch.title)}</span>`;
    li.title = `chapter ${i + 1} — ${ch.steps} steps (starts in a fresh playground)`;
    li.onclick = () => send(`:menu ${i + 1}`);
    toc.appendChild(li);
  });
}

function render(v) {
  view = v;
  renderTOC(v);
  $("dir").textContent = v.dir || "";
  $("stepTitle").textContent = v.title || "";
  $("counter").textContent = v.steps ? `${Math.min(v.step, v.steps)}/${v.steps}` : "";
  $("prose").innerHTML = v.finished
    ? "<p>Chapter complete.</p>"
    : renderProse(v.prose);

  const tryBox = $("try");
  tryBox.hidden = !v.try;
  tryBox.textContent = v.try || "";

  const hints = $("hints");
  hints.innerHTML = "";
  for (const h of v.hints || []) {
    const el = document.createElement("div");
    el.className = "hint";
    el.innerHTML = inline(h);
    hints.appendChild(el);
  }
  if (v.solution) {
    const el = document.createElement("div");
    el.className = "hint answer";
    el.textContent = v.solution;
    hints.appendChild(el);
  }

  $("btnTry").hidden = !v.try;
  $("btnHint").disabled = !v.moreHints || v.finished;
  $("btnSol").disabled = !v.hasSolution || !!v.solution || v.finished;
  $("btnSkip").disabled = v.finished;
  $("btnNext").disabled = v.chapter + 1 >= v.chapters.length;
  $("btnKeep").hidden = !v.keep;
  $("btnKeep").textContent = v.keep ? `keep ${v.keep}` : "keep the file";

  marker.innerHTML = v.pending ? "&hellip;" : "&#9656;";
  lane.innerHTML = "";
  $("note").textContent = v.ended
    ? "The session has ended. `reset` starts the curriculum over in a clean playground."
    : "";
  lineInput.disabled = busy || v.ended;
  for (const b of document.querySelectorAll(".actions button")) {
    if (v.ended) b.disabled = true;
  }
}

// ── input ─────────────────────────────────────────────────────────

async function send(line) {
  if (busy || (view && view.ended)) return;
  busy = true;
  lineInput.disabled = true;
  try {
    const res = await fetch("/input", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ line }),
    });
    if (res.ok) {
      render(await res.json());
    } else if (res.status === 404) {
      // The session is gone: the server was restarted, or this tab sat
      // idle long enough to be reclaimed. Saying so matters — the failure
      // is otherwise indistinguishable from a shell that stopped
      // answering, and the page would just quietly ignore every keystroke.
      append("\x1b[33m[this session has ended — reload the page to start a new one]\x1b[0m\n");
    } else {
      append(`\x1b[33m[the tour server answered ${res.status}]\x1b[0m\n`);
    }
  } catch (e) {
    append("\x1b[33m[the tour server is not answering — is it still running?]\x1b[0m\n");
  } finally {
    busy = false;
    if (!view || !view.ended) {
      lineInput.disabled = false;
      lineInput.focus();
    }
  }
}

lineInput.addEventListener("keydown", (e) => {
  if (e.key === "Enter") {
    const line = lineInput.value;
    lineInput.value = "";
    if (line.trim() !== "") { history.push(line); }
    histPos = history.length;
    lane.innerHTML = "";
    send(line);
    return;
  }
  // Arrow-key recall over what has been submitted this session. Not the
  // shell's history — that lives in the session and a lesson's `session
  // save` depends on it holding only real units — just the page being a
  // civilised place to retype a long command.
  if (e.key === "ArrowUp" && histPos > 0) {
    histPos--;
    lineInput.value = history[histPos];
    e.preventDefault();
  } else if (e.key === "ArrowDown") {
    histPos = Math.min(histPos + 1, history.length);
    lineInput.value = histPos === history.length ? "" : history[histPos];
    e.preventDefault();
  }
});

// The classifier lane, debounced. Only the chapter that teaches
// classification asks for it; the server returns empty otherwise, so this
// stays a no-op everywhere else.
let laneTimer = null;
lineInput.addEventListener("input", () => {
  if (!view || !view.explain) return;
  clearTimeout(laneTimer);
  laneTimer = setTimeout(async () => {
    const src = lineInput.value;
    if (src.trim() === "") { lane.innerHTML = ""; return; }
    try {
      const res = await fetch("/classify?src=" + encodeURIComponent(src));
      const { verdict } = await res.json();
      lane.innerHTML = verdict ? `<span class="verdict">${escapeHTML(verdict)}</span>` : "";
    } catch (e) { /* the lane is a nicety; a failed fetch just leaves it blank */ }
  }, 120);
});

$("btnTry").onclick = () => { lineInput.value = view.try.split("\n")[0]; lineInput.focus(); };
$("btnHint").onclick = () => send(":hint");
$("btnSol").onclick = () => send(":sol");
$("btnSkip").onclick = () => send(":skip");
$("btnNext").onclick = () => send(":next");
$("btnKeep").onclick = () => send(":keep");

$("stop").onclick = () => fetch("/interrupt", { method: "POST" });
$("reset").onclick = async () => {
  if (!confirm("Throw this playground away and start the curriculum over?")) return;
  transcript.innerHTML = "";
  const res = await fetch("/reset", { method: "POST" });
  if (res.ok) render(await res.json());
};

// The page draws itself from /state before the stream arrives, so a slow
// SSE connection never shows an empty sidebar next to a full transcript.
//
// A page restored from the back/forward cache lands here with a session
// the server may no longer have — so a 404 reloads once rather than
// leaving a dead terminal on screen with no way to tell.
fetch("/state")
  .then((r) => {
    if (r.status === 404) { location.reload(); return null; }
    return r.json();
  })
  .then((v) => { if (v) render(v); })
  .catch(() => {});
connect();
lineInput.focus();
