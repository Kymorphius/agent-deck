# Safe session sleep

<!-- matrix/hardened: process lifetime is separate from saved conversation lifetime. -->

## Behavior

`[lifecycle] auto_sleep = true` enables conservative idle process reclamation.
`sleep_after = "1h"` is the default grace period when enabled. The feature is
disabled when omitted. It initially supports ordinary local Codex user threads;
other tools and ambiguous sessions keep running.

A process holding several native thread writers stays awake, even when some
subagents show completed output. Their identity and lifecycle need independent
proof; older collaboration sessions can therefore remain ineligible.

The list entry, native conversation ID, transcript and project files survive
sleep. Before stopping, Agent Deck writes the complete available tmux scrollback
to a private artifact. A nonempty or unrecognized composer prevents sleep, so
unsent input stays in its original running process.

Only an explicit interaction wakes an automatically sleeping session: opening
its interactive terminal, attaching, or sending input. Reading history, menu
refreshes and passive WebSocket connections do not wake it. Wake must resume the
same native thread; a missing, changed or nonresumable thread is an error rather
than permission to create a new conversation. Manual stop retains its existing
meaning and is not treated as automatic sleep.

## Safety checks

The native turn must have finished normally and no later input or unfinished
event may be present. The managed pane must have an empty recognized composer,
no approval/menu, queued follow-up or background work, no interactive viewer and
no additional panes/windows. Active child sessions, outstanding inbox/queued
messages, unresolved submissions and unknown process descendants block sleep.
Configured idle MCP processes are identified by executable, arguments and
verified ownership, never by a broad `node` or `python` process-name match.

Idle age is measured from the most recent native completion, recorded activity
and access, and requires stable evidence across controller passes. Before the
stop, the controller rechecks under the instance spawn and Codex submission
locks and temporarily disables input to the exact tmux pane before its final
fresh check. A changed pane or a newly attached interactive viewer cancels the
transition and restores input. Agent Deck interaction paths record access in
the owning database so a detached controller cannot ignore a new viewer.
Unmanaged direct tmux clients can encounter this short input-gated transition;
on cancellation a message asks them to retype input. Commands sent directly to
tmux outside Agent Deck's lifecycle protocol are not serialized by its locks.
The sleep marker and scrollback provide recovery information; completion
is reported only after the owned processes are confirmed stopped. Unknown or
partial results remain visible and do not cause a second blind termination.

Saved scrollback references remain available after a successful wake. They are
separate from the active sleep marker; normal live output stays live, while
`session output --saved-pane` reads the retained output without restarting.

## Ownership and scope

One controller per profile owns a local advisory lock. TUI and headless Web
hosts can run it; their shutdown cancels the controller and closes its separate
storage handle. It does not add a LaunchAgent, change the shared read-only
monitor, or re-enable group concurrency queues. No controller runs when every
Agent Deck management process is closed.

The older per-session `idle_timeout_secs` option remains explicit and separate;
its output-inactivity semantics are not the safety proof used by this feature.

## Verification

Use `scripts/run-isolated.py` for regression and race tests. Cover exact native
identity, malformed/partial records, active work, composer drafts, viewers,
pending messages, private scrollback persistence, concurrent stop/wake, failed
cleanup, marker round trips, and passive reads versus explicit interactions.
Deployment must distinguish the installed executable from loaded TUI/Web hosts
and verify that unrelated AI pane PIDs stay unchanged.
