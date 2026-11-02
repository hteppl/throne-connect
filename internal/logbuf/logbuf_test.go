package logbuf

import (
	"errors"
	"log/slog"
	"sync"
	"testing"
)

func TestBuffer(t *testing.T) {
	b := New(3)
	log := slog.New(b.Handler(slog.LevelInfo)).With("node", "a")
	log.Debug("hidden")
	log.Info("started", "listen", "127.0.0.1:10808")
	log.Warn("check failed", "err", errors.New("i/o timeout"))

	lines, next := b.Since(0)
	if len(lines) != 2 || next != 2 {
		t.Fatalf("got %d lines, next %d; want 2, 2", len(lines), next)
	}
	if got, want := lines[0].Text, "started node=a listen=127.0.0.1:10808"; got != want {
		t.Errorf("text %q, want %q", got, want)
	}
	if got, want := lines[1].Text, `check failed node=a err="i/o timeout"`; got != want {
		t.Errorf("text %q, want %q", got, want)
	}
	if lines[1].Level != "WARN" {
		t.Errorf("level %q", lines[1].Level)
	}

	for range 5 {
		log.Info("x")
	}
	lines, next = b.Since(2)
	if len(lines) != 3 || lines[0].Seq != 5 || next != 7 {
		t.Fatalf("ring: got %d lines from seq %d, next %d; want 3 from 5, next 7", len(lines), lines[0].Seq, next)
	}
	if lines, _ := b.Since(next); len(lines) != 0 {
		t.Fatalf("nothing new expected, got %d", len(lines))
	}
}

func TestHandlerFormatting(t *testing.T) {
	tests := []struct {
		name string
		log  func(*slog.Logger)
		want string
	}{
		{"group prefixes record attrs", func(l *slog.Logger) { l.WithGroup("srv").Info("up", "port", 1080) }, "up srv.port=1080"},
		{"nested groups", func(l *slog.Logger) { l.WithGroup("a").WithGroup("b").Info("m", "k", "v") }, "m a.b.k=v"},
		{"attrs bound before a group stay unprefixed", func(l *slog.Logger) { l.With("x", 1).WithGroup("g").With("y", 2).Info("m", "z", 3) }, "m x=1 g.y=2 g.z=3"},
		{"group attr value", func(l *slog.Logger) { l.Info("m", slog.Group("req", "id", 7, slog.Group("peer", "ip", "1.2.3.4"))) }, "m req.id=7 req.peer.ip=1.2.3.4"},
		{"empty group is dropped", func(l *slog.Logger) { l.Info("m", slog.Group("empty")) }, "m"},
		{"empty attr is dropped", func(l *slog.Logger) { l.Info("m", slog.Attr{}, "k", "v") }, "m k=v"},
		{"empty string is quoted", func(l *slog.Logger) { l.Info("m", "k", "") }, `m k=""`},
		{"special characters are quoted", func(l *slog.Logger) { l.Info("m", "a", "x=y", "b", "t\tab", "c", `q"`) }, `m a="x=y" b="t\tab" c="q\""`},
		{"LogValuer is resolved", func(l *slog.Logger) { l.Info("m", "s", secret("pw")) }, "m s=***"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := New(10)
			tt.log(slog.New(b.Handler(slog.LevelInfo)))
			lines, _ := b.Since(0)
			if len(lines) != 1 {
				t.Fatalf("got %d lines, want 1", len(lines))
			}
			if lines[0].Text != tt.want {
				t.Errorf("text %q, want %q", lines[0].Text, tt.want)
			}
		})
	}
}

type secret string

func (secret) LogValue() slog.Value { return slog.StringValue("***") }

func TestHandlerLevelAndTime(t *testing.T) {
	var lv slog.LevelVar
	lv.Set(slog.LevelWarn)
	b := New(10)
	log := slog.New(b.Handler(&lv))
	log.Info("dropped")
	log.Error("kept")
	lv.Set(slog.LevelDebug)
	log.Debug("now visible")

	lines, next := b.Since(0)
	if len(lines) != 2 || next != 2 {
		t.Fatalf("got %d lines, next %d; want 2, 2", len(lines), next)
	}
	if lines[0].Level != "ERROR" || lines[1].Level != "DEBUG" {
		t.Errorf("levels %q, %q", lines[0].Level, lines[1].Level)
	}
	if lines[0].Time.IsZero() || lines[0].Seq != 1 || lines[1].Seq != 2 {
		t.Errorf("unexpected line metadata: %+v", lines)
	}
}

func TestBufferEmptyAndConcurrent(t *testing.T) {
	b := New(50)
	if lines, next := b.Since(0); lines != nil || next != 0 {
		t.Fatalf("empty buffer: got %v, next %d", lines, next)
	}
	log := slog.New(b.Handler(slog.LevelInfo))
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 100 {
				log.Info("x")
				b.Since(0)
			}
		}()
	}
	wg.Wait()
	lines, next := b.Since(0)
	if len(lines) != 50 || next != 800 {
		t.Fatalf("got %d lines, next %d; want 50, 800", len(lines), next)
	}
	for i := 1; i < len(lines); i++ {
		if lines[i].Seq != lines[i-1].Seq+1 {
			t.Fatalf("sequence gap at %d: %d after %d", i, lines[i].Seq, lines[i-1].Seq)
		}
	}
	if lines[len(lines)-1].Seq != 800 {
		t.Fatalf("last seq %d, want 800", lines[len(lines)-1].Seq)
	}
}
