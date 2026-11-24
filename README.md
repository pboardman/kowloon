# Kowloon

Kowloon runs coding agents in terminal panes. The sidebar groups terminals by Git repository and nests each worktree under its repository. A worktree is a separate checkout of the same repository. Each terminal keeps running when you switch to another one or detach from Kowloon. The sidebar removes a repository when its last terminal exits, including terminals in its worktrees.

## Run Kowloon

Install Go 1.25 or later, Git 2.43 or later, and tmux 3.4 or later. Install Codex CLI to use the Codex controls. Build and start Kowloon from its source directory.

```sh
go build -o kowloon .
./kowloon
```

Running `kowloon` reattaches to the running workspace. If the workspace stopped, Kowloon restores the last saved session. The first start opens an empty workspace. To open a specific directory, pass its path to the executable. Kowloon starts your shell there. The sidebar updates the repository and branch when you use `cd` or switch branches. To install the command in your Go binary directory, run `go install .`.

| Action | Control |
| --- | --- |
| Open a terminal | Click + New or press `n` in the sidebar to open a shell in your home directory. Use `cd` to enter a repository. |
| Switch terminals | Click a terminal or its checkout. |
| Create a worktree | Right-click the source checkout, choose Create worktree, and enter a branch name. |
| Launch Codex | Type `codex` in a shell. |
| Resize the sidebar | Drag the divider. Kowloon sizes the sidebar automatically until you set its width. |
| Use the sidebar with the keyboard | Press Ctrl+B, then Left. Use the arrow keys and Enter. Press `m` for actions or `s` for Settings. |
| Toggle agent mode | Press Ctrl+B, then `a` to focus the sidebar and toggle the flat agent list. In the sidebar, press `a` directly. |
| Return to the terminal | Press Ctrl+B, then Right. |
| Detach | Press Ctrl+B, then `d`. Run Kowloon again to reattach. |
| Upgrade the running server | After installing the new binary, run `kowloon --restart`. |
| Skip recovery | When the workspace is stopped, run `kowloon --no-restore` to start fresh. |
| Stop the workspace | Run `kowloon --stop`. This terminates all terminals in the workspace. |

Kowloon creates worktrees under `$XDG_DATA_HOME/kowloon/worktrees`. The default base directory is `~/.local/share`. For a new branch, Git starts from the selected checkout. For an existing local branch, Git uses that branch if another worktree does not have it checked out.

Agent mode lists open Codex sessions in terminal creation order. Each entry shows the status symbol, repository, and available task title. The branch appears on the line below. An idle agent or a finished turn stays visible until Codex exits. Use the arrow keys and Enter to open a listed terminal.

## Session recovery

For an upgrade, install the new binary and run `kowloon --restart`. This replaces the sidebar and MCP server. Tmux keeps the same terminal processes, pane IDs, scrollback, and running agents. If only the Kowloon server crashes, run `kowloon` to reconnect to those panes.

After a reboot or a tmux shutdown, run `kowloon` to reopen the saved workspace. Kowloon restores terminal directories, worktrees, the selected terminal, agent mode, and sidebar width. Shells start fresh in their saved directories. Commands interrupted by the shutdown need to be started again.

For a managed Codex terminal, answer the folder and hook questions to resume its saved conversation. For Codex started by hand, choose the conversation from the Codex resume picker. Kowloon keeps run receipts and reports an unconfirmed turn as `unavailable`. Cold recovery can assign new terminal IDs. MCP clients can use `list_workspace` to get the current IDs.

Use `kowloon --no-restore` to start empty after shutdown, or `kowloon --no-restore /path/to/repo` to open just that directory. The fresh workspace replaces the saved session. When tmux is still running, Kowloon reattaches to the live workspace.

Kowloon saves changes automatically in `session.json` beside the MCP key. Only your account can read the file. The file contains workspace metadata and run receipts. Terminal contents, prompts, and approval choices stay outside the file. If a saved directory is unavailable, Kowloon tries it again at the next cold start.

## Codex status

Kowloon reads status events directly from each terminal, including terminals in the background. Codex CLI 0.170.0 emits these events through the OSC 7501 terminal protocol. Each sidebar state has its own symbol. [Codex terminal status implementation](https://github.com/openai/codex/blob/rust-v0.170.0/codex-rs/tui/src/terminal_program_status.rs)

| Sidebar | MCP state | Meaning |
| --- | --- | --- |
| ◌ / … | `starting` / `unknown` | Kowloon launched or detected Codex and awaits its first status event. |
| ○ | `idle` | The terminal reports idle. Use `get_agent_status` for prompt readiness. |
| ● | `working` | Codex is processing a turn. |
| ? | `waiting` | Codex needs an answer. The reason is `question`. |
| ! / @ | `blocked` | Codex needs permission or authentication. The reason is `permission` or `auth`. |
| ⊘ | `blocked` | Codex reports another blocker. |
| ✓ / × | `done` / `error` | Codex finished a turn or reported an error. |
| Hidden | `exited` | The terminal process ended. MCP can read its final screen until you close the terminal. |

Kowloon rings the terminal bell when Codex finishes a turn, asks a question, needs approval or sign-in, reports another blocker, or reports an error. This includes background agents in either sidebar view. Repeated reports of the same state and reason ring once. The agent bell is on by default. In sidebar Settings, press `b` or click **Agent bell** to switch it off or on. Kowloon saves your choice for future workspaces. To hear the bell, enable audible bells in your terminal settings.

Other interactive programs run in the same terminal pane. V1 tracks agent lifecycle events for Codex.

## Connect an MCP client

The Model Context Protocol (MCP) lets a client read and control Kowloon. Kowloon listens on all network interfaces on port 9417. Local clients can use `http://127.0.0.1:9417/mcp`. Every request requires a bearer key. A client with the control key can read terminal contents and send shell commands.

For Linden on another machine, use `http://<kowloon-host>:9417/mcp` with the IP address or hostname of the Kowloon machine. Set the header name to `Authorization` and the value to `Bearer <your-key>`. HTTP sends the key unencrypted. Use HTTPS or an encrypted tunnel across untrusted networks.

Open Settings in the sidebar to see the local endpoint. Press `r` to reveal the MCP key or `c` to copy it. Copy uses the tmux clipboard and forwards the key to the system clipboard when the terminal allows clipboard writes.

In an external Bash shell, run these commands to register Kowloon with Codex. Paste the key at the hidden prompt.

```bash
read -rs -p 'Kowloon MCP key: ' KOWLOON_MCP_KEY
echo
export KOWLOON_MCP_KEY
codex mcp add kowloon --url http://127.0.0.1:9417/mcp --bearer-token-env-var KOWLOON_MCP_KEY
codex
```

Start the MCP client in an environment that contains `KOWLOON_MCP_KEY`. For another port, set `KOWLOON_PORT` before starting the workspace and use the endpoint in Settings. To start Kowloon without attaching a terminal, run `kowloon --detach /path/to/repo`.

| Tool | Action |
| --- | --- |
| `save_session` | Save the current Kowloon workspace immediately. |
| `list_conversations` | List live agent terminals, their conversation IDs, current state, and pending questions. |
| `wait_for_next_event` | Pass a conversation ID to wait for its next question, blocker, outcome, or exit. |
| `list_workspace` | Get checkout paths, branches, terminal IDs, titles, states, and the active terminal. |
| `open_repo` / `new_terminal` | Open a shell at a checkout root or in a directory. |
| `create_worktree` | Create a named checkout and open a shell there. |
| `create_or_open_workspace` | Create a directory or reuse a terminal there. Set `init_git` to initialize Git and `launch_codex` to open Codex. |
| `launch_codex` | Open a new interactive Codex terminal with run tracking. |
| `get_agent_status` | Read `can_submit` and the current setup decision for a terminal. |
| `answer_setup` | Choose how Codex handles folder trust or hooks for this session. |
| `submit_prompt` | Submit a prompt and receive the run ID that Codex accepted. |
| `get_run` / `wait_for_run` | Read or wait for the status of one run by ID. |
| `list_questions` | Read pending Codex questions and choices. Pass a run ID to see questions for that run. |
| `answer_question` | Send answers for one pending request. Pass its request ID and one answer per question ID. |
| `select_terminal` / `close_terminal` | Focus a terminal or terminate it. |
| `set_agent_mode` | Set `enabled` to show the flat agent list or clear it to restore the repository tree. `list_workspace` reports `agent_mode`. |
| `set_agent_bell` | Set `enabled` to turn the agent bell on or off. `list_workspace` reports `agent_bell`. |
| `resize_sidebar` | Set the width in columns. |
| `read_terminal` | Read the screen and optional scrollback. |
| `send_input` | Send literal text, a key, or both to a terminal. |
| `wait_for_state` | Wait for terminal activity for up to 120 seconds. |

### Submit a prompt from Linden

1. Call `create_or_open_workspace` with an absolute `path` and `launch_codex: true`. For a new Git repository, also pass `init_git: true`.
2. Call `get_agent_status` with the terminal `id`. If it returns a blocker, pass its `kind` and a listed `choice` to `answer_setup`.
3. For folder trust, choose `open_untrusted`, `trust_folder`, or `cancel`. The trust choice applies to this Codex session.
4. For hook review, choose `continue_without_hooks` to disable hooks. Choose `use_previously_trusted_hooks` to use the normal Codex hook policy.
5. When `can_submit` is true, call `submit_prompt` with `id` and `prompt`. Save the returned `run_id`.
6. Call `wait_for_run` with that `run_id`. On timeout, the response includes the current run status and `matched: false`.

For retries, include a unique `request_id` and reuse it with the same terminal and prompt. Kowloon returns the saved receipt while it retains that run.

Codex supplies the run ID. An accepted run can report `running`, `waiting_for_approval`, or `waiting_for_input`. Use `list_questions` for structured Codex questions. Use the terminal for command approvals, secret input, and questions that Codex prints as ordinary text. Call `wait_for_run` with `states: ["finished", "failed", "interrupted"]` to wait for an outcome.

Only the matching Codex turn can report `finished`, `failed`, or `interrupted`. If Kowloon loses contact, it reports `unavailable`. Kowloon retains active runs and the last 512 closed run records across workspace restarts.

Run tracking uses the Codex app server and its interactive terminal. It was tested with Codex CLI 0.171.0. For Codex started by hand in a shell, use the terminal tools. [Codex app server](https://developers.openai.com/codex/app-server/)

## Wait for a conversation in Linden

Ask Linden to list the conversations, choose one by repository or title, and wait for its next event.

1. Call `list_conversations` with `{}`. The result shows each live agent terminal and any questions already open.
2. Call `wait_for_next_event` with the returned ID, for example `{"conversation_id":"%3"}`.
3. If the call times out after 30 seconds, call it again with the same ID.

Kowloon remembers the read position from the first listing. Events that arrive between calls stay available. A structured question includes the text, choices, and request ID for `answer_question`. Both conversation tools use the control key.

A conversation ID identifies one live agent terminal. Callers watching the same terminal share its read position. If the event history fills up, the result includes `history_gap: true`. List the conversations again for the current state.

### Answer a Codex question in Linden

For a run started with `submit_prompt`, ask Linden:

> Use Kowloon to wait for this conversation. When Codex asks a question, show me the choices and send my answer back.

For each request, Linden passes the returned `request_id` to `answer_question` with an `answers` object. Each entry maps a question ID to the user's answer. Use the option label for a menu choice. Questions with `isOther: true` or no choices also accept written answers.

A `sent` receipt means Kowloon sent the reply to Codex. Codex resolves the request once, even if someone also answers in the terminal. Use `wait_for_run` to see what happens next.

## Tests

Run `go test ./...` for the unit tests. The integration test starts a private tmux server, uses a temporary repository, and exercises the HTTP tools. Use the following command to include the integration test and launch the installed Codex CLI.

```sh
KOWLOON_INTEGRATION=1 KOWLOON_TEST_CODEX=1 go test -race ./...
```
