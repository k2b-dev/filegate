package cli

import (
	"context"
	"errors"
	"testing"

	indexpebble "github.com/k2b-dev/filegate/v3/infra/pebble"
)

func TestIsDetectorTerminalError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil", err: nil, want: false},
		{name: "context canceled", err: context.Canceled, want: true},
		{name: "index closed", err: indexpebble.ErrIndexClosed, want: true},
		{name: "index unavailable", err: indexpebble.ErrIndexUnavailable, want: true},
		{name: "wrapped closed", err: errors.New("wrapped: pebble: closed"), want: true},
		{name: "generic", err: errors.New("other"), want: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := isDetectorTerminalError(tc.err)
			if got != tc.want {
				t.Fatalf("isDetectorTerminalError(%v)=%v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
