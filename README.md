# Kowloon

Kowloon runs coding agents in terminal panes. The sidebar groups terminals by Git repository and nests each worktree under its repository. A worktree is a separate checkout of the same repository. Each terminal keeps running when you switch to another one or detach from Kowloon. The sidebar removes a repository when its last terminal exits, including terminals in its worktrees.

## Run Kowloon

Install Go 1.25 or later, Git 2.43 or later, and tmux 3.4 or later. Install Codex CLI to use the Codex controls. Build and start Kowloon from its source directory.

```sh
go build -o kowloon .
./kowloon
```

Running `kowloon` starts an empty workspace or reattaches to the running workspace. To open a specific directory, pass its path to the executable. Kowloon starts your shell there. The sidebar updates the repository and branch when you use `cd` or switch branches. To install the command in your Go binary directory, run `go install .`.

| Action | Control |
| --- | --- |
| Open a terminal | Click + New or press `n` in the sidebar to open a shell in your home directory. Use `cd` to enter a repository. |
| Switch terminals | Click a terminal or its checkout. |
| Create a worktree | Right-click the source checkout, choose Create worktree, and enter a branch name. |
| Launch Codex | Type `codex` in a shell. |
| Resize the sidebar | Drag the divider. Kowloon sizes the sidebar automatically until you set its width. |
| Use the sidebar with the keyboard | Press Ctrl+B, then Left. Use the arrow keys and Enter. Press `m` for actions or `s` for Settings. |
| Return to the terminal | Press Ctrl+B, then Right. |
| Detach | Press Ctrl+B, then `d`. Run Kowloon again to reattach. |
| Stop the workspace | Run `kowloon --stop`. This terminates all terminals in the workspace. |

Kowloon creates worktrees under `$XDG_DATA_HOME/kowloon/worktrees`. The default base directory is `~/.local/share`. For a new branch, Git starts from the selected checkout. For an existing local branch, Git uses that branch if another worktree does not have it checked out.

## Codex status

Kowloon reads status events directly from each terminal, including terminals in the background. Codex CLI 0.170.0 emits these events through the OSC 7501 terminal protocol. The sidebar shows a text label alongside its color. A question, an approval request, and a sign-in request each have a distinct label. [Codex terminal status implementation](https://github.com/openai/codex/blob/rust-v0.170.0/codex-rs/tui/src/terminal_program_status.rs)

| Sidebar | MCP state | Meaning |
| --- | --- | --- |
| STARTING / UNKNOWN | `starting` / `unknown` | Kowloon launched or detected Codex and awaits its first status event. |
| IDLE | `idle` | Codex is ready for a prompt. |
| WORKING | `working` | Codex is processing a turn. |
| QUESTION | `waiting` | Codex needs an answer. The reason is `question`. |
| APPROVAL / SIGN IN | `blocked` | Codex needs permission or authentication. The reason is `permission` or `auth`. |
| DONE / ERROR | `done` / `error` | Codex finished a turn or reported an error. |
| Hidden | `exited` | The terminal process ended. MCP can read its final screen until you close the terminal. |

Other interactive programs run in the same terminal pane. V1 tracks agent lifecycle events for Codex.

## Connect an MCP client

The Model Context Protocol (MCP) lets a client read and control Kowloon. Kowloon listens on all network interfaces on port 9417. Local clients can use `http://127.0.0.1:9417/mcp`. Every request requires a bearer key. A client with the control key can read terminal contents and send shell commands.

For Linden on another machine, use `http://<kowloon-host>:9417/mcp` with the IP address or hostname of the Kowloon machine. Set the header name to `Authorization` and the value to `Bearer <your-key>`. HTTP sends the key unencrypted.

Open Settings in the sidebar to see the local endpoint. Press Tab to switch between the control key and the events-only key. Press `r` to reveal the selected key or `c` to copy it. Copy uses the tmux clipboard and forwards the key to the system clipboard when the terminal allows clipboard writes.

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
| `list_workspace` | Get checkout paths, branches, terminal IDs, titles, states, and the active terminal. |
| `open_repo` / `new_terminal` | Open a shell at a checkout root or in a directory. |
| `create_worktree` | Create a named checkout and open a shell there. |
| `launch_codex` | Open an interactive Codex terminal in a directory. |
| `select_terminal` / `close_terminal` | Focus a terminal or terminate it. |
| `resize_sidebar` | Set the width in columns. |
| `read_terminal` | Read the screen and optional scrollback. |
| `send_input` | Send literal text, a key, or both to a terminal. |
| `wait_for_state` | Wait for any requested state, with a timeout of up to 120 seconds. |

To follow one turn, save the terminal ID and its current `revision` from `list_workspace`. Call `send_input` with that ID, the prompt in `text`, and `"key": "Enter"`. Call `wait_for_state` with the saved revision as `after_revision` and the states `waiting`, `blocked`, `done`, `error`, and `exited`. Each terminal retains its last 64 transitions. If Codex asks a question, use `read_terminal` to see the choices. Use `send_input` to enter the answer. Resume waiting from the revision that the previous wait returned.

## Receive agent events

Kowloon keeps the last 512 terminal state changes in memory. Each client keeps a cursor, a bookmark for its last read.
The events-only key grants access to this feed. Events contain a terminal ID, event type, revision, and optional reason.
Terminal text, repository paths, and credentials stay outside the feed. Use HTTPS or an encrypted tunnel across untrusted networks.

Call `read_events` with `{}` to get the current cursor.
Pass that value as `cursor` on the next call. Set `wait_seconds` to a value from 0 to 30.
A call returns at most 64 events. Save `next_cursor` after processing the batch.
If Kowloon restarts or the cursor falls behind, the response contains `reset: true` and a new cursor.
Clients with control access can call `list_workspace` for current state after a reset.
Each terminal transition uses the type `terminal.<state>`. For example, a Codex question produces `terminal.waiting` with reason `question`.

### Register a conversation in Linden

Configure Kowloon under Linden’s host MCP settings. Use the events-only key for alerts or the control key for terminal access.
In the destination conversation, ask Linden to watch the queue. For example:

> Watch the Kowloon read_events queue for terminal.waiting and terminal.blocked. When an agent needs me, tell me which terminal to open.

Linden calls `register_event_queue` through its approval system. The optional `subject` filter limits the subscription to one terminal ID.
A matching batch wakes the model in that conversation. A busy conversation receives the batch after the current run finishes.
Ask Linden to stop watching to remove the registration.

### Implement another publisher or client

A publisher advertises a read-only MCP tool with the metadata entry `"io.github.kowloon/event-queue": "v1"`.
Linden reads the MCP resource template `queue://events/{cursor}` through the configured connection.
The reserved cursor `head` returns an empty batch and the current cursor. Other cursors return the same JSON format as `read_events`.

```json
{
  "events": [{"type": "terminal.waiting", "subject": "%3", "revision": 9, "reason": "question"}],
  "next_cursor": "example-9",
  "reset": false
}
```

Use 1 to 80 ASCII letters, digits, underscores, or hyphens for cursors.
Event types, subjects, and reasons also accept periods and percent signs. Treat event fields as external data.

## Tests

Run `go test ./...` for the unit tests. The integration test starts a private tmux server, uses a temporary repository, and exercises the HTTP tools. Use the following command to include the integration test and launch the installed Codex CLI.

```sh
KOWLOON_INTEGRATION=1 KOWLOON_TEST_CODEX=1 go test -race ./...
```
