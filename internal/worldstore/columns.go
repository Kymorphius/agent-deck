package worldstore

import (
	"sort"
	"strings"
)

// Column is one kanban column: a bridge lane with its tasks in board order.
type Column struct {
	Lane  string
	Title string
	Tasks []Task
	// Hidden counts tasks left out (only the done column is capped).
	Hidden int
}

// Lanes in board order, with the title shown for each. The bridge maps
// statuses it does not know (已调整, 周期进行中, ...) to "review"; those stay
// visible but are never moved by agent-deck's plain status change.
var laneOrder = []struct{ lane, title string }{
	{"doing", "进行中"},
	{"todo", "待办"},
	{"paused", "暂停"},
	{"review", "待核对"},
	{"done", "已完成"},
}

// DoneLimit caps the done column to the most recent completions.
const DoneLimit = 20

// Columns groups tasks into board columns. Cancelled tasks are left out.
// Open lanes sort by plan date (undated last) then name; done sorts by
// completion time, newest first.
func Columns(tasks []Task, filter string) []Column {
	filter = strings.ToLower(strings.TrimSpace(filter))
	by := map[string][]Task{}
	for _, t := range tasks {
		if t.Lane == "cancelled" {
			continue
		}
		if filter != "" && !strings.Contains(strings.ToLower(t.Name+" "+t.Category+" "+t.Status), filter) {
			continue
		}
		by[t.Lane] = append(by[t.Lane], t)
	}
	cols := make([]Column, 0, len(laneOrder))
	for _, lo := range laneOrder {
		ts := by[lo.lane]
		if lo.lane == "done" {
			sort.SliceStable(ts, func(i, j int) bool { return deref(ts[i].CompletedAt) > deref(ts[j].CompletedAt) })
		} else {
			sort.SliceStable(ts, func(i, j int) bool {
				a, b := ts[i].PlanDate, ts[j].PlanDate
				if (a == nil) != (b == nil) {
					return a != nil
				}
				if a != nil && *a != *b {
					return *a < *b
				}
				return ts[i].Name < ts[j].Name
			})
		}
		col := Column{Lane: lo.lane, Title: lo.title, Tasks: ts}
		if lo.lane == "done" && len(ts) > DoneLimit {
			col.Tasks, col.Hidden = ts[:DoneLimit], len(ts)-DoneLimit
		}
		cols = append(cols, col)
	}
	return cols
}

func deref(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

// LaunchPrompt is the first message for a session started from a task.
func LaunchPrompt(t Task) string {
	var sb strings.Builder
	sb.WriteString("World Store 任务：")
	sb.WriteString(t.Name)
	if t.Category != "" {
		sb.WriteString("\n分类：" + t.Category)
	}
	if plan := strings.TrimSpace(t.PlanText); plan != "" {
		sb.WriteString("\n计划说明：\n" + plan)
	}
	sb.WriteString("\n\n请先确认目标和范围，再开始处理。")
	return sb.String()
}
