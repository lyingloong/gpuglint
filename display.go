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
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/lyingloong/gpuglint/internal/collect"
	"github.com/lyingloong/gpuglint/internal/state"
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
	if d.interactive {
		b.WriteString("\x1b[H\x1b[J")
	}
	if err := drawTable(&b, s, order); err != nil {
		return err
	}
	if d.interactive {
		b.WriteString("\nCtrl+C to close\n")
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

type row struct {
	host  *hostState
	gpu   *collect.GPU
	stale bool
}

func (r row) free() float64 {
	if r.stale || r.gpu == nil || r.gpu.MemoryUsedMiB == nil || r.gpu.MemoryTotalMiB == nil {
		return -1
	}
	return *r.gpu.MemoryTotalMiB - *r.gpu.MemoryUsedMiB
}

func clipped(s string, limit int) string {
	r := []rune(strings.Map(func(c rune) rune {
		if !unicode.IsPrint(c) {
			return ' '
		}
		return c
	}, s))
	if len(r) > limit {
		return string(r[:limit-1]) + "…"
	}
	return string(r)
}

func metric(p *float64, divisor float64, decimals int) string {
	if p == nil {
		return "–"
	}
	return fmt.Sprintf("%.*f", decimals, *p/divisor)
}

func utilization(v *float64) string {
	if v == nil {
		return "–"
	}
	n := int(math.Round(max(0, min(100, *v)) / 20))
	return fmt.Sprintf("%s%s %3.0f%%", strings.Repeat("▰", n), strings.Repeat("▱", 5-n), *v)
}

func drawTable(out io.Writer, s snapshot, order string) error {
	var rows []row
	now := time.Now()
	count := 0
	for i := range s.Hosts {
		h := &s.Hosts[i]
		stale := h.Status == "error" || (h.FreshUntil != nil && now.After(*h.FreshUntil))
		if len(h.GPUs) == 0 {
			rows = append(rows, row{h, nil, stale})
		}
		for j := range h.GPUs {
			rows = append(rows, row{h, &h.GPUs[j], stale})
			count++
		}
	}
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if order == "free" && a.free() != b.free() {
			return a.free() > b.free()
		}
		if a.host.Alias != b.host.Alias {
			return a.host.Alias < b.host.Alias
		}
		return a.gpu != nil && b.gpu != nil && a.gpu.Index < b.gpu.Index
	})
	var b strings.Builder
	fmt.Fprintf(&b, "GPU Glint   %d hosts · %d GPUs   |   %s\n\n", len(s.Hosts), count, s.Updated.Local().Format("15:04:05"))
	const columns = "%-15s %-19s %-10s %13s %4s %7s  %s\n"
	fmt.Fprintf(&b, columns, "HOST", "GPU", "LOAD", "USED/TOT GiB", "°C", "AGE", "STATUS")
	for _, r := range rows {
		h := r.host
		gpu, load, mem, temp, age := "–", "–", "–", "–", "–"
		if g := r.gpu; g != nil {
			name := strings.TrimPrefix(g.Name, "NVIDIA ")
			name = strings.TrimPrefix(name, "GeForce ")
			gpu = fmt.Sprintf("%d %s", g.Index, strings.TrimSuffix(name, " GPU"))
			load = utilization(g.UtilizationPct)
			mem = metric(g.MemoryUsedMiB, 1024, 1) + "/" + metric(g.MemoryTotalMiB, 1024, 1)
			temp = metric(g.TemperatureC, 1, 0)
		}
		if h.LastSuccess != nil {
			age = max(time.Duration(0), now.Sub(*h.LastSuccess)).Truncate(time.Second).String()
		}
		label := h.Status
		if h.Status == "ok" {
			label = h.Backend
		}
		if r.stale && h.LastSuccess != nil {
			label += " (stale)"
		}
		alias := h.Alias
		if chars := []rune(alias); len(chars) > 15 {
			alias = string(chars[:8]) + "…" + string(chars[len(chars)-6:])
		}
		fmt.Fprintf(&b, columns, clipped(alias, 15), clipped(gpu, 19), load, mem, temp, age, label)
	}
	for _, h := range s.Hosts {
		if h.Error != nil {
			fmt.Fprintf(&b, "\n%s · %s: %s\n", clipped(h.Alias, 64), clipped(h.Error.Code, 40), clipped(h.Error.Message, 240))
		}
	}
	_, err := io.WriteString(out, b.String())
	return err
}
