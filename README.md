# Kowloon

A minimal agent multiplexer using tmux for session persistence.

## Installing

- Grab a tarball from the latest [Release](https://github.com/pboardman/kowloon/releases/latest)
- Unzip it and put the `kowloon` binary somewhere in your PATH
- Run `kowloon`

## Building

Kowloon has been tested on Linux and MacOS right now, compatibility with Windows is unknown.

Install Go 1.25 or later, git, and tmux then run:

```sh
go build -o kowloon .
```

## Usage

Running `kowloon` reattaches to the running workspace. If the workspace was stopped, Kowloon will attempt to restore it from a saved session.

### Important actions reference

| Action | Control |
| --- | --- |
| Open a terminal | Click + New or press `n` in the sidebar to open a shell in your home directory. Use `cd` to enter a repository. |
| Create a worktree | Right-click a repo on the side bar, choose Create worktree, and enter a branch name. |
| Resize the sidebar | Drag the divider. Kowloon sizes the sidebar automatically until you set its width. |
| Use the sidebar with the keyboard | Press Ctrl+B, then Left. Use the arrow keys and Enter. |
| Toggle agent mode | Press Ctrl+B, then `a` to focus the sidebar and toggle the flat agent list. |
| Return to the terminal | Press Ctrl+B, then Right. |
| Detach | Press Ctrl+B, then `d`. Run `kowloon` again to reattach. |
| Upgrade the running server | After installing the new binary, run `kowloon --restart`. (This should not stop your session) |
| Skip recovery | When the workspace is stopped, run `kowloon --no-restore` to start fresh. |
| Stop the workspace | Run `kowloon --stop`. This terminates all terminals in the workspace. |

Kowloon creates worktrees under `$XDG_DATA_HOME/kowloon/worktrees`. For a new branch, git starts from the selected checkout. For an existing local branch, Git uses that branch if another worktree does not have it checked out.

Agent mode lists open Claude Code and Codex sessions in terminal creation order.

## Agent status

Kowloon tracks activity in visible and background terminals. Here is the different possible status

| Sidebar | MCP state | Meaning |
| --- | --- | --- |
| ◌ / … | `starting` / `unknown` | Kowloon launched or detected the agent and awaits its first status event. |
| ○ | `idle` | The terminal reports idle. For managed Codex, use `get_agent_status` for prompt readiness. |
| ● | `working` | The agent is processing a turn. |
| ? | `waiting` | The agent needs an answer. The reason is `question`. |
| ! / @ | `blocked` | The agent needs permission or authentication. The reason is `permission` or `auth`. |
| ⊘ | `blocked` | The agent reports another blocker. |
| ✓ / × | `done` / `error` | The agent finished a turn or reported an error. |
| Hidden | `exited` | The terminal process ended. MCP can read its final screen until you close the terminal. |

## Connect an MCP client

The Model Context Protocol (MCP) lets a client read and control Kowloon. By default, Kowloon listens only on localhost Local clients can use `http://127.0.0.1:9417/mcp`. Every request requires a bearer key. A client with the control key can read terminal contents and send shell commands.

On first start, Kowloon creates `~/.config/kowloon/config.json` on Linux. On macOS, the file is in `~/Library/Application Support/kowloon/`.

```json
{
  "bind": "127.0.0.1",
  "port": 9417
}
```

From a remote MCP client, use `http://<kowloon-host>:9417/mcp` and set the `Authorization` header to `Bearer <your-key>`.

(Open Settings in the sidebar to see the local endpoint. Press `r` to reveal the MCP key or `c` to copy it.)

## Releases

To publish a release, a tag such as `v0.1.0` must be pushed to GitHub at the intended commit on `main`.
format is: `vMAJOR.MINOR.PATCH`.

## Tests

Run `go test ./...` for the unit tests. To also run the integration tests:

```sh
KOWLOON_INTEGRATION=1 KOWLOON_TEST_CODEX=1 go test -race ./...
```
