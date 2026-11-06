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
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"strconv"
	"strings"
	"sync"
	"time"
	_ "time/tzdata" // `!ig timezone` must not depend on the image shipping zoneinfo

	"github.com/rs/zerolog"
	"go.mau.fi/util/dbutil"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/matrix"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/format"
	"maunium.net/go/mautrix/id"

	"go.mau.fi/mautrix-meta/pkg/metaid"
)

// This file implements the homestacks held messages: Matrix messages the
// bridge keeps back and sends to Instagram later, either because the user
// scheduled them (`!ig schedule`) or because they were sent during the chat's
// offline hours (`!ig offline-hours`).
//
// A held message is refused with a PENDING status before it is converted, and
// its (decrypted) event is stored. When it is due, the same event is queued
// back into the bridge, so the Instagram message maps to the Matrix event the
// user sent and replies, edits and deletes work afterwards as for any other
// message. Deleting a held message in Matrix cancels it: the bridge checks for
// a redaction before sending.

const (
	heldReasonSchedule = "schedule"
	heldReasonOffline  = "offline"
)

// heldReleaseInterval is how often due messages are looked for, so the
// worst-case lateness of a held message.
const heldReleaseInterval = 15 * time.Second

// Instagram's ban thresholds are unknown, so a large batch (a long offline
// window, or `schedule now all`) is not sent in one burst: each pass releases
// at most heldReleaseBatch messages, spaced heldReleaseGapMin to
// heldReleaseGapMax apart. The rest stay due and go out on the next pass, so
// 40 messages take about ten minutes instead of one burst.
const (
	heldReleaseBatch  = 4
	heldReleaseGapMin = 2 * time.Second
	heldReleaseGapMax = 4 * time.Second
)

// heldArmTTL is how long `!ig schedule <time>` without a message waits for
// the next message before forgetting it, so a forgotten command doesn't
// delay a message sent hours later.
const heldArmTTL = 10 * time.Minute

const heldReactionKey = "⏰"

var errMessageHeld = errors.New("message held to be sent later")

const heldVersionTable = "homestacks_heldmsg_version"

var heldUpgrades = dbutil.BuildUpgradeTable().
	WithRaw(0, 1, 0, "Add homestacks held messages", dbutil.TxnModeOn, func(ctx context.Context, db *dbutil.Database) error {
		_, err := db.Exec(ctx, `CREATE TABLE homestacks_held_message (
			bridge_id   TEXT   NOT NULL,
			event_id    TEXT   NOT NULL,
			room_id     TEXT   NOT NULL,
			login_id    TEXT   NOT NULL,
			reason      TEXT   NOT NULL,
			send_at     BIGINT NOT NULL,
			held_at     BIGINT NOT NULL,
			event_json  TEXT   NOT NULL,
			reaction_id TEXT   NOT NULL,
			PRIMARY KEY (bridge_id, event_id)
		)`)
		return err
	}).
	Finish()

type heldMessage struct {
	EventID    id.EventID
	RoomID     id.RoomID
	LoginID    networkid.UserLoginID
	Reason     string
	SendAt     time.Time
	HeldAt     time.Time
	Event      json.RawMessage
	ReactionID id.EventID
}

type heldMessageStore struct {
	db       *dbutil.Database
	bridgeID networkid.BridgeID
}

// heldStore is set by startHeldMessages; nil disables holding entirely.
var heldStore *heldMessageStore

const heldColumns = `event_id, room_id, login_id, reason, send_at, held_at, event_json, reaction_id`

func (s *heldMessageStore) insert(ctx context.Context, h *heldMessage) error {
	_, err := s.db.Exec(ctx, `
		INSERT INTO homestacks_held_message (bridge_id, `+heldColumns+`)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
	`, s.bridgeID, h.EventID, h.RoomID, h.LoginID, h.Reason, h.SendAt.UnixMilli(), h.HeldAt.UnixMilli(),
		string(h.Event), h.ReactionID)
	return err
}

func (s *heldMessageStore) delete(ctx context.Context, eventID id.EventID) error {
	_, err := s.take(ctx, eventID)
	return err
}

// take deletes a held message and reports whether it was still there, so the
// release loop and `!ig schedule now` can't both send it.
func (s *heldMessageStore) take(ctx context.Context, eventID id.EventID) (bool, error) {
	res, err := s.db.Exec(ctx, `DELETE FROM homestacks_held_message WHERE bridge_id=$1 AND event_id=$2`,
		s.bridgeID, eventID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

func (s *heldMessageStore) query(ctx context.Context, where string, args ...any) ([]*heldMessage, error) {
	rows, err := s.db.Query(ctx, `SELECT `+heldColumns+` FROM homestacks_held_message
		WHERE bridge_id=$1 AND `+where+` ORDER BY send_at, held_at`, append([]any{s.bridgeID}, args...)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*heldMessage
	for rows.Next() {
		var h heldMessage
		var sendAt, heldAt int64
		var evt string
		if err = rows.Scan(&h.EventID, &h.RoomID, &h.LoginID, &h.Reason, &sendAt, &heldAt, &evt, &h.ReactionID); err != nil {
			return nil, err
		}
		h.SendAt, h.HeldAt, h.Event = time.UnixMilli(sendAt), time.UnixMilli(heldAt), json.RawMessage(evt)
		out = append(out, &h)
	}
	return out, rows.Err()
}

// makeDue moves the given messages' send time to now, so the paced release
// loop picks them up.
func (s *heldMessageStore) makeDue(ctx context.Context, eventID id.EventID, now time.Time) error {
	_, err := s.db.Exec(ctx, `UPDATE homestacks_held_message SET send_at=$3 WHERE bridge_id=$1 AND event_id=$2 AND send_at>$3`,
		s.bridgeID, eventID, now.UnixMilli())
	return err
}

func (s *heldMessageStore) listDue(ctx context.Context, now time.Time) ([]*heldMessage, error) {
	return s.query(ctx, `send_at<=$2`, now.UnixMilli())
}

func (s *heldMessageStore) listRoom(ctx context.Context, roomID id.RoomID) ([]*heldMessage, error) {
	return s.query(ctx, `room_id=$2`, roomID)
}

// offlineBatchSendAt returns when the room's pending offline-hours messages
// go out, if there are any.
func (s *heldMessageStore) offlineBatchSendAt(ctx context.Context, roomID id.RoomID) (time.Time, bool, error) {
	var sendAt sql.NullInt64
	err := s.db.QueryRow(ctx, `SELECT MAX(send_at) FROM homestacks_held_message
		WHERE bridge_id=$1 AND room_id=$2 AND reason=$3`, s.bridgeID, roomID, heldReasonOffline).Scan(&sendAt)
	if err != nil || !sendAt.Valid {
		return time.Time{}, false, err
	}
	return time.UnixMilli(sendAt.Int64), true, nil
}

// heldState is the in-memory side: armed schedules and events on their way
// back through the bridge.
type heldState struct {
	lock      sync.Mutex
	armed     map[armKey]armedSchedule
	releasing map[id.EventID]time.Time
}

type armKey struct {
	Room id.RoomID
	User id.UserID
}

type armedSchedule struct {
	SendAt  time.Time
	Expires time.Time
}

var held = &heldState{
	armed:     make(map[armKey]armedSchedule),
	releasing: make(map[id.EventID]time.Time),
}

func (hs *heldState) arm(room id.RoomID, user id.UserID, sendAt, now time.Time) {
	hs.lock.Lock()
	defer hs.lock.Unlock()
	hs.armed[armKey{room, user}] = armedSchedule{SendAt: sendAt, Expires: now.Add(heldArmTTL)}
}

// takeArmed returns and forgets the schedule armed for the user's next message.
func (hs *heldState) takeArmed(room id.RoomID, user id.UserID, now time.Time) (time.Time, bool) {
	hs.lock.Lock()
	defer hs.lock.Unlock()
	a, ok := hs.armed[armKey{room, user}]
	delete(hs.armed, armKey{room, user})
	return a.SendAt, ok && now.Before(a.Expires)
}

func (hs *heldState) peekArmed(room id.RoomID, user id.UserID, now time.Time) (time.Time, bool) {
	hs.lock.Lock()
	defer hs.lock.Unlock()
	a, ok := hs.armed[armKey{room, user}]
	return a.SendAt, ok && now.Before(a.Expires)
}

func (hs *heldState) markReleasing(evtID id.EventID, now time.Time) {
	hs.lock.Lock()
	defer hs.lock.Unlock()
	// An event that never reached HandleMatrixMessage (e.g. its room is gone)
	// would otherwise stay here forever.
	for k, t := range hs.releasing {
		if now.Sub(t) > time.Hour {
			delete(hs.releasing, k)
		}
	}
	hs.releasing[evtID] = now
}

func (hs *heldState) takeReleasing(evtID id.EventID) bool {
	hs.lock.Lock()
	defer hs.lock.Unlock()
	_, ok := hs.releasing[evtID]
	delete(hs.releasing, evtID)
	return ok
}

// clockMinutes parses a time of day: 18:30, 1830, 8, 8am, 6:30pm, 12am.
func clockMinutes(s string) (int, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	invalid := fmt.Errorf("%q is not a time of day (try 18:30 or 6:30pm)", s)
	pm, am := strings.HasSuffix(s, "pm"), strings.HasSuffix(s, "am")
	if pm || am {
		s = strings.TrimSpace(s[:len(s)-2])
	}
	hs, ms, hasColon := strings.Cut(s, ":")
	if !hasColon {
		hs, ms = s, "0"
		if len(s) == 4 && !pm && !am {
			hs, ms = s[:2], s[2:]
		}
	} else if len(ms) != 2 {
		return 0, invalid
	}
	h, err1 := strconv.Atoi(hs)
	m, err2 := strconv.Atoi(ms)
	if err1 != nil || err2 != nil || hs == "" || m < 0 || m > 59 || h < 0 {
		return 0, invalid
	}
	if pm || am {
		if h < 1 || h > 12 {
			return 0, invalid
		}
		h %= 12
		if pm {
			h += 12
		}
	} else if h > 23 {
		return 0, invalid
	}
	return h*60 + m, nil
}

func formatClock(minutes int) string {
	return fmt.Sprintf("%02d:%02d", minutes/60, minutes%60)
}

// nextClock returns the first time at or after `after`'s next minute that
// reads `minutes` on the clock in after's location.
func nextClock(after time.Time, minutes int) time.Time {
	y, mo, d := after.Date()
	t := time.Date(y, mo, d, minutes/60, minutes%60, 0, 0, after.Location())
	if !t.After(after) {
		t = time.Date(y, mo, d+1, minutes/60, minutes%60, 0, 0, after.Location())
	}
	return t
}

// parseSendTime parses the time argument of `!ig schedule`: a time of day
// (the next time the clock reads it, so at most 24 hours away) or a delay
// prefixed with + (+45m, +2h30m), at most 24 hours.
func parseSendTime(s string, now time.Time) (time.Time, error) {
	if rest, ok := strings.CutPrefix(s, "+"); ok {
		d, err := time.ParseDuration(rest)
		if err != nil || d <= 0 {
			return time.Time{}, fmt.Errorf("%q is not a delay (try +45m or +2h30m)", s)
		} else if d > 24*time.Hour {
			return time.Time{}, errors.New("can only schedule up to 24 hours ahead")
		}
		return now.Add(d).Truncate(time.Second), nil
	}
	minutes, err := clockMinutes(s)
	if err != nil {
		return time.Time{}, err
	}
	return nextClock(now, minutes), nil
}

// parseOfflineHours parses `22:00-08:00` (any clockMinutes forms).
func parseOfflineHours(s string) (*metaid.OfflineHours, error) {
	start, end, ok := strings.Cut(s, "-")
	if !ok {
		return nil, errors.New("offline hours look like 22:00-08:00")
	}
	var oh metaid.OfflineHours
	var err error
	if oh.Start, err = clockMinutes(start); err != nil {
		return nil, err
	}
	if oh.End, err = clockMinutes(end); err != nil {
		return nil, err
	}
	if oh.Start == oh.End {
		return nil, errors.New("offline hours must not start and end at the same time")
	}
	return &oh, nil
}

// offlineUntil reports whether now is inside the window and, if so, when it
// ends. now must be in the user's location.
func offlineUntil(oh *metaid.OfflineHours, now time.Time) (time.Time, bool) {
	cur := now.Hour()*60 + now.Minute()
	var inside bool
	if oh.Start < oh.End {
		inside = cur >= oh.Start && cur < oh.End
	} else {
		inside = cur >= oh.Start || cur < oh.End
	}
	if !inside {
		return time.Time{}, false
	}
	return nextClock(now, oh.End), true
}

func describeOfflineHours(oh *metaid.OfflineHours) string {
	return formatClock(oh.Start) + "–" + formatClock(oh.End)
}

func formatSendAt(t, now time.Time) string {
	if y, m, d := t.Date(); y == now.Year() && m == now.Month() && d == now.Day() {
		return t.Format("15:04 MST")
	}
	return t.Format("Mon 15:04 MST")
}

// userLocation is the login's timezone (`!ig timezone`), else the bridge's.
func userLocation(login *bridgev2.UserLogin) *time.Location {
	if meta, ok := login.Metadata.(*metaid.UserLoginMetadata); ok {
		if loc, err := meta.LoadTimezone(); err == nil && loc != nil {
			return loc
		}
	}
	return time.Local
}

func portalOfflineHours(portal *bridgev2.Portal) *metaid.OfflineHours {
	meta, ok := portal.Metadata.(*metaid.PortalMetadata)
	if !ok {
		return nil
	}
	return meta.OfflineHours
}

// encodeHeldEvent keeps what is needed to queue the event again. The content
// is the original JSON where there is one, since the bridge edits the parsed
// copy while handling it.
func encodeHeldEvent(evt *event.Event) (json.RawMessage, error) {
	content := evt.Content.VeryRaw
	if content == nil {
		var err error
		if content, err = json.Marshal(evt.Content.Parsed); err != nil {
			return nil, err
		}
	}
	return json.Marshal(&event.Event{
		Type:      evt.Type,
		ID:        evt.ID,
		RoomID:    evt.RoomID,
		Sender:    evt.Sender,
		Timestamp: evt.Timestamp,
		Content:   event.Content{VeryRaw: content},
	})
}

func decodeHeldEvent(data json.RawMessage) (*event.Event, error) {
	var evt event.Event
	if err := json.Unmarshal(data, &evt); err != nil {
		return nil, err
	}
	evt.Type.Class = event.MessageEventType
	if err := evt.Content.ParseRaw(evt.Type); err != nil {
		return nil, err
	}
	return &evt, nil
}

// checkHold holds a Matrix message that is scheduled or sent during offline
// hours. Called from HandleMatrixMessage (Instagram has no polls); returns
// nil when the message should be sent now.
func (ic *IGClient) checkHold(ctx context.Context, msg *bridgev2.MatrixMessage) error {
	evt := msg.Event
	if heldStore == nil || evt == nil || msg.OrigSender != nil || held.takeReleasing(evt.ID) {
		return nil
	}
	now := time.Now().In(userLocation(ic.UserLogin))
	var reason string
	var sendAt time.Time
	if at, ok := held.takeArmed(msg.Portal.MXID, evt.Sender, now); ok {
		reason, sendAt = heldReasonSchedule, at
	} else {
		// Messages sent while earlier ones still wait for the end of offline
		// hours join them, even if the window has just ended, so the chat
		// never sees them out of order.
		batchAt, pending, err := heldStore.offlineBatchSendAt(ctx, msg.Portal.MXID)
		if err != nil {
			zerolog.Ctx(ctx).Err(err).Msg("Failed to check for held offline-hours messages")
		}
		sendAt = batchAt
		oh := portalOfflineHours(msg.Portal)
		if oh != nil {
			if until, inside := offlineUntil(oh, now); inside && until.After(sendAt) {
				sendAt = until
			}
		}
		if sendAt.IsZero() {
			return nil
		}
		reason = heldReasonOffline
		if !pending && oh != nil {
			// Once per batch, not once per message.
			defer ic.Main.sendHeldNotice(ctx, evt.RoomID, evt.ID, fmt.Sprintf(
				"%s Offline hours (%s): held until %s, along with anything else you send here before then. "+
					"Delete a message to cancel it, or `%s schedule now` to send them all now.",
				heldReactionKey, describeOfflineHours(oh), formatSendAt(sendAt, now), ic.Main.Bridge.Config.CommandPrefix))
		}
	}
	if err := ic.Main.holdEvent(ctx, ic.UserLogin.ID, evt, reason, sendAt); err != nil {
		zerolog.Ctx(ctx).Err(err).Msg("Failed to hold message")
		return bridgev2.WrapErrorInStatus(fmt.Errorf("it should have been held until %s, but storing it failed: %w",
			formatSendAt(sendAt, now), err)).WithErrorAsMessage().WithIsCertain(true).WithSendNotice(true)
	}
	zerolog.Ctx(ctx).Debug().Str("reason", reason).Time("send_at", sendAt).Msg("Held Matrix message")
	return bridgev2.WrapErrorInStatus(errMessageHeld).WithStatus(event.MessageStatusPending).
		WithIsCertain(true).WithSendNotice(false)
}

// holdEvent stores evt to be queued again at sendAt and marks it with a
// reaction, which is removed when it is sent.
func (wc *IGConnector) holdEvent(ctx context.Context, loginID networkid.UserLoginID, evt *event.Event, reason string, sendAt time.Time) error {
	data, err := encodeHeldEvent(evt)
	if err != nil {
		return err
	}
	h := &heldMessage{
		EventID: evt.ID, RoomID: evt.RoomID, LoginID: loginID, Reason: reason,
		SendAt: sendAt, HeldAt: time.Now(), Event: data,
	}
	resp, err := wc.Bridge.Bot.SendMessage(ctx, evt.RoomID, event.EventReaction, &event.Content{
		Parsed: &event.ReactionEventContent{RelatesTo: event.RelatesTo{
			Type: event.RelAnnotation, EventID: evt.ID, Key: heldReactionKey,
		}},
	}, nil)
	if err != nil {
		zerolog.Ctx(ctx).Warn().Err(err).Msg("Failed to mark held message with a reaction")
	} else {
		h.ReactionID = resp.EventID
	}
	if err = heldStore.insert(ctx, h); err != nil {
		wc.redactHeldReaction(ctx, h)
	}
	return err
}

func (wc *IGConnector) sendHeldNotice(ctx context.Context, roomID id.RoomID, replyTo id.EventID, text string) {
	content := format.RenderMarkdown(text, true, false)
	content.MsgType = event.MsgNotice
	if replyTo != "" {
		content.RelatesTo = (&event.RelatesTo{}).SetReplyTo(replyTo)
	}
	if _, err := wc.Bridge.Bot.SendMessage(ctx, roomID, event.EventMessage, &event.Content{Parsed: &content}, nil); err != nil {
		zerolog.Ctx(ctx).Warn().Err(err).Msg("Failed to send held message notice")
	}
}

func (wc *IGConnector) redactHeldReaction(ctx context.Context, h *heldMessage) {
	if h.ReactionID == "" {
		return
	}
	_, err := wc.Bridge.Bot.SendMessage(ctx, h.RoomID, event.EventRedaction, &event.Content{
		Parsed: &event.RedactionEventContent{Redacts: h.ReactionID},
	}, nil)
	if err != nil {
		zerolog.Ctx(ctx).Warn().Err(err).Msg("Failed to remove held message reaction")
	}
}

// heldEventRedacted asks the homeserver whether the user deleted the message
// while it was held. The raw event is fetched, since a redacted encrypted
// event can't be decrypted to find out.
func (wc *IGConnector) heldEventRedacted(ctx context.Context, h *heldMessage) (bool, error) {
	bot, ok := wc.Bridge.Bot.(*matrix.ASIntent)
	if !ok {
		return false, nil
	}
	evt, err := bot.Matrix.GetEvent(ctx, h.RoomID, h.EventID)
	if err != nil {
		return false, err
	}
	return evt.Unsigned.RedactedBecause != nil, nil
}

// heldLoginReady reports whether the message's login can send now. Messages
// of a login that no longer exists are dropped.
func (wc *IGConnector) heldLoginReady(ctx context.Context, h *heldMessage) (ready, gone bool) {
	login := wc.Bridge.GetCachedUserLoginByID(h.LoginID)
	if login == nil {
		var err error
		if login, err = wc.Bridge.GetExistingUserLoginByID(ctx, h.LoginID); err != nil {
			return false, false
		} else if login == nil {
			return false, true
		}
	}
	ic, ok := login.Client.(*IGClient)
	return ok && ic.heldReady(), false
}

// heldReady reports whether the login is authenticated and its socket is
// subscribed. IGClient.IsLoggedIn dereferences Client, which Disconnect sets
// to nil, so Client is read once here.
func (ic *IGClient) heldReady() bool {
	cli := ic.Client
	return cli != nil && cli.IsAuthenticated() && !ic.permanentErrored.Load() && cli.IsConnected()
}

// releaseHeld sends one held message now, unless it was deleted meanwhile.
func (wc *IGConnector) releaseHeld(ctx context.Context, h *heldMessage) {
	log := zerolog.Ctx(ctx).With().Stringer("event_id", h.EventID).Stringer("room_id", h.RoomID).Logger()
	ctx = log.WithContext(ctx)
	if redacted, err := wc.heldEventRedacted(ctx, h); err != nil {
		log.Warn().Err(err).Msg("Failed to check whether held message was deleted, sending anyway")
	} else if redacted {
		log.Debug().Msg("Held message was deleted, dropping it")
		if err = heldStore.delete(ctx, h.EventID); err != nil {
			log.Err(err).Msg("Failed to drop deleted held message")
		}
		wc.redactHeldReaction(ctx, h)
		return
	}
	evt, err := decodeHeldEvent(h.Event)
	if err != nil {
		log.Err(err).Msg("Failed to decode held message, dropping it")
		_ = heldStore.delete(ctx, h.EventID)
		wc.redactHeldReaction(ctx, h)
		wc.sendHeldNotice(ctx, h.RoomID, h.EventID, "⚠️ This held message could not be sent and was dropped.")
		return
	}
	// Delete before queueing: a crash in between loses the message rather
	// than sending it twice.
	if taken, err := heldStore.take(ctx, h.EventID); err != nil {
		log.Err(err).Msg("Failed to remove held message before sending, will retry")
		return
	} else if !taken {
		return
	}
	held.markReleasing(h.EventID, time.Now())
	log.Debug().Str("reason", h.Reason).Msg("Sending held message")
	wc.Bridge.QueueMatrixEvent(ctx, evt)
	wc.redactHeldReaction(ctx, h)
}

// releaseDue sends the held messages that are due, at most heldReleaseBatch
// of them, with a pause between each (see heldReleaseBatch). A room whose
// login is not connected is skipped as a whole so its messages stay in order.
func (wc *IGConnector) releaseDue(ctx context.Context, now time.Time) {
	due, err := heldStore.listDue(ctx, now)
	if err != nil {
		zerolog.Ctx(ctx).Err(err).Msg("Failed to list due held messages")
		return
	}
	blocked := make(map[id.RoomID]bool)
	sent := 0
	for _, h := range due {
		if blocked[h.RoomID] {
			continue
		}
		if ready, gone := wc.heldLoginReady(ctx, h); gone {
			_ = heldStore.delete(ctx, h.EventID)
			continue
		} else if !ready {
			blocked[h.RoomID] = true
			continue
		}
		if sent >= heldReleaseBatch {
			zerolog.Ctx(ctx).Debug().Int("remaining", len(due)).Msg("Held message batch limit reached, rest waits for the next pass")
			return
		}
		if sent > 0 {
			gap := heldReleaseGapMin + time.Duration(rand.Int64N(int64(heldReleaseGapMax-heldReleaseGapMin)))
			select {
			case <-ctx.Done():
				return
			case <-time.After(gap):
			}
		}
		wc.releaseHeld(ctx, h)
		sent++
	}
}

// startHeldMessages upgrades the held message table and starts the loop that
// sends them. Only a failed schema upgrade is returned as an error.
func (wc *IGConnector) startHeldMessages(ctx context.Context) error {
	log := wc.Bridge.Log.With().Str("db_section", "homestacks_heldmsg").Logger()
	db := wc.Bridge.DB.Child(heldVersionTable, heldUpgrades, dbutil.ZeroLogger(log))
	if err := db.Upgrade(ctx); err != nil {
		return err
	}
	heldStore = &heldMessageStore{db: db, bridgeID: wc.Bridge.ID}
	if wc.Bridge.Background {
		return nil
	}
	go func() {
		ctx := log.WithContext(wc.Bridge.BackgroundCtx)
		ticker := time.NewTicker(heldReleaseInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				wc.releaseDue(ctx, time.Now())
			}
		}
	}()
	return nil
}
