// mautrix-meta - A Matrix-Facebook Messenger and Instagram DM puppeting bridge.
// Copyright (C) 2026 Tulir Asokan
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <https://www.gnu.org/licenses/>.

package igconnector

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"go.mau.fi/mautrix-meta/pkg/messagix/httpclient"
)

func TestIsTransientConnectError(t *testing.T) {
	// The chain the 2026-11-08 outage produced: instameow wraps makeRequest's
	// give-up error, which wraps the last HTTP status error.
	gaveUp503 := fmt.Errorf("failed to load inbox: %w",
		fmt.Errorf("%w: %w", httpclient.ErrMaxRetriesReached,
			fmt.Errorf("%w %d", httpclient.ErrUnexpectedError, 503)))
	gaveUpNetwork := fmt.Errorf("failed to load inbox: %w",
		fmt.Errorf("%w: %w", httpclient.ErrMaxRetriesReached,
			fmt.Errorf("%w: dial tcp: connection refused", httpclient.ErrRequestFailed)))
	gaveUp429 := fmt.Errorf("%w: %w", httpclient.ErrMaxRetriesReached, httpclient.ErrRateLimited)
	// makeRequest returns 4xx errors (other than 429) straight away, unwrapped.
	notRetried404 := fmt.Errorf("failed to load inbox: %w",
		fmt.Errorf("%w %d", httpclient.ErrUnexpectedError, 404))

	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"503 after retries", gaveUp503, true},
		{"network error after retries", gaveUpNetwork, true},
		{"429 after retries", gaveUp429, true},
		{"404 not retried", notRetried404, false},
		{"token invalidated", fmt.Errorf("failed to load inbox: %w", httpclient.ErrTokenInvalidated), false},
		{"challenge", httpclient.RedirectedError{Type: httpclient.ErrChallengeRequired}, false},
		{"cancelled", context.Canceled, false},
		{"plain error", errors.New("failed to parse inbox"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isTransientConnectError(tt.err); got != tt.want {
				t.Errorf("isTransientConnectError(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

func TestRetryTransientConnectErrorShortCircuits(t *testing.T) {
	// Neither case touches the login, so a bare client is enough.
	ic := &IGClient{}
	transient := fmt.Errorf("%w: boom", httpclient.ErrMaxRetriesReached)

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if !ic.retryTransientConnectError(cancelled, context.Background(), transient, 0) {
		t.Error("a cancelled attempt should be treated as handled, not reported as UNKNOWN_ERROR")
	}
	if ic.retryTransientConnectError(context.Background(), context.Background(), errors.New("not transient"), 0) {
		t.Error("a non-transient error should fall through to upstream's UNKNOWN_ERROR")
	}
}

func TestScheduleTransientRetryIsCancelledByDisconnect(t *testing.T) {
	ic := &IGClient{}
	var oldCancelled bool
	oldCancel := context.CancelFunc(func() { oldCancelled = true })
	ic.stopPeriodicReconnect.Store(&oldCancel)

	ic.scheduleTransientRetry(context.Background())
	if !oldCancelled {
		t.Error("scheduling should cancel the previous periodic reconnect")
	}
	if ic.stopPeriodicReconnect.Load() == nil {
		t.Fatal("scheduled retry should be stored in stopPeriodicReconnect")
	}
	// Disconnect calls this; it must clear the pending retry.
	ic.cancelPeriodicReconnect()
	if ic.stopPeriodicReconnect.Load() != nil {
		t.Error("cancelPeriodicReconnect should clear the scheduled retry")
	}
}
