package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/lyingloong/gpuglint/internal/state"
	"github.com/rivo/uniseg"
)

func displayState(out io.Writer, data []byte, o options) error {
	if o.json {
		_, err := out.Write(data)
		return err
	}
	var s snapshot
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}
	if o.errorsOnly {
		return displayErrors(out, s)
	}
	return drawTable(out, s, o.order)
}

type dashboard struct {
	out         io.Writer
	interactive bool
	restore     func()
}

func openDashboard(out io.Writer) *dashboard {
	d := &dashboard{out: out, restore: func() {}}
	if f, ok := out.(*os.File); ok {
		d.interactive, d.restore = terminalMode(f)
	}
	if d.interactive {
		fmt.Fprint(out, "\x1b[?1049h\x1b[?25l")
	}
	return d
}

func (d *dashboard) close() {
	if d.interactive {
		fmt.Fprint(d.out, "\x1b[?25h\x1b[?1049l")
	}
	d.restore()
}

func (d *dashboard) draw(s snapshot, order string) error {
	var b bytes.Buffer
	width, height := 100, 0
	if d.interactive {
		width, height = terminalSize(d.out.(*os.File))
		// Stay off the terminal's auto-wrap boundary even with immediate EOL wrap.
		if width > 1 {
			width--
		}
		b.WriteString("\x1b[H")
	}
	lines := hostOverview(s, order, width, height, d.interactive, d.interactive && os.Getenv("NO_COLOR") == "")
	if d.interactive {
		// Erase only the unused part of each row, then any old rows below.
		// Omitting a trailing newline avoids scrolling the alternate screen.
		b.WriteString(strings.Join(lines, "\x1b[K\r\n"))
		b.WriteString("\x1b[J")
	} else {
		b.WriteString(strings.Join(lines, "\n"))
		b.WriteByte('\n')
	}
	_, err := b.WriteTo(d.out)
	return err
}

func follow(ctx context.Context, out io.Writer, data []byte, o options) error {
	d := openDashboard(out)
	defer d.close()
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		var s snapshot
		if err := json.Unmarshal(data, &s); err != nil {
			return err
		}
		if err := d.draw(s, o.order); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
		var err error
		data, err = state.Read(o.path)
		if errors.Is(err, state.ErrInactive) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// Keep terminal control sequences, newlines and bidi overrides out of cells.
// Use grapheme boundaries and terminal-cell widths for both clipping and padding.
func clipped(text string, width int) string {
	text = strings.Map(func(r rune) rune {
		if !unicode.IsPrint(r) {
			return ' '
		}
		return r
	}, text)
	if width <= 0 {
		return ""
	}
	if uniseg.StringWidth(text) <= width {
		return text
	}
	var b strings.Builder
	g := uniseg.NewGraphemes(text)
	used := 0
	for g.Next() && used+g.Width() < width {
		b.WriteString(g.Str())
		used += g.Width()
	}
	return b.String() + "…"
}

func shortError(e *failure) string {
	if e == nil {
		return "Failed"
	}
	switch e.Code {
	case "timeout":
		return "Timed out"
	case "no_collector":
		return "No GPU tool"
	case "collection_failed":
		return "Probe failed"
	case "invalid_host":
		return "Invalid alias"
	case "cancelled":
		return "Cancelled"
	case "ssh_error":
		msg := strings.ToLower(e.Message)
		switch {
		case strings.Contains(msg, "host key"), strings.Contains(msg, "identification has changed"):
			return "Host key error"
		case strings.Contains(msg, "permission denied"), strings.Contains(msg, "authentication"):
			return "Auth denied"
		case strings.Contains(msg, "resolve hostname"), strings.Contains(msg, "name resolution"), strings.Contains(msg, "name or service not known"):
			return "DNS failure"
		case strings.Contains(msg, "refused"):
			return "Conn refused"
		case strings.Contains(msg, "timed out"):
			return "Timed out"
		case strings.Contains(msg, "unreachable"), strings.Contains(msg, "no route to host"):
			return "Unreachable"
		case strings.Contains(msg, "connection closed"), strings.Contains(msg, "connection reset"):
			return "Disconnected"
		case strings.Contains(msg, "executable file not found"):
			return "SSH missing"
		default:
			return "SSH failed"
		}
	default:
		return "Failed"
	}
}

type hostRow struct {
	alias, status, models, age, loads string
	count                             int
	peakLoad, hottest                 *float64
	bestFree                          float64 // MiB; -1 marks unknown
	kind                              string
}

func summarizeHost(h hostState, now time.Time) hostRow {
	r := hostRow{alias: h.Alias, kind: h.Status, age: "–", models: "–", count: -1, bestFree: -1}
	if h.LastSuccess != nil {
		age := max(time.Duration(0), now.Sub(*h.LastSuccess))
		switch {
		case age < time.Minute:
			r.age = fmt.Sprintf("%ds", int(age.Seconds()))
		case age < time.Hour:
			r.age = fmt.Sprintf("%dm", int(age.Minutes()))
		case age < 48*time.Hour:
			r.age = fmt.Sprintf("%dh", int(age.Hours()))
		default:
			r.age = fmt.Sprintf("%dd", int(age.Hours()/24))
		}
	}
	switch h.Status {
	case "error":
		r.status = shortError(h.Error)
		if h.LastSuccess != nil {
			r.status += " (stale)"
		}
		return r
	case "pending":
		r.status = "Waiting"
		return r
	}
	if h.FreshUntil != nil && now.After(*h.FreshUntil) {
		r.status, r.kind = "Stale", "stale"
		return r
	}
	if h.Status == "no_gpu" {
		r.status, r.count, r.models = "No GPUs", 0, "0"
		return r
	}
	if h.Status != "ok" {
		r.status = "Unknown"
		return r
	}
	r.status = "Online"
	r.count = len(h.GPUs)
	models := make(map[string]int)
	gpus := slices.Clone(h.GPUs)
	sort.Slice(gpus, func(i, j int) bool { return gpus[i].Index < gpus[j].Index })
	var loads strings.Builder
	for _, g := range gpus {
		if g.UtilizationPct == nil {
			loads.WriteByte('?')
		} else {
			levels := [...]rune{'▁', '▂', '▃', '▄', '▅', '▆', '▇', '█'}
			level := int(math.Round(max(0, min(100, *g.UtilizationPct)) * 7 / 100))
			loads.WriteRune(levels[level])
		}
		name := strings.TrimPrefix(strings.TrimPrefix(g.Name, "NVIDIA "), "GeForce ")
		name = strings.TrimSpace(strings.TrimSuffix(name, " GPU"))
		if name == "" {
			name = "unknown"
		}
		models[name]++
		if g.UtilizationPct != nil && (r.peakLoad == nil || *g.UtilizationPct > *r.peakLoad) {
			r.peakLoad = g.UtilizationPct
		}
		if g.TemperatureC != nil && (r.hottest == nil || *g.TemperatureC > *r.hottest) {
			r.hottest = g.TemperatureC
		}
		if g.MemoryTotalMiB != nil && g.MemoryUsedMiB != nil && *g.MemoryTotalMiB >= *g.MemoryUsedMiB {
			r.bestFree = max(r.bestFree, *g.MemoryTotalMiB-*g.MemoryUsedMiB)
		}
	}
	r.loads = loads.String()
	switch len(models) {
	case 0:
		r.models = "0"
	case 1:
		for name, n := range models {
			r.models = fmt.Sprintf("%d × %s", n, name)
		}
	default:
		r.models = fmt.Sprintf("%d · mixed", r.count)
	}
	return r
}

type column struct {
	label   string
	width   int
	numeric bool
}

// Size the columns using the viewport alone so new samples never shift labels.
func hostColumns(width int) []column {
	statusWidth := 22
	if width < 70 {
		statusWidth = 14
	}
	if width < 36 {
		statusWidth = max(5, width/2)
	}
	cols := []column{{"HOST", 0, false}, {"STATUS", statusWidth, false}}
	if width >= 48 {
		gpuWidth := 4
		if width >= 72 {
			gpuWidth = min(29, 13+(width-72)/3)
		}
		cols = append(cols, column{"GPUs", gpuWidth, false})
	}
	if width >= 96 {
		cols = append(cols, column{"LOAD / PEAK", 15, true})
	} else if width >= 62 {
		cols = append(cols, column{"PEAK %", 6, true})
	}
	if width >= 40 {
		cols = append(cols, column{"MAX FREE", 9, true})
	}
	if width >= 104 {
		cols = append(cols, column{"MAX °C", 6, true})
	}
	if width >= 78 {
		cols = append(cols, column{"AGE", 5, true})
	}
	remaining := width - 2*(len(cols)-1)
	for _, c := range cols {
		remaining -= c.width
	}
	cols[0].width = max(1, remaining)
	return cols
}

func (r hostRow) cell(c column) string {
	switch c.label {
	case "HOST":
		// SSH aliases often share a rack prefix. Preserve the machine number.
		if n := uniseg.StringWidth(r.alias); n > c.width && c.width >= 6 {
			tailWidth := min(6, c.width/2)
			g := uniseg.NewGraphemes(r.alias)
			for g.Next() {
				n -= g.Width()
				if n <= tailWidth {
					_, end := g.Positions()
					return clipped(r.alias, c.width-n) + r.alias[end:]
				}
			}
		}
		return r.alias
	case "STATUS":
		if len(r.status) > c.width {
			// Metrics are already hidden for failed hosts. Keep the reason
			// readable when the optional freshness qualifier will not fit.
			return strings.TrimSuffix(r.status, " (stale)")
		}
		return r.status
	case "GPUs":
		if c.width > 4 {
			return r.models
		}
		if r.count >= 0 {
			return fmt.Sprint(r.count)
		}
	case "PEAK %":
		if r.peakLoad != nil {
			return fmt.Sprintf("%.0f", *r.peakLoad)
		}
	case "LOAD / PEAK":
		if r.loads != "" {
			peak := "–"
			if r.peakLoad != nil {
				peak = fmt.Sprintf("%.0f%%", *r.peakLoad)
			}
			return fmt.Sprintf("%s %4s", clipped(r.loads, c.width-5), peak)
		}
	case "MAX FREE":
		if r.bestFree >= 0 {
			return fmt.Sprintf("%.1f GiB", r.bestFree/1024)
		}
	case "MAX °C":
		if r.hottest != nil {
			return fmt.Sprintf("%.0f", *r.hottest)
		}
	case "AGE":
		return r.age
	}
	return "–"
}

func styled(text, style string, color bool) string {
	if !color || style == "" {
		return text
	}
	return "\x1b[" + style + "m" + text + "\x1b[0m"
}

func tableLine(cols []column, row *hostRow, color bool) string {
	var cells []string
	for _, c := range cols {
		text, style := c.label, "1;36"
		if row != nil {
			text, style = row.cell(c), ""
			switch c.label {
			case "HOST":
				style = "1"
			case "STATUS":
				switch row.kind {
				case "error":
					style = "31"
				case "pending", "stale":
					style = "33"
				case "ok":
					style = "32"
				}
			}
		}
		text = clipped(text, c.width)
		space := strings.Repeat(" ", c.width-uniseg.StringWidth(text))
		text = styled(text, style, color)
		if c.numeric {
			cells = append(cells, space+text)
		} else {
			cells = append(cells, text+space)
		}
	}
	return strings.TrimRight(strings.Join(cells, "  "), " ")
}

func hostOverview(s snapshot, order string, width, height int, live, color bool) []string {
	if width <= 0 {
		width = 100
	}
	width = min(width, 160)
	now := time.Now()
	rows := make([]hostRow, 0, len(s.Hosts))
	errors := 0
	for _, h := range s.Hosts {
		rows = append(rows, summarizeHost(h, now))
		if h.Status == "error" {
			errors++
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		if order == "free" && rows[i].bestFree != rows[j].bestFree {
			return rows[i].bestFree > rows[j].bestFree
		}
		return rows[i].alias < rows[j].alias
	})

	unit := "hosts"
	if len(rows) == 1 {
		unit = "host"
	}
	title := fmt.Sprintf("GPU Glint   %d %s", len(rows), unit)
	if errors > 0 {
		unit := "errors"
		if errors == 1 {
			unit = "error"
		}
		title += fmt.Sprintf(" · %d %s", errors, unit)
	}
	clock := "Updated " + s.Updated.Local().Format("15:04:05")
	space := width - uniseg.StringWidth(title) - len(clock)
	heading := styled(clipped(title, width), "1", color)
	if space >= 3 {
		heading += strings.Repeat(" ", space) + styled(clock, "2", color)
	}
	cols := hostColumns(width)
	header := tableLine(cols, nil, color)
	if width < 14 {
		header = styled(clipped("HOST  STATUS", width), "1", color)
	}
	lines := []string{heading, "", header, styled(strings.Repeat("─", width), "2", color)}

	var footer []string
	if errors > 0 {
		if s.IntervalSeconds == 0 {
			footer = append(footer, "Repeat this scan with --errors")
		} else {
			footer = append(footer, "Error details: gpuglint status --errors")
		}
	}
	if live {
		if len(footer) == 0 {
			footer = append(footer, "Ctrl+C quit")
		} else if len(footer[0])+14 <= width {
			footer[0] += "    Ctrl+C quit"
		}
	}
	show := len(rows)
	if live && height > 0 && len(lines)+show+len(footer) > height {
		// Spend vertical space on hosts before the blank line or separator.
		lines = []string{heading, header}
		if len(lines)+show+len(footer) > height {
			show = max(0, height-len(lines)-len(footer)-1)
			hiddenErrors := 0
			for _, r := range rows[show:] {
				if r.kind == "error" {
					hiddenErrors++
				}
			}
			hidden := fmt.Sprintf("+%d hosts", len(rows)-show)
			if hiddenErrors > 0 {
				hidden += fmt.Sprintf(" (%d failed)", hiddenErrors)
			}
			footer = append([]string{hidden + " · gpuglint status"}, footer...)
		}
	}
	for _, r := range rows[:show] {
		line := tableLine(cols, &r, color)
		if width < 14 {
			line = clipped(r.alias+" "+r.status, width)
		}
		lines = append(lines, line)
	}
	if len(footer) > 0 && (!live || height <= 0 || len(lines)+len(footer) < height) {
		lines = append(lines, "")
	}
	for _, f := range footer {
		lines = append(lines, styled(clipped(f, width), "2", color))
	}
	if live && height > 0 && len(lines) > height {
		lines = lines[:height]
	}
	return lines
}

func drawTable(out io.Writer, s snapshot, order string) error {
	width := 100
	if f, ok := out.(*os.File); ok {
		if w, _ := terminalSize(f); w > 0 {
			width = w
		}
	}
	lines := hostOverview(s, order, width, 0, false, false)
	_, err := fmt.Fprintln(out, strings.Join(lines, "\n"))
	return err
}
