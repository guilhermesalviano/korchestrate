"use strict";
const $ = (id) => document.getElementById(id);
let token = new URLSearchParams(location.hash.slice(1)).get("token") || "";
try {
  token = token || sessionStorage.getItem("kor-token") || "";
} catch (_) {}
if (location.hash) history.replaceState(null, "", location.pathname);
let selected = "",
  current = null,
  tab = "logs",
  active = false,
  connected = false;
let gateKey = "",
  listKey = "",
  polling = false,
  sending = false,
  chooseActive = true,
  connectionFailed = false;
let defaultPrompts = {},
  promptRun = "";
let worktreesAt = 0,
  worktreesLoading = false;
const promptStages = {
  planner: "Plan",
  executor: "Execute",
  reviewer: "Review",
};
const labels = {
  logs: "Activity",
  plan: "Plan",
  review: "Review",
  diff: "Diff",
};

function message(text = "") {
  $("error").textContent = text;
  $("error").hidden = !text;
}
function connection(text, ok = false) {
  $("connection").textContent = text;
  $("connection").classList.toggle("connected", ok);
}
async function api(path, body) {
  const response = await fetch("/api" + path, {
    method: body === undefined ? "GET" : "POST",
    headers: {
      Authorization: "Bearer " + token,
      ...(body === undefined ? {} : { "Content-Type": "application/json" }),
    },
    body: body === undefined ? undefined : JSON.stringify(body),
    cache: "no-store",
    signal: AbortSignal.timeout(15000),
  });
  const data = await response.json();
  if (response.status === 401) {
    connected = false;
    $("login").hidden = false;
    $("app").hidden = true;
    connection("Access required");
  }
  if (!response.ok)
    throw new Error(data.error || "Request failed (" + response.status + ")");
  return data;
}
function element(tag, text, className) {
  const el = document.createElement(tag);
  el.textContent = text;
  if (className) el.className = className;
  return el;
}
function setText(id, text) {
  if ($(id).textContent !== text) $(id).textContent = text;
}

function promptEditor(prefix, prompts, readOnly = false) {
  const container = $(prefix + "-prompts");
  container.replaceChildren(
    ...Object.entries(promptStages).map(([stage, title]) => {
      const section = document.createElement("details");
      section.className = "prompt-stage";
      const summary = element("summary", title + " · " + stage);
      const id = prefix + "-prompt-" + stage;
      const label = element("label", title + " base prompt");
      label.htmlFor = id;
      const input = document.createElement("textarea");
      input.id = id;
      input.rows = 12;
      input.maxLength = 16384;
      input.spellcheck = false;
      input.value = prompts[stage] || "";
      input.readOnly = readOnly;
      const reset = element(
        "button",
        "Reset " + title.toLowerCase() + " to built-in prompt",
      );
      reset.type = "button";
      reset.hidden = readOnly;
      reset.addEventListener("click", () => {
        input.value = defaultPrompts[stage];
      });
      section.append(summary, label, input, reset);
      return section;
    }),
  );
}

function readPrompts(prefix) {
  return Object.fromEntries(
    Object.keys(promptStages).map((stage) => [
      stage,
      $(prefix + "-prompt-" + stage).value,
    ]),
  );
}

async function connect() {
  try {
    const config = await api("/config");
    try {
      sessionStorage.setItem("kor-token", token);
    } catch (_) {}
    $("token").value = "";
    connected = true;
    chooseActive = true;
    selected = "";
    current = null;
    $("login").hidden = true;
    $("app").hidden = false;
    $("compose").hidden = false;
    $("detail").hidden = true;
    $("repo").textContent = config.repo;
    defaultPrompts = config.default_prompts;
    promptEditor("new", config.prompts);
    promptRun = "";
    $("models").replaceChildren(
      ...Object.entries(config.models).map(([stage, model]) => {
        const box = document.createElement("div");
        box.append(element("small", stage), element("p", model));
        return box;
      }),
    );
    message();
    connection("Connected to host", true);
    await refresh();
  } catch (err) {
    message(err.message);
    connection("Unable to connect");
    $("login").hidden = false;
  }
}

function renderList(runs) {
  active = runs.some((run) => run.active);
  $("start").disabled = active || sending;
  $("resume").disabled = active || sending;
  $("busy-note").textContent = active
    ? "Finish or stop the active run to start another."
    : "You approve each step in default mode.";
  $("run-count").textContent = runs.length;
  const key =
    JSON.stringify(
      runs.map((run) => [
        run.id,
        run.prompt,
        run.status,
        run.active,
        run.gate?.id,
      ]),
    ) + selected;
  if (key === listKey) return;
  listKey = key;
  $("runs").replaceChildren(
    ...runs.map((run) => {
      const button = element(
        "button",
        "",
        "run-item" +
          (run.id === selected ? " selected" : "") +
          (run.gate ? " waiting" : ""),
      );
      button.setAttribute(
        "aria-current",
        run.id === selected ? "true" : "false",
      );
      button.append(
        element("strong", run.prompt),
        element(
          "small",
          run.gate
            ? "● Needs your input"
            : (run.active ? "● " : "") + run.status,
        ),
      );
      button.addEventListener("click", () => select(run.id));
      return button;
    }),
  );
  if (!runs.length)
    $("runs").append(element("p", "Your runs will appear here.", "muted"));
}

function worktreeName(path) {
  return path.split(/[\\/]/).filter(Boolean).pop() || path;
}

// Worktrees run git status, so they refresh less often than runs.
async function refreshWorktrees(force = false) {
  if (!connected || worktreesLoading) return;
  if (!force && Date.now() - worktreesAt < 10000) return;
  worktreesLoading = true;
  try {
    renderWorktrees(await api("/worktrees"));
    worktreesAt = Date.now();
  } catch (_) {
    // The run list reports connection problems.
  } finally {
    worktreesLoading = false;
  }
}

function renderWorktrees(list) {
  $("worktree-count").textContent = list.length;
  $("worktrees").replaceChildren(
    ...list.map((wt) => {
      const button = element("button", "", "run-item worktree-item");
      button.title = wt.path;
      const branch = wt.branch || "detached " + (wt.head || "").slice(0, 7);
      const details = [
        wt.main ? "current checkout" : worktreeName(wt.path),
        wt.prunable
          ? "missing directory"
          : wt.changes
            ? wt.changes + " changed file" + (wt.changes === 1 ? "" : "s")
            : "clean",
      ];
      if (wt.run_status) details.push("run " + wt.run_status);
      button.append(
        element("strong", branch),
        element("small", details.join(" · ")),
      );
      button.addEventListener("click", () => {
        if (wt.run_id) {
          select(wt.run_id);
          return;
        }
        // Starting a run on this branch reuses its worktree.
        selected = "";
        current = null;
        listKey = "";
        $("detail").hidden = true;
        $("compose").hidden = false;
        $("branch").value = wt.main ? "" : wt.branch || "";
        $("prompt").focus();
      });
      return button;
    }),
  );
}

async function select(id) {
  selected = id;
  current = null;
  gateKey = "";
  listKey = "";
  $("compose").hidden = true;
  $("detail").hidden = true;
  message();
  try {
    await refreshDetail();
  } catch (err) {
    message(err.message);
  }
}
async function refreshDetail() {
  if (!selected) return;
  const id = selected;
  const run = await api("/runs/" + encodeURIComponent(id));
  if (selected !== id) return;
  current = run;
  renderDetail(run);
}
function renderDetail(run) {
  $("detail").hidden = false;
  setText("run-title", run.prompt);
  setText("run-status", run.gate ? "Waiting for your decision" : run.status);
  $("cancel").hidden = !run.active;
  $("cancel").disabled = sending || run.status === "stopping";
  const meta = run.run;
  setText(
    "run-meta",
    meta
      ? [
          meta.branch,
          meta.autopilot ? "AUTOPILOT" : "DEFAULT",
          meta.id,
          meta.commit ? "Commit " + meta.commit.slice(0, 12) : "",
          meta.pushed ? "Pushed" : "",
        ]
          .filter(Boolean)
          .join(" · ")
      : "Preparing your workspace…",
  );
  $("run-error").hidden = !run.error;
  setText("run-error", run.error || "");
  const stage =
    run.gate?.kind === "plan"
      ? "planner"
      : run.gate?.kind === "review"
        ? "reviewer"
        : run.status.split(":")[0];
  document
    .querySelectorAll("[data-stage]")
    .forEach((el) =>
      el.classList.toggle("current", run.active && el.dataset.stage === stage),
    );
  $("gate").hidden = !run.gate;
  const nextKey = run.id + ":" + (run.gate?.id || "");
  if (nextKey !== gateKey) {
    gateKey = nextKey;
    $("gate-actions").replaceChildren();
    if (run.gate) {
      setText("gate-title", run.gate.title);
      setText("gate-body", run.gate.body);
      for (const [index, choice] of run.gate.choices.entries()) {
        const button = element(
          "button",
          choice.label,
          index === 0 ? "primary" : "",
        );
        button.disabled = sending;
        button.addEventListener("click", () =>
          action(() =>
            api("/runs/" + encodeURIComponent(run.id) + "/answer", {
              gate_id: run.gate.id,
              value: choice.value,
            }),
          ),
        );
        $("gate-actions").append(button);
      }
    }
  }
  $("resume-form").hidden = run.active || !meta;
  $("resume").disabled = active || sending;
  $("run-prompt-panel").hidden = !run.prompts;
  if (run.prompts) {
    if (promptRun !== run.id) {
      promptEditor("run", run.prompts, run.active);
      promptRun = run.id;
    }
    $("run-prompts")
      .querySelectorAll("textarea")
      .forEach((input) => {
        input.readOnly = run.active;
      });
    $("run-prompts")
      .querySelectorAll("button")
      .forEach((button) => {
        button.hidden = run.active;
      });
    setText(
      "run-prompts-note",
      run.active
        ? "These are the base instructions for this run. Stop the run to edit them, then resume from the step you want to rerun."
        : "Edit these instructions before resuming. Changes apply from the step you select below; earlier steps are kept.",
    );
  }
  setText(
    "output-note",
    run.active
      ? "Updates automatically · latest 300 lines"
      : "Run retained on your computer",
  );
  renderOutput();
}
function renderOutput() {
  if (!current) return;
  const text = tab === "logs" ? (current.logs || []).join("\n") : current[tab];
  const output = $("output");
  const next =
    text ||
    (tab === "logs" && !current.active
      ? "Open Plan, Review, or Diff to inspect saved results. Full agent logs are in the run artifacts on your computer."
      : "Waiting for " + labels[tab].toLowerCase() + "…");
  output.classList.toggle("diff", tab === "diff");
  if (output.textContent !== next) {
    output.textContent = next;
    if (tab === "logs" && $("follow").checked)
      output.scrollTop = output.scrollHeight;
  }
}
async function refresh() {
  if (!connected || polling) return;
  polling = true;
  try {
    const runs = await api("/runs");
    renderList(runs);
    refreshWorktrees();
    if (chooseActive) {
      chooseActive = false;
      if (!selected && runs.some((run) => run.active)) {
        selected = runs.find((run) => run.active).id;
        $("compose").hidden = true;
      }
    }
    await refreshDetail();
    if (connectionFailed) message();
    connectionFailed = false;
    connection("Connected to host", true);
  } catch (err) {
    connectionFailed = true;
    connection("Connection lost · retrying");
    message(err.message);
  } finally {
    polling = false;
  }
}
async function action(fn) {
  if (sending) return;
  sending = true;
  message();
  document
    .querySelectorAll("#gate-actions button, #start, #resume, #cancel")
    .forEach((button) => (button.disabled = true));
  try {
    await fn();
  } catch (err) {
    message(err.message);
  } finally {
    sending = false;
    gateKey = "";
    worktreesAt = 0;
    await refresh();
  }
}
$("login-form").addEventListener("submit", (event) => {
  event.preventDefault();
  token = $("token").value.trim();
  connect();
});
$("new-run").addEventListener("click", () => {
  selected = "";
  current = null;
  listKey = "";
  $("detail").hidden = true;
  $("compose").hidden = false;
  $("prompt").focus();
});
$("start-form").addEventListener("submit", (event) => {
  event.preventDefault();
  action(async () => {
    const result = await api("/runs", {
      prompt: $("prompt").value,
      name: $("branch").value,
      autopilot: $("autopilot").checked,
      prompts: readPrompts("new"),
    });
    $("prompt").value = "";
    await select(result.id);
  });
});
$("cancel").addEventListener("click", () => {
  if (
    current &&
    confirm("Stop this run? Its checkout and edits will be retained.")
  ) {
    const id = current.id;
    action(() => api("/runs/" + encodeURIComponent(id) + "/cancel", {}));
  }
});
$("resume-form").addEventListener("submit", (event) => {
  event.preventDefault();
  if (!current?.run) return;
  const id = current.run.id;
  action(async () => {
    const result = await api("/runs", {
      run_id: id,
      from: $("resume-stage").value,
      prompts: readPrompts("run"),
    });
    await select(result.id);
  });
});
$("tabs").addEventListener("click", (event) => {
  const button = event.target.closest("[data-tab]");
  if (!button) return;
  tab = button.dataset.tab;
  document
    .querySelectorAll("[data-tab]")
    .forEach((el) => el.setAttribute("aria-pressed", String(el === button)));
  renderOutput();
});
document.addEventListener("visibilitychange", () => {
  if (!document.hidden) refresh();
});
window.addEventListener("hashchange", () => {
  const accessToken = new URLSearchParams(location.hash.slice(1)).get("token");
  if (!accessToken) return;
  token = accessToken;
  history.replaceState(null, "", location.pathname);
  connect();
});
setInterval(() => {
  if (!document.hidden) refresh();
}, 1200);
if (token) connect();
else {
  $("login").hidden = false;
  connection("Access required");
}
