package session

import "github.com/asheshgoplani/agent-deck/internal/tmux"

// CLIStatusCandidates avoids probing each stopped session separately. A stopped
// row with no tmux session in a complete socket listing retains its stored
// status; an indeterminate listing also retains it, marked as cached.
//
// matrix/hardened: every other row whose tmux session is missing from a
// complete listing of its own socket is still refreshed, but its absence is
// noted on the tmux session first, so the status pass reaches the same verdict
// without one has-session fork per dead row. Fresh loads (`list --json`, the
// remote-agent probe) otherwise forked ~150 times per pass on a host with many
// error rows, every few seconds per connected remote.
func CLIStatusCandidates(instances []*Instance) ([]*Instance, map[*Instance]bool) {
	refresh := make([]*Instance, 0, len(instances))
	cached := make(map[*Instance]bool)
	bySocket := make(map[string]map[string]struct{})
	// listing returns the session names on socket, or nil when the listing
	// failed (indeterminate).
	listing := func(socket string) map[string]struct{} {
		names, ok := bySocket[socket]
		if !ok {
			var err error
			names, err = tmux.ListSessionNamesOnSocket(socket)
			if err != nil {
				names = nil
			}
			bySocket[socket] = names
		}
		return names
	}
	for _, inst := range instances {
		if inst == nil {
			continue
		}
		sess := inst.GetTmuxSession()
		if inst.Status != StatusStopped {
			if sess != nil {
				if names := listing(sess.SocketName); names != nil {
					if _, exists := names[sess.Name]; !exists {
						sess.NoteAbsentFromListing()
					}
				}
			}
			refresh = append(refresh, inst)
			continue
		}
		if sess == nil {
			cached[inst] = true
			continue
		}
		if _, exists := listing(sess.SocketName)[sess.Name]; !exists {
			cached[inst] = true
			continue
		}
		refresh = append(refresh, inst)
	}
	return refresh, cached
}
