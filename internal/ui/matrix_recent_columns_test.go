package ui

// matrix/hardened: the composed list owns stable numeric and device columns;
// render tests cover filtering, scrolling, hints and sidebar width fallback.

import (
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/asheshgoplani/agent-deck/internal/session"
)

func matrixRecentColumnsHome(t *testing.T, remoteName string) *Home {
	t.Helper()
	h := matrixPinnedHome(t)
	local := matrixPinnedInstance(t, h, "a1")
	local.Title, local.Status = "Local", session.StatusIdle
	matrixRecentInteraction(t, local, time.Now())
	remote := session.RemoteSessionInfo{
		ID: "columns-remote", Title: "Remote", Tool: "codex", Status: "idle",
		LastInteractionAt: time.Now().Add(-time.Minute).Format(time.RFC3339Nano),
	}
	h.remoteSessions = map[string][]session.RemoteSessionInfo{remoteName: {remote}}
	h.flatItems = []session.Item{
		{Type: session.ItemTypeSession, Session: local},
		{Type: session.ItemTypeRemoteSession, RemoteName: remoteName, RemoteSession: &h.remoteSessions[remoteName][0]},
	}
	h.groupViewMode, h.cursor = session.GroupViewRecent, 1
	h.sessionRenderSnapshot.Store(map[string]sessionRenderState{
		local.ID: {title: "Local", status: session.StatusIdle, tool: "codex"},
	})
	return h
}

func matrixRecentColumnsLine(t *testing.T, view, label string) string {
	t.Helper()
	for _, line := range strings.Split(stripAnsi(view), "\n") {
		if strings.Contains(line, label) {
			return line
		}
	}
	t.Fatalf("missing %q in list:\n%s", label, stripAnsi(view))
	return ""
}

func matrixFirstNumberColumn(t *testing.T, view string) int {
	t.Helper()
	for _, line := range strings.Split(stripAnsi(view), "\n") {
		if strings.Contains(line, "above") || strings.Contains(line, "below") {
			continue
		}
		for index, r := range line {
			if r >= '0' && r <= '9' {
				return cellWidth(line[:index])
			}
			if r != ' ' {
				break
			}
		}
	}
	t.Fatalf("no numbered row in list:\n%s", stripAnsi(view))
	return -1
}

func TestMatrixRecentColumnsNumbersStayAtNormalColumn(t *testing.T) {
	for _, count := range []int{105, 1001} {
		t.Run(fmt.Sprintf("inventory=%d", count), func(t *testing.T) {
			h := matrixPinnedHome(t)
			h.instances = make([]*session.Instance, count)
			for index := range h.instances {
				inst := session.NewInstanceWithGroupAndTool(fmt.Sprintf("row-%04d", index), "/fixture", "work", "codex")
				inst.Status = session.StatusIdle
				if index < 2 {
					matrixRecentInteraction(t, inst, time.Now().Add(-time.Duration(index)*time.Minute))
				}
				h.instances[index] = inst
			}
			h.groupTree = session.NewGroupTree(h.instances)
			h.groupViewMode = session.GroupViewNormal
			h.rebuildFlatItems()
			normal := matrixFirstNumberColumn(t, h.renderSessionList(80, 8))
			if normal < 2 {
				t.Errorf("normal numeric column = %d, want room for at least three digits", normal)
			}
			h.groupViewMode = session.GroupViewRecent
			h.rebuildFlatItems()
			if len(h.flatItems) != 2 {
				t.Fatalf("recent fixture has %d rows, want 2", len(h.flatItems))
			}
			if got := matrixFirstNumberColumn(t, h.renderSessionList(80, 8)); got != normal {
				t.Errorf("recent numeric column = %d, normal = %d", got, normal)
			}
			h.viewOffset, h.cursor = 1, 1
			if got := matrixFirstNumberColumn(t, h.renderSessionList(80, 8)); got != normal {
				t.Errorf("scrolled recent numeric column = %d, normal = %d", got, normal)
			}
			if !h.matrixJumpToLine(1) || h.cursor != 1 {
				t.Errorf("visible line 1 selected cursor %d, want the scrolled first row", h.cursor)
			}
		})
	}
}

func TestMatrixRecentColumnsDevicePrecedesSession(t *testing.T) {
	for _, embedded := range []bool{false, true} {
		for _, width := range []int{27, 40, 80} {
			t.Run(fmt.Sprintf("embedded=%v/width=%d", embedded, width), func(t *testing.T) {
				h := matrixRecentColumnsHome(t, "pro")
				h.embeddedLayout, h.compactSidebar, h.sidebarDensity = embedded, embedded, session.SidebarDensityCompact
				view := h.renderSessionList(width, 20)
				local := matrixRecentColumnsLine(t, view, "本机")
				// matrix/hardened: narrow rows may shorten the title to keep the
				// visit age, but a source column must leave a visible title.
				if !strings.Contains(local, "L") {
					t.Errorf("device column consumed the local title: %q", local)
				}
				remote := matrixRecentColumnsLine(t, view, "Remote")
				for label, line := range map[string]string{"本机": local, "pro": remote} {
					source, status := strings.Index(line, label), strings.Index(line, "○")
					if source < 0 || status < 0 || source >= status {
						t.Errorf("source %q must precede status/title: %q", label, line)
					}
					if strings.Contains(line, "[pro]") {
						t.Errorf("remote source remained attached to title: %q", line)
					}
					if cellWidth(line) > width {
						t.Errorf("row is %d cells, over %d: %q", cellWidth(line), width, line)
					}
				}
				if leadDisplayWidth(local, "○") != leadDisplayWidth(remote, "○") {
					t.Errorf("local/remote status columns differ:\n%q\n%q", local, remote)
				}
				if strings.Index(remote, "pro") > strings.Index(remote, "▶") {
					t.Errorf("device must precede selection marker: %q", remote)
				}
			})
		}
	}
}

func TestMatrixRecentColumnsKeepDeviceWithoutNumbersAndDuringJump(t *testing.T) {
	h := matrixRecentColumnsHome(t, "pro")
	h.navCount.noNumbers = true
	h.jumpMode = true
	view := h.renderSessionList(80, 16)
	hints := generateJumpHints(len(h.flatItems))
	for index, label := range []string{"Local", "Remote"} {
		line := matrixRecentColumnsLine(t, view, label)
		source := "本机"
		if index == 1 {
			source = "pro"
		}
		at := strings.Index(line, source)
		if at < 0 {
			t.Errorf("device source disappeared with nonu/jump: %q", line)
			continue
		}
		if !strings.Contains(line[at+len(source):], hints[index]) {
			t.Errorf("jump hint %q must remain after source %q: %q", hints[index], source, line)
		}
		if strings.Index(line, source) != len(line)-len(strings.TrimLeft(line, " ")) {
			t.Errorf("nonu left a numeric field before the device: %q", line)
		}
	}
}

func TestMatrixRecentColumnsClipWideDeviceAndBlankContinuation(t *testing.T) {
	const name = "远程设备名字非常长"
	for _, width := range []int{27, 40, 80} {
		t.Run(fmt.Sprintf("width=%d", width), func(t *testing.T) {
			h := matrixRecentColumnsHome(t, name)
			h.embeddedLayout, h.compactSidebar, h.sidebarDensity = true, true, session.SidebarDensityCompact
			view := stripAnsi(h.renderSessionList(width, 20))
			remote := matrixRecentColumnsLine(t, view, "Remote")
			prefix := remote[:strings.Index(remote, "○")]
			prefix = strings.TrimSpace(strings.ReplaceAll(prefix, "▶", ""))
			if !strings.HasPrefix(prefix, "远程") || !strings.Contains(prefix, "…") || cellWidth(prefix) > 10 || !utf8.ValidString(prefix) {
				t.Errorf("wide device label was not clipped safely within 10 cells: %q", prefix)
			}
			if strings.Contains(view, name) {
				t.Errorf("full device name leaked back into title/metadata:\n%s", view)
			}
			for _, line := range strings.Split(view, "\n") {
				if strings.Contains(line, "╰") && (strings.Contains(line, "本机") || strings.Contains(line, "远程")) {
					t.Errorf("continuation repeated device column: %q", line)
				}
				if cellWidth(line) > width {
					t.Errorf("wide-source row overflows %d cells: %q", width, line)
				}
			}
		})
	}
}

func TestMatrixRecentColumnsWideDeviceKeepsNarrowLocalTitle(t *testing.T) {
	h := matrixRecentColumnsHome(t, "远程设备名字非常长")
	view := h.renderSessionList(27, 20)
	local := matrixRecentColumnsLine(t, view, "本机")
	if at := strings.Index(local, "○"); at < 0 || !strings.Contains(local[at:], "L") {
		t.Fatalf("wide device column consumed narrow local title: %q", local)
	}
}

func TestMatrixRecentDeviceNamesShareLeftEdge(t *testing.T) {
	for _, embedded := range []bool{false, true} {
		for _, compact := range []bool{false, true} {
			t.Run(fmt.Sprintf("embedded=%v/compact=%v", embedded, compact), func(t *testing.T) {
				h := matrixRecentColumnsHome(t, "pro")
				h.embeddedLayout, h.compactSidebar, h.sidebarDensity = embedded, compact, session.SidebarDensityCompact
				near := matrixRecentColumnsLine(t, h.renderSessionList(80, 20), "Remote")
				// matrix/hardened: a wider device column keeps short names on
				// the same left edge, with status/title alignment preserved.
				h.remoteSessions["long-workstation-alias"] = nil
				view := h.renderSessionList(80, 20)
				wide := matrixRecentColumnsLine(t, view, "Remote")
				local := matrixRecentColumnsLine(t, view, "Local")
				if leadDisplayWidth(near, "pro") != leadDisplayWidth(wide, "pro") || leadDisplayWidth(local, "本机") != leadDisplayWidth(wide, "pro") {
					t.Fatalf("device names do not share a stable left edge:\n%q\n%q\n%q", near, local, wide)
				}
				if leadDisplayWidth(local, "○") != leadDisplayWidth(wide, "○") {
					t.Fatalf("left-aligned devices moved status columns apart:\n%q\n%q", local, wide)
				}
			})
		}
	}
}
