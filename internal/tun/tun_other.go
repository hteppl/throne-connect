//go:build !linux

package tun

import (
	"context"
	"log/slog"
)

// Start is not implemented outside Linux.
func Start(_ context.Context, _ Options, _ *slog.Logger) (Tunnel, error) {
	return nil, ErrUnsupported
}
