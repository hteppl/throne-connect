package logbuf

import (
	"errors"
	"log/slog"
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
