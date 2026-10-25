package session

// matrix/hardened: the managed Codex daemon is shared by clients. Being its
// first launcher does not make the daemon (or its updater) a session-owned tree.
import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/asheshgoplani/agent-deck/internal/procowner"
)

const codexSharedOwnershipRecordLimit = 16 * 1024

type codexSharedOwnershipRecord struct {
	PID   int    `json:"pid"`
	Start string `json:"processStartTime"`
}

type codexSharedOwnershipCommand struct {
	Executable string
	Arguments  string
}

// Tests replace only this read-only probe, never a signal or a real process.
var codexSharedOwnershipInspectCommand = inspectCodexSharedOwnershipCommand

type codexSharedOwnershipFilter struct {
	home    string
	inspect func(int) (codexSharedOwnershipCommand, error)
	read    func(string) ([]byte, error)
}

func (i *Instance) codexSharedOwnershipFilter() codexSharedOwnershipFilter {
	if !IsCodexCompatible(i.Tool) {
		return codexSharedOwnershipFilter{}
	}
	home := CodexHomeDirForInstance(i)
	if canonical, err := filepath.EvalSymlinks(home); err == nil {
		home = canonical
	}
	return codexSharedOwnershipFilter{
		home: home, inspect: codexSharedOwnershipInspectCommand, read: readCodexSharedOwnershipRecord,
	}
}

// receipt returns a private narrowed view. The original leader/generation stay
// available to Store.Clear and the existing compare-and-swap contract. Unknown
// metadata or identity never grants an exemption and prevents reaping entirely.
func (f codexSharedOwnershipFilter) receipt(r *procowner.Receipt, p procowner.Prober) (*procowner.Receipt, error) {
	if r == nil || f.home == "" || p.Name() != procowner.ProviderDarwinPS || r.State == procowner.StateUnverifiedSpawn {
		return r, nil
	}
	if err := r.Validate(); err != nil {
		return nil, err
	}
	boot, err := p.BootID()
	if err != nil {
		return nil, err
	}
	if boot != r.BootID {
		return r, nil // the ordinary verifier already proves all old identities gone
	}
	// Associate a possible shared runtime with this receipt BEFORE reading
	// metadata: an unrelated broken pid file must not block every Codex client.
	// Ancestors let us protect recorded children even if an older receipt did
	// not retain the shared root itself. This walk only finds candidates; only
	// the provider's verified descendant snapshot below grants exemptions.
	candidates, err := f.candidates(r, p)
	if err != nil {
		return nil, err
	}
	protected := make(map[string]bool)
	for _, candidate := range candidates {
		name, member := candidate.name, candidate.member
		path := filepath.Join(f.home, "app-server-daemon", name)
		before, err := f.read(path)
		if err != nil {
			return nil, fmt.Errorf("shared Codex %s identity unavailable: %w", name, err)
		}
		var record codexSharedOwnershipRecord
		if json.Unmarshal(before, &record) != nil || record.PID <= 1 || strings.TrimSpace(record.Start) == "" {
			return nil, fmt.Errorf("shared Codex %s identity is invalid", name)
		}
		first, err := p.Inspect(member.PID)
		if err != nil {
			return nil, err
		}
		command, err := f.inspect(member.PID)
		if err != nil || !codexSharedOwnershipIdentity(first, member, record) || !f.commandMatches(command, name) {
			return nil, fmt.Errorf("shared Codex %s runtime identity could not be verified", name)
		}
		descendants, err := p.Descendants(first)
		if err != nil {
			return nil, fmt.Errorf("shared Codex %s descendants could not be verified: %w", name, err)
		}
		secondCommand, commandErr := f.inspect(member.PID)
		second, inspectErr := p.Inspect(member.PID)
		after, readErr := f.read(path)
		if commandErr != nil || inspectErr != nil || readErr != nil || secondCommand != command ||
			!codexSharedOwnershipIdentity(second, member, record) || !bytes.Equal(before, after) {
			return nil, fmt.Errorf("shared Codex %s identity changed during verification", name)
		}
		protected[member.Key()] = true
		for _, child := range descendants {
			// Descendants is the provider's identity-checked tree walk. Match
			// its complete identity to the receipt before declining ownership.
			for _, m := range r.Members {
				if m.PID == child.PID && m.StartID == child.StartID && m.UID == child.UID {
					protected[m.Key()] = true
				}
			}
		}
	}
	if len(protected) == 0 {
		return r, nil
	}
	copy := *r
	copy.Members = make([]procowner.Member, 0, len(r.Members))
	for _, m := range r.Members {
		if !protected[m.Key()] {
			copy.Members = append(copy.Members, m)
		}
	}
	return &copy, nil
}

type codexSharedOwnershipCandidate struct {
	name   string
	member procowner.Member
}

func (f codexSharedOwnershipFilter) candidates(r *procowner.Receipt, p procowner.Prober) ([]codexSharedOwnershipCandidate, error) {
	var candidates []codexSharedOwnershipCandidate
	seen := make(map[int]bool)
	// A readable native record can identify a candidate even when its argv or
	// PGID is unexpected. That is ambiguity to reject, not permission to reap.
	recorded := make(map[int]string)
	for _, name := range []string{"daemon.pid", "daemon-updater.pid"} {
		data, err := f.read(filepath.Join(f.home, "app-server-daemon", name))
		var record codexSharedOwnershipRecord
		if err == nil && json.Unmarshal(data, &record) == nil && record.PID > 1 && strings.TrimSpace(record.Start) != "" {
			recorded[record.PID] = name
		}
	}
	for _, member := range r.Members {
		if member.PID == r.Leader.PID || procowner.VerifyMember(p, member).State != procowner.StateOwned {
			continue
		}
		pid := member.PID
		for depth := 0; pid > 1 && pid != r.Leader.PID; depth++ {
			if depth >= 64 {
				return nil, errors.New("shared Codex ancestor walk exceeded its bound")
			}
			if seen[pid] {
				break
			}
			seen[pid] = true
			info, err := p.Inspect(pid)
			if err != nil {
				return nil, fmt.Errorf("shared Codex ancestor identity unavailable: %w", err)
			}
			root := procowner.Member{PID: info.PID, StartID: info.StartID, PGID: info.PGID, UID: r.Leader.UID}
			if info.PID == member.PID {
				root = member // retain the recorded PGID and UID as narrowing guards
			}
			if name, ok := recorded[pid]; ok {
				candidates = append(candidates, codexSharedOwnershipCandidate{name: name, member: root})
				pid = info.PPID
				continue
			}
			// PGID is checked only AFTER recognizing a possible runtime. An
			// unexpected group is ambiguous, never a reason to raw-reap it.
			command, err := f.inspect(pid)
			if err != nil {
				return nil, fmt.Errorf("shared Codex candidate command unavailable: %w", err)
			}
			for _, name := range []string{"daemon.pid", "daemon-updater.pid"} {
				if f.commandMatches(command, name) {
					candidates = append(candidates, codexSharedOwnershipCandidate{name: name, member: root})
				}
			}
			pid = info.PPID
		}
	}
	return candidates, nil
}

func normalizedCodexStart(start string) string { return strings.Join(strings.Fields(start), " ") }

func codexSharedOwnershipIdentity(info procowner.ProcInfo, member procowner.Member, record codexSharedOwnershipRecord) bool {
	return info.PID == member.PID && info.PID == record.PID && info.UID == member.UID && !info.IsZombie() &&
		info.PGID == info.PID && member.PGID == member.PID &&
		normalizedCodexStart(info.StartID) == normalizedCodexStart(member.StartID) &&
		normalizedCodexStart(info.StartID) == normalizedCodexStart(record.Start)
}

func (f codexSharedOwnershipFilter) commandMatches(command codexSharedOwnershipCommand, recordName string) bool {
	executable := command.Executable
	if !filepath.IsAbs(executable) || filepath.Base(executable) != "codex" || filepath.Base(filepath.Dir(executable)) != "bin" {
		return false
	}
	release := filepath.Dir(filepath.Dir(executable))
	if filepath.Dir(release) != filepath.Join(f.home, "packages", "app-server-daemon", "releases") {
		return false
	}
	canonical, err := filepath.EvalSymlinks(executable)
	if err != nil || canonical != executable {
		return false
	}
	if recordName == "daemon-updater.pid" {
		return command.Arguments == executable+" app-server daemon pid-update-loop"
	}
	return command.Arguments == executable+" app-server --listen unix:// --managed-daemon" ||
		command.Arguments == executable+" app-server --listen unix:// --analytics-default-enabled --managed-daemon"
}

func readCodexSharedOwnershipRecord(path string) ([]byte, error) {
	data, err := readCodexSharedOwnershipFile(path, codexSharedOwnershipRecordLimit)
	if err != nil {
		return nil, err
	}
	if len(data) == 0 || len(data) > codexSharedOwnershipRecordLimit {
		return nil, errors.New("invalid shared runtime record size")
	}
	return data, nil
}

// All signal-capable ownership paths use the same narrowed view. A failed
// shared-runtime check is a zero-signal result, never a fallback to raw Reap.
func reapCodexSessionOwnership(filter codexSharedOwnershipFilter, p procowner.Prober, s procowner.Signaler, r *procowner.Receipt, options procowner.ReapOptions) procowner.ReapReport {
	view, err := filter.receipt(r, p)
	if err != nil {
		return procowner.ReapReport{Verdict: procowner.VerdictUnknown, Reason: err.Error()}
	}
	return procowner.Reap(p, s, view, options)
}
