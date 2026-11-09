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

// Keep retrying the connection after transient failures (homestacks fork).
//
// Upstream's connectWithRetry only retries Meta lserror responses. Any other
// error that is not a known account problem (an HTTP 5xx or 429 from
// instagram.com/direct/inbox, a network error) sends UNKNOWN_ERROR and returns
// without retrying and without rescheduling the periodic reconnect, so the
// login stays disconnected until the bridge is restarted. That happened when
// the 20h periodic reconnect met a burst of 503s.
//
// The HTTP client already decides what is transient: it retries those
// failures itself and wraps the last one in ErrMaxRetriesReached when it gives
// up. Here such an error gets the same treatment as a retryable lserror:
// TRANSIENT_DISCONNECT and another connectWithRetry attempt with upstream's
// exponential backoff, up to MaxConnectRetries. Once those run out the caller
// still sends UNKNOWN_ERROR, but a full reconnect is scheduled for
// transientRetryLaterDelay later, so the login never stays down for good.

package igconnector

import (
	"context"
	"errors"
	"time"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/bridgev2/status"

	"go.mau.fi/mautrix-meta/pkg/messagix/httpclient"
)

// transientRetryLaterDelay is how long to wait after MaxConnectRetries quick
// attempts (about 34 minutes of backoff) before starting another round.
const transientRetryLaterDelay = 30 * time.Minute

// isTransientConnectError reports whether err is a failure the HTTP client
// retried and gave up on, as opposed to one it refused to retry (4xx, client
// errors, cancellation).
func isTransientConnectError(err error) bool {
	return errors.Is(err, httpclient.ErrMaxRetriesReached)
}

// retryTransientConnectError handles a failed index load in connectWithRetry.
// It returns true if it dealt with err (by retrying, or because the attempt was
// cancelled), and false if the caller should send its UNKNOWN_ERROR state.
func (ic *IGClient) retryTransientConnectError(retryCtx, ctx context.Context, err error, attempts int) bool {
	if retryCtx.Err() != nil {
		return true
	} else if !isTransientConnectError(err) {
		return false
	}
	if attempts < MaxConnectRetries {
		ic.UserLogin.BridgeState.Send(status.BridgeState{
			StateEvent: status.StateTransientDisconnect,
			Error:      MetaConnectError,
			Info: map[string]any{
				"go_error": err.Error(),
			},
		})
		ic.connectWithRetry(retryCtx, ctx, attempts+1)
		return true
	}
	ic.scheduleTransientRetry(ctx)
	return false
}

// scheduleTransientRetry runs a full reconnect after transientRetryLaterDelay.
// It shares stopPeriodicReconnect with the periodic reconnect, so Disconnect
// (including bridgev2's own unknown-error reconnect) cancels it, and a
// successful connection replaces it with the normal periodic reconnect.
func (ic *IGClient) scheduleTransientRetry(ctx context.Context) {
	ctx, cancel := context.WithCancel(ctx)
	if oldCancel := ic.stopPeriodicReconnect.Swap(&cancel); oldCancel != nil {
		(*oldCancel)()
	}
	zerolog.Ctx(ctx).Warn().
		Int("attempts", MaxConnectRetries).
		Stringer("retry_in", transientRetryLaterDelay).
		Msg("Connection retries exhausted, scheduling another full reconnect")
	go func() {
		defer cancel()
		select {
		case <-time.After(transientRetryLaterDelay):
			ic.UserLogin.Log.Info().Msg("Doing full reconnect after transient connection failures")
			ic.FullReconnect(false, true)
		case <-ctx.Done():
		}
	}()
}
