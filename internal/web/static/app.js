"use strict";
const $ = (id) => document.getElementById(id);
// Old access links carried a token in the hash; it is no longer used.
if (location.hash) history.replaceState(null, "", location.pathname);
try {
  sessionStorage.removeItem("kor-token");
} catch (_) {}
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
let catalog = null,
  catalogLoading = false,
  modelRun = "";
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
    headers: body === undefined ? {} : { "Content-Type": "application/json" },
    body: body === undefined ? undefined : JSON.stringify(body),
    cache: "no-store",
    signal: AbortSignal.timeout(15000),
  });
  const data = await response.json();
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

// The run list is a drawer below 800px; on wider screens the class has no
// effect and the sidebar stays in the layout.
const sidebarToggle = $("sidebar-toggle");
function setSidebar(open) {
  document.body.classList.toggle("sidebar-open", open);
  sidebarToggle.setAttribute("aria-expanded", String(open));
  sidebarToggle.setAttribute(
    "aria-label",
    open ? "Hide run list" : "Show run list",
  );
  $("scrim").hidden = !open;
}
sidebarToggle.addEventListener("click", () =>
  setSidebar(!document.body.classList.contains("sidebar-open")),
);
$("scrim").addEventListener("click", () => setSidebar(false));
document.addEventListener("keydown", (event) => {
  if (event.key === "Escape") setSidebar(false);
});

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

// catalog lists installed providers with their models and efforts; until it
// loads, pickers offer only the configured choices.
function catalogAgent(agent) {
  return catalog?.agents.find((info) => info.agent === agent);
}
function catalogModel(agent, model) {
  return catalogAgent(agent)?.models.find((info) => info.id === model);
}
function fillSelect(select, values, value, labels = {}) {
  // A configured value missing from the catalog stays selectable.
  if (value && !values.includes(value)) values = [value, ...values];
  select.replaceChildren(
    ...values.map((v) => {
      const option = element("option", labels[v] ?? v);
      option.value = v;
      return option;
    }),
  );
  select.value = value;
}

function modelPicker(prefix, choices, readOnly = false) {
  $(prefix + "-models").replaceChildren(
    ...Object.entries(promptStages).map(([stage, title]) => {
      const choice = choices[stage] || {};
      const box = document.createElement("div");
      const selects = {};
      for (const [field, label] of [
        ["agent", "provider"],
        ["model", "model"],
        ["variant", "effort"],
      ]) {
        const select = document.createElement("select");
        select.id = prefix + "-model-" + stage + "-" + field;
        select.disabled = readOnly;
        select.setAttribute("aria-label", title + " " + label);
        selects[field] = select;
      }
      const fillEfforts = (variant) => {
        const efforts =
          catalogModel(selects.agent.value, selects.model.value)?.efforts || [];
        fillSelect(selects.variant, ["", ...efforts], variant, {
          "": "default effort",
        });
        selects.variant.hidden = efforts.length === 0 && !variant;
      };
      const fillModels = (model, variant) => {
        const ids =
          catalogAgent(selects.agent.value)?.models.map((info) => info.id) ||
          [];
        fillSelect(selects.model, ids, model || ids[0] || "");
        // A newly picked model starts at its default effort.
        fillEfforts(
          variant ??
            (catalogModel(selects.agent.value, selects.model.value)
              ?.default_effort ||
              ""),
        );
      };
      fillSelect(
        selects.agent,
        catalog ? catalog.agents.map((info) => info.agent) : [],
        choice.agent || "",
      );
      fillModels(choice.model, choice.variant || "");
      selects.agent.addEventListener("change", () => fillModels("", null));
      selects.model.addEventListener("change", () =>
        fillEfforts(
          catalogModel(selects.agent.value, selects.model.value)
            ?.default_effort || "",
        ),
      );
      box.append(
        element("small", title + " · " + stage),
        selects.agent,
        selects.model,
        selects.variant,
      );
      return box;
    }),
  );
}

function readModels(prefix) {
  return Object.fromEntries(
    Object.keys(promptStages).map((stage) => [
      stage,
      Object.fromEntries(
        ["agent", "model", "variant"].map((field) => [
          field,
          $(prefix + "-model-" + stage + "-" + field).value,
        ]),
      ),
    ]),
  );
}

// Discovery probes the agent CLIs, so it loads once after connecting and
// refreshes the pickers in place, keeping what the user already chose.
async function loadCatalog() {
  if (catalog || catalogLoading) return;
  catalogLoading = true;
  try {
    const loaded = await api("/models");
    const newChoices = readModels("new");
    catalog = loaded;
    modelPicker("new", newChoices);
    if (current?.models && modelRun) {
      modelPicker("run", readModels("run"), current.active);
    }
  } catch (err) {
    message("Could not list models: " + err.message);
  } finally {
    catalogLoading = false;
  }
}

async function connect() {
  try {
    const config = await api("/config");
    connected = true;
    chooseActive = true;
    selected = "";
    current = null;
    $("app").hidden = false;
    $("compose").hidden = false;
    $("detail").hidden = true;
    $("repo").textContent = config.repo;
    defaultPrompts = config.default_prompts;
    promptEditor("new", config.prompts);
    promptRun = "";
    modelPicker("new", config.models);
    modelRun = "";
    loadCatalog();
    message();
    connection("Connected to host", true);
    await refresh();
  } catch (err) {
    message(err.message);
    connection("Unable to connect · retrying");
    setTimeout(connect, 3000);
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
  $("toggle-count").textContent = runs.length;
  sidebarToggle.classList.toggle(
    "attention",
    runs.some((run) => run.gate),
  );
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
        setSidebar(false);
        window.scrollTo(0, 0);
        $("detail").hidden = true;
        $("compose").hidden = false;
        $("branch").value = wt.main ? "" : wt.branch || "";
        $("prompt").focus();
      });
      return button;
    }),
  );
}

// Pipeline states map to the step being worked on; 3 means every step is done.
const stateStep = {
  preflight: 0,
  worktree: 0,
  planning: 0,
  gate_plan: 0,
  executing: 1,
  reviewing: 2,
  gate_review: 2,
  publishing: 3,
  committing: 3,
  done: 3,
};
const stepOrder = ["planner", "executor", "reviewer"];
const stepNotes = {
  planner: {
    active: "Planning…",
    waiting: "Needs your approval",
    done: "Plan approved",
  },
  executor: {
    active: "Implementing…",
    waiting: "Needs you",
    done: "Changes made",
  },
  reviewer: {
    active: "Reviewing…",
    waiting: "Needs your decision",
    done: "Review passed",
  },
};

// stepStates returns each step's state: pending, active, waiting, done,
// failed or stopped.
function stepStates(run) {
  const state = run.run?.state || (run.active ? "preflight" : "");
  let at = stateStep[state] ?? -1;
  let mode = run.gate ? "waiting" : run.active ? "active" : "stopped";
  if (state === "failed" || state === "aborted") {
    // The saved artifacts show how far the run got.
    at = run.review ? 2 : run.plan ? 1 : 0;
    if (run.diff && !run.review) at = 2;
    mode = state === "failed" ? "failed" : "stopped";
  }
  return stepOrder.map((_, i) =>
    i < at ? "done" : i === at ? mode : "pending",
  );
}

function stepNote(run, stage, state) {
  const iteration = run.run?.iteration || 0;
  if (state === "active" && run.run?.state === "preflight")
    return "Preparing workspace…";
  if (state === "active" && iteration > 0) {
    if (stage === "executor") return "Fix pass " + iteration + "…";
    if (stage === "reviewer") return "Review " + (iteration + 1) + "…";
  }
  if (state === "failed") return "Failed";
  if (state === "stopped") return "Stopped here";
  if (state === "pending") return "Up next";
  return stepNotes[stage][state] || "";
}

function renderStages(run) {
  const states = stepStates(run);
  const list = document.querySelector(".pipeline");
  list.classList.toggle(
    "complete",
    states.every((state) => state === "done"),
  );
  stepOrder.forEach((stage, i) => {
    const el = list.querySelector('[data-stage="' + stage + '"]');
    // Setting the same state again must not restart its entry animation.
    if (el.dataset.state !== states[i]) el.dataset.state = states[i];
    const note = el.querySelector(".stage-note");
    const text = stepNote(run, stage, states[i]);
    if (note.textContent !== text) note.textContent = text;
    el.setAttribute(
      "aria-current",
      states[i] === "active" || states[i] === "waiting" ? "step" : "false",
    );
  });
}

async function select(id) {
  selected = id;
  current = null;
  gateKey = "";
  listKey = "";
  setSidebar(false);
  window.scrollTo(0, 0);
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
  renderStages(run);
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
  $("run-model-panel").hidden = !run.models;
  if (run.models) {
    if (modelRun !== run.id) {
      modelPicker("run", run.models, run.active);
      modelRun = run.id;
    }
    $("run-models")
      .querySelectorAll("select")
      .forEach((select) => {
        select.disabled = run.active;
      });
    setText(
      "run-models-note",
      run.active
        ? "Stop the run to change its models, then resume from the step you want to rerun."
        : "Change models before resuming. They apply from the step you select below.",
    );
  }
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
$("new-run").addEventListener("click", () => {
  selected = "";
  current = null;
  listKey = "";
  setSidebar(false);
  window.scrollTo(0, 0);
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
      models: readModels("new"),
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
      models: readModels("run"),
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
setInterval(() => {
  if (!document.hidden) refresh();
}, 1200);
connect();
