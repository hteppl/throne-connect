// Package logbuf keeps the most recent log records in memory so the TUI can show
// them, both in-process and through the node's HTTP API.
package logbuf

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Line is one formatted log record.
type Line struct {
	Seq   uint64    `json:"seq"`
	Time  time.Time `json:"time"`
	Level string    `json:"level"`
	Text  string    `json:"text"` // message followed by key=value attributes
}

// Buffer is a fixed-size ring of log lines. It is safe for concurrent use.
type Buffer struct {
	mu    sync.Mutex
	lines []Line
	size  int
	next  uint64
}

func New(size int) *Buffer {
	return &Buffer{size: size, next: 1}
}

func (b *Buffer) add(l Line) {
	b.mu.Lock()
	defer b.mu.Unlock()
	l.Seq = b.next
	b.next++
	b.lines = append(b.lines, l)
	if len(b.lines) > b.size {
		b.lines = append(b.lines[:0], b.lines[len(b.lines)-b.size:]...)
	}
}

// Since returns the lines with Seq > after and the Seq to pass next time.
func (b *Buffer) Since(after uint64) ([]Line, uint64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []Line
	for _, l := range b.lines {
		if l.Seq > after {
			out = append(out, l)
		}
	}
	return out, b.next - 1
}

// Handler returns a slog.Handler that records into the buffer at the given level.
func (b *Buffer) Handler(level slog.Leveler) slog.Handler {
	return &handler{buf: b, level: level}
}

type handler struct {
	buf    *Buffer
	level  slog.Leveler
	attrs  string
	prefix string
}

func (h *handler) Enabled(_ context.Context, l slog.Level) bool { return l >= h.level.Level() }

func (h *handler) Handle(_ context.Context, r slog.Record) error {
	var sb strings.Builder
	sb.WriteString(r.Message)
	sb.WriteString(h.attrs)
	r.Attrs(func(a slog.Attr) bool {
		writeAttr(&sb, h.prefix, a)
		return true
	})
	h.buf.add(Line{Time: r.Time, Level: r.Level.String(), Text: sb.String()})
	return nil
}

func (h *handler) WithAttrs(as []slog.Attr) slog.Handler {
	var sb strings.Builder
	for _, a := range as {
		writeAttr(&sb, h.prefix, a)
	}
	c := *h
	c.attrs += sb.String()
	return &c
}

func (h *handler) WithGroup(name string) slog.Handler {
	c := *h
	c.prefix += name + "."
	return &c
}

func writeAttr(sb *strings.Builder, prefix string, a slog.Attr) {
	a.Value = a.Value.Resolve()
	if a.Equal(slog.Attr{}) {
		return
	}
	if a.Value.Kind() == slog.KindGroup {
		for _, g := range a.Value.Group() {
			writeAttr(sb, prefix+a.Key+".", g)
		}
		return
	}
	v := fmt.Sprint(a.Value.Any())
	if v == "" || strings.ContainsAny(v, " \t\"=") {
		v = strconv.Quote(v)
	}
	fmt.Fprintf(sb, " %s%s=%s", prefix, a.Key, v)
}
