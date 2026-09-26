# korchestrate

**kor** is a terminal orchestrator for AI coding agents. It takes a prompt and runs a
plan → execute → review pipeline, each stage powered by the agent of your choice,
in your current checkout or an isolated git worktree.

- **Plan** with [Claude](https://claude.com)
- **Execute** with [Codex](https://openai.com/codex)
- **Review** with [OpenCode](https://opencode.ai)

Leave the run name blank to work directly on your current branch. Enter a new
branch name to work in an isolated worktree instead.

## Install

Requires Go 1.22+ and the agent CLIs you want to use (`claude`, `codex`, `opencode`,
and optionally `agy` for Antigravity).

```sh
./install.sh
```

## Quick start

```sh
# Edit config.json to set the default agents and models per stage.
kor run "add pagination to the users list"
```

kor plans the change, edits your current checkout, and has a reviewer agent check
the diff. After review, choose to commit, commit and push, or leave changes staged.

Use `kor run --name my-feature "your prompt"` for an isolated branch and worktree.
Using the current branch's name also works directly in its existing checkout.
Failed or cancelled runs preserve that checkout and any edits; cleaning an
in-place run removes only its run history. A detached HEAD requires a branch name.

The dashboard starts with the left sidebar closed. Press `b` to toggle the run
list. On narrow terminals it opens at full width: use `↑↓` to select a run and
`enter` to open it, or `b`/`esc` to close the list. Use `tab` or `1`–`5` for
Activity, Plan, Review, Diff, and Support; `n` starts a prompt and `m` opens model selection.
Press `o` to open the selected run full screen, hiding the run list and diff
aside; press `o` again to return.

Each run starts in one of two modes, shown under the prompt and switched with
`ctrl+a` (or preselected with `--autopilot`):

- **default** stops for your confirmation: plan approval, the review verdict,
  and whether to commit, push or leave the changes staged.
- **autopilot** never asks. It approves the plan, sends failed reviews back to
  the executor (up to `max_iterations`), falls back to another agent on
  failure, then commits, pushes and opens a pull request with `gh`. The pull
  request is only opened when the run is on a separate branch; a run on the
  default branch (for example, a blank name while `main` is checked out) is
  pushed without one. It ends with a message summarizing the commit, push and PR link, or why it stopped.
  Autopilot runs are badged `AUTOPILOT`; `t` retries a stopped one in the same mode.

The Support tab is a terminal for the selected run: press `enter` or `!`, type a
shell command and press `enter` to run it in the run's checkout. Output streams
into the tab, `ctrl+c` stops the command, `↑↓` recalls history, and `clear`
empties the view. Commands get no stdin, so editors and pagers won't work.

Commits use your own git identity (`user.name` / `user.email`), so set an email
linked to your GitHub account. opencode drafts each commit message from your
original prompt and the diff; if drafting fails, the prompt (or plan summary) is
used instead.

Executor changes are staged before review. Press `p` to commit and push the
selected run, including after a run finishes, or `ctrl+p` while typing a prompt.
During an active agent step, publishing waits until that step yields. A failed
push keeps the commit; press `p` again to retry. When a step fails, press `t` to
retry that step without restarting earlier stages, or choose another agent.

## Commands

| Command | Description |
| --- | --- |
| `kor run [prompt]` | plan, execute and review a prompt end to end (`--autopilot` to skip every confirmation and open a PR) |
| `kor` / `kor dashboard` | open the TUI dashboard |
| `kor web` | open a browser dashboard, accessible from a phone on the same LAN |
| `kor resume` | resume a previous run |
| `kor list` | list runs |
| `kor status` | show status of a run |
| `kor clean` | remove worktrees and artifacts |
| `kor doctor` | check agents, config and repo health |

## Web dashboard / phone access

Run this on the computer with your repository and agent CLIs:

```sh
kor web
# Or without installing:
go run ./cmd/kor web
```

Keep the command running and open the printed LAN link (for example,
`http://192.168.1.20:8787/`) on your phone, connected to the same Wi-Fi. There
is no token: the server only accepts this computer and private LAN addresses
(`10.x`, `172.16–31.x`, `192.168.x`, link-local and IPv6 ULA), and only when
opened by IP address or `localhost`. Anyone on that network can control agents
on this computer, so use it only on a network you trust, not public or guest
Wi-Fi. Access is over HTTP. No cloud service or frontend build is needed.

You can also press `w` in the terminal dashboard to start the web server on
`0.0.0.0:8787` without leaving it; the LAN link appears above the key hints.
Press `w` again to stop it (twice if a browser-started run is active, since
stopping cancels it). Quitting the dashboard also stops the server. Runs
started from the browser don't appear in the terminal's run list until you
reopen the dashboard.

The responsive dashboard starts runs, shows live activity, plans, reviews and
diffs, and lets you approve or reject decisions, request fixes, select a fallback
agent, retry a failed step, and choose to commit, commit and push, or leave changes
staged. Autopilot is available when starting a run. It uses the same agent/model
configuration as the terminal dashboard. Saved runs for this repository can be
inspected and resumed from Plan, Execute or Review.

Expand **Base prompts · Plan, Execute, Review** before starting a run to view or
edit each step’s base instructions. Each step has a reset button to restore its
built-in prompt. The original request, plan, diff and retry feedback are added
automatically. Prompts are saved with the run: expand **Base prompts for this
run** to inspect them, or edit them before resuming a stopped/completed run.
Active runs show their prompts read-only. Edits affect the selected resume step
and later steps; they do not rerun earlier steps or change defaults for other runs.

To set persistent defaults for new runs (web and terminal), add `prompts.planner`,
`prompts.executor`, and/or `prompts.reviewer` strings to your JSON/YAML config.
Omitted or blank prompts use the built-in instructions. Each base prompt can be
up to 16 KiB; retain the JSON output requirements so the pipeline can read the
agent’s result.

One run can be active at a time in the web server. Closing the browser or locking
your phone does not stop it; reopening the link restores the active run and its
pending decisions. **Stop run** cancels the run while preserving its checkout and
edits. Ctrl+C stops the server and cancels its active run. After a server restart,
unfinished runs can be resumed explicitly from their saved history.

Use `kor web --allow-dirty` to permit starting with uncommitted changes,
`kor --repo /path/to/repo web` to select another repository, or
`kor web --listen 127.0.0.1:8787` for access only from the host computer.
The default is `0.0.0.0:8787`; `--listen 0.0.0.0:9000` changes the port. If the
phone cannot connect, allow the port through your host firewall and check that
your Wi-Fi does not isolate devices (as guest networks often do).

## Configuration

Edit [config.json](config.json) to define the default provider CLI (`agent`) and
`model` for each stage: `planner`, `executor`, and `reviewer`. Supported agents are
`claude`, `codex`, `opencode`, and `antigravity` (offered only when the `agy`
CLI is installed; it can fill any stage). Each stage can also set a `fallback` agent;
`variant` controls reasoning effort and `subagent` selects an OpenCode agent.
These values are preselected in the dashboard's model picker.

Without `--config`, kor uses the first file found in this order:

1. `<repo>/kor.yaml`
2. `<repo>/config.json`
3. `~/.config/kor/config.yaml`
4. `~/.config/kor/config.json`

`XDG_CONFIG_HOME` replaces `~/.config` when set. To share defaults across repos,
copy `config.json` to the user config directory. To select a file explicitly,
use `kor --config /path/to/config.json run "your prompt"`.

JSON and YAML use the same fields, and omitted fields keep their built-in defaults.
See [examples/kor.yaml](examples/kor.yaml) for all options, including iteration
limits, review gates, timeouts and budgets.

## Uninstall

```sh
./uninstall.sh
```
