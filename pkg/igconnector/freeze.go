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
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"
	"go.mau.fi/util/dbutil"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"

	"go.mau.fi/mautrix-meta/pkg/instameow/slidetypes"
	"go.mau.fi/mautrix-meta/pkg/msgconv/mediadl"
)

// This file implements the homestacks frozen chats (`!ig freeze`): Instagram
// events that would create, edit, delete or react to a message in a frozen
// chat are not bridged to Matrix but stored, and replayed through the normal
// delta handler when the chat is unfrozen, so they arrive with their original
// timestamps and edits, reactions and deletes made meanwhile still apply.
// Matrix messages sent into a frozen chat are refused.
//
// What is stored is the raw delta JSON (slidetypes.Delta keeps it and
// marshals back to it), keyed by a digest of that JSON. Replay unmarshals it
// again and calls handleDelta, not the socket's event handler, so sequence IDs
// and the saved socket position are not touched.
//
// Two things would otherwise let a frozen message reach Matrix, and both are
// handled here: a resnapshot makes the bridge reload the mailbox, whose chat
// resyncs forward-backfill from the server (see frozenBackfillCheck), and the
// media URLs Instagram sends expire within days, so a long freeze would
// replay messages whose attachments can no longer be downloaded (see
// refreshExpiredMedia).

const freezeVersionTable = "homestacks_freeze_version"

var freezeUpgrades = dbutil.BuildUpgradeTable().
	WithRaw(0, 1, 0, "Add homestacks frozen chats", dbutil.TxnModeOn, func(ctx context.Context, db *dbutil.Database) error {
		if _, err := db.Exec(ctx, `CREATE TABLE homestacks_frozen_chat (
			bridge_id       TEXT   NOT NULL,
			portal_id       TEXT   NOT NULL,
			portal_receiver TEXT   NOT NULL,
			frozen_at       BIGINT NOT NULL,
			PRIMARY KEY (bridge_id, portal_id, portal_receiver)
		)`); err != nil {
			return err
		}
		_, err := db.Exec(ctx, `CREATE TABLE homestacks_frozen_event (
			bridge_id       TEXT   NOT NULL,
			portal_id       TEXT   NOT NULL,
			portal_receiver TEXT   NOT NULL,
			login_id        TEXT   NOT NULL,
			digest          TEXT   NOT NULL,
			seq             BIGINT NOT NULL,
			uq_seq_id       BIGINT NOT NULL,
			event_json      TEXT   NOT NULL,
			PRIMARY KEY (bridge_id, portal_id, portal_receiver, login_id, digest)
		)`)
		return err
	}).
	Finish()

type freezeReplayKey struct{}

type frozenRow struct {
	LoginID networkid.UserLoginID
	// Digest identifies the delta by its content. Instagram's own sequence
	// ID (UQSeqID) is kept for debugging but is not the key: the bridge resets
	// its copy of it on a resnapshot, so a repeat of the same number after
	// one is not proven to be the same delta.
	Digest  string
	Seq     int64
	UQSeqID int64
	Event   json.RawMessage
}

func digestOf(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

type freezeStore struct {
	db       *dbutil.Database
	bridgeID networkid.BridgeID
}

func (s *freezeStore) loadChats(ctx context.Context) (map[networkid.PortalKey]time.Time, error) {
	rows, err := s.db.Query(ctx, `SELECT portal_id, portal_receiver, frozen_at FROM homestacks_frozen_chat WHERE bridge_id=$1`, s.bridgeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[networkid.PortalKey]time.Time)
	for rows.Next() {
		var key networkid.PortalKey
		var at int64
		if err = rows.Scan(&key.ID, &key.Receiver, &at); err != nil {
			return nil, err
		}
		out[key] = time.UnixMilli(at)
	}
	return out, rows.Err()
}

func (s *freezeStore) putChat(ctx context.Context, key networkid.PortalKey, at time.Time) error {
	_, err := s.db.Exec(ctx, `INSERT INTO homestacks_frozen_chat (bridge_id, portal_id, portal_receiver, frozen_at)
		VALUES ($1, $2, $3, $4) ON CONFLICT DO NOTHING`, s.bridgeID, key.ID, key.Receiver, at.UnixMilli())
	return err
}

func (s *freezeStore) deleteChat(ctx context.Context, key networkid.PortalKey) error {
	_, err := s.db.Exec(ctx, `DELETE FROM homestacks_frozen_chat WHERE bridge_id=$1 AND portal_id=$2 AND portal_receiver=$3`,
		s.bridgeID, key.ID, key.Receiver)
	return err
}

// putEvent stores a held delta. A redelivery of one already held (the socket
// resends from its saved position after a reconnect) is ignored.
func (s *freezeStore) putEvent(ctx context.Context, key networkid.PortalKey, r *frozenRow) error {
	_, err := s.db.Exec(ctx, `INSERT INTO homestacks_frozen_event
		(bridge_id, portal_id, portal_receiver, login_id, digest, seq, uq_seq_id, event_json)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8) ON CONFLICT DO NOTHING`,
		s.bridgeID, key.ID, key.Receiver, r.LoginID, r.Digest, r.Seq, r.UQSeqID, string(r.Event))
	return err
}

func (s *freezeStore) deleteEvent(ctx context.Context, key networkid.PortalKey, r *frozenRow) error {
	_, err := s.db.Exec(ctx, `DELETE FROM homestacks_frozen_event
		WHERE bridge_id=$1 AND portal_id=$2 AND portal_receiver=$3 AND login_id=$4 AND digest=$5`,
		s.bridgeID, key.ID, key.Receiver, r.LoginID, r.Digest)
	return err
}

func (s *freezeStore) listEvents(ctx context.Context, key networkid.PortalKey) ([]*frozenRow, error) {
	rows, err := s.db.Query(ctx, `SELECT login_id, digest, seq, uq_seq_id, event_json FROM homestacks_frozen_event
		WHERE bridge_id=$1 AND portal_id=$2 AND portal_receiver=$3 ORDER BY seq`, s.bridgeID, key.ID, key.Receiver)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*frozenRow
	for rows.Next() {
		var r frozenRow
		var evt string
		if err = rows.Scan(&r.LoginID, &r.Digest, &r.Seq, &r.UQSeqID, &evt); err != nil {
			return nil, err
		}
		r.Event = json.RawMessage(evt)
		out = append(out, &r)
	}
	return out, rows.Err()
}

func (s *freezeStore) countEvents(ctx context.Context, key networkid.PortalKey) (n int, err error) {
	err = s.db.QueryRow(ctx, `SELECT COUNT(*) FROM homestacks_frozen_event
		WHERE bridge_id=$1 AND portal_id=$2 AND portal_receiver=$3`, s.bridgeID, key.ID, key.Receiver).Scan(&n)
	return
}

// freezeState is the in-memory copy of the frozen chat list, so the check on
// every incoming delta doesn't touch the database. lock is only held for
// single operations, never across a replay, so a long unfreeze doesn't stall
// other chats; the chat stays frozen while it is replayed and is unfrozen by
// the locked check that finds nothing left.
type freezeState struct {
	lock    sync.Mutex
	store   *freezeStore
	chats   map[networkid.PortalKey]time.Time
	thawing map[networkid.PortalKey]bool
	lastSeq int64
}

// frozen is set up by startFreeze; a nil store disables freezing entirely.
var frozen = &freezeState{
	chats:   make(map[networkid.PortalKey]time.Time),
	thawing: make(map[networkid.PortalKey]bool),
}

func (fs *freezeState) since(key networkid.PortalKey) (time.Time, bool) {
	fs.lock.Lock()
	defer fs.lock.Unlock()
	at, ok := fs.chats[key]
	return at, ok
}

func (fs *freezeState) list() map[networkid.PortalKey]time.Time {
	fs.lock.Lock()
	defer fs.lock.Unlock()
	out := make(map[networkid.PortalKey]time.Time, len(fs.chats))
	for k, v := range fs.chats {
		out[k] = v
	}
	return out
}

// nextSeq orders held deltas by arrival, even several in one nanosecond.
// Callers hold lock.
func (fs *freezeState) nextSeq(now time.Time) int64 {
	seq := now.UnixNano()
	if seq <= fs.lastSeq {
		seq = fs.lastSeq + 1
	}
	fs.lastSeq = seq
	return seq
}

func (fs *freezeState) freeze(ctx context.Context, key networkid.PortalKey, now time.Time) (since time.Time, already bool, err error) {
	fs.lock.Lock()
	defer fs.lock.Unlock()
	if at, ok := fs.chats[key]; ok {
		return at, true, nil
	}
	if err = fs.store.putChat(ctx, key, now); err != nil {
		return time.Time{}, false, err
	}
	fs.chats[key] = now
	return now, false, nil
}

// freezesDelta reports whether a delta changes what a chat's message history
// looks like in Matrix. Receipts, typing, mute, pins, names and the like are
// not frozen.
func freezesDelta(data slidetypes.DeltaEvent) bool {
	switch data.(type) {
	case *slidetypes.NewMessageEvent, *slidetypes.AdminMessageEvent, *slidetypes.EditMessageEvent,
		*slidetypes.CreateReactionEvent, *slidetypes.DeleteReactionEvent, *slidetypes.DeleteMessageEvent:
		return true
	}
	return false
}

func encodeFrozenDelta(d *slidetypes.Delta) (json.RawMessage, error) {
	if len(d.Raw) == 0 {
		return nil, errors.New("delta has no raw JSON")
	}
	return bytes.Clone(d.Raw), nil
}

func decodeFrozenDelta(data json.RawMessage) (*slidetypes.Delta, error) {
	var d slidetypes.Delta
	if err := json.Unmarshal(data, &d); err != nil {
		return nil, err
	}
	return &d, nil
}

// checkFrozen holds a delta whose chat is frozen. Called from handleDelta
// once the portal is known; returns true if the delta was held and must not
// be bridged now.
func (ic *IGClient) checkFrozen(ctx context.Context, key networkid.PortalKey, d *slidetypes.Delta) bool {
	if ctx.Value(freezeReplayKey{}) != nil || !freezesDelta(d.Data) {
		return false
	}
	frozen.lock.Lock()
	defer frozen.lock.Unlock()
	if _, ok := frozen.chats[key]; !ok || frozen.store == nil {
		return false
	}
	log := zerolog.Ctx(ctx).With().Int64("uq_seq_id", d.UQSeqID).Str("portal_id", string(key.ID)).Logger()
	data, err := encodeFrozenDelta(d)
	if err == nil {
		err = frozen.store.putEvent(ctx, key, &frozenRow{
			LoginID: ic.UserLogin.ID, Digest: digestOf(data), Seq: frozen.nextSeq(time.Now()), UQSeqID: d.UQSeqID, Event: data,
		})
	}
	if err != nil {
		// Fail open: bridging a message early beats losing it.
		log.Err(err).Msg("Failed to hold event in frozen chat, bridging it now")
		return false
	}
	log.Debug().Str("delta_type", d.TypeName).Msg("Held event in frozen chat")
	return true
}

// frozenBackfillCheck is the CheckNeedsBackfillFunc of every chat resync. A
// resnapshot makes the bridge drop its socket position and reload the
// mailbox, whose resyncs forward-backfill anything newer than the last
// bridged message from the server, which for a frozen chat is exactly what is
// being held back. So a frozen chat never backfills; unfreeze catches up
// instead. Otherwise this is the library's default comparison.
func frozenBackfillCheck(key networkid.PortalKey, latestTS time.Time) func(context.Context, *database.Message) (bool, error) {
	return func(ctx context.Context, latest *database.Message) (bool, error) {
		if _, isFrozen := frozen.since(key); isFrozen {
			zerolog.Ctx(ctx).Debug().Msg("Not backfilling frozen chat on resync")
			return false, nil
		}
		if latest == nil {
			return !latestTS.IsZero(), nil
		}
		return latestTS.After(latest.Timestamp), nil
	}
}

// frozenPortal is the backstop for frozenBackfillCheck: whatever else asks
// for a forward backfill of a frozen chat gets nothing.
func frozenPortal(key networkid.PortalKey) bool {
	_, isFrozen := frozen.since(key)
	return isFrozen
}

// checkFrozenSend refuses a Matrix message sent into a frozen chat.
func (ic *IGClient) checkFrozenSend(portal *bridgev2.Portal) error {
	since, ok := frozen.since(portal.PortalKey)
	if !ok {
		return nil
	}
	return bridgev2.WrapErrorInStatus(fmt.Errorf("this chat is frozen (since %s); `%s unfreeze` it first",
		since.In(userLocation(ic.UserLogin)).Format("2006-01-02 15:04 MST"), ic.Main.Bridge.Config.CommandPrefix)).
		WithErrorAsMessage().WithIsCertain(true).WithSendNotice(true)
}

var errFrozenLoginOffline = errors.New("not connected")

// frozenClient returns the connected client of the login a delta was held
// for. A login that no longer exists returns nil and no error, so its
// events are dropped.
func (ic *IGConnector) frozenClient(ctx context.Context, loginID networkid.UserLoginID) (*IGClient, error) {
	login := ic.Bridge.GetCachedUserLoginByID(loginID)
	if login == nil {
		var err error
		if login, err = ic.Bridge.GetExistingUserLoginByID(ctx, loginID); err != nil {
			return nil, err
		} else if login == nil {
			return nil, nil
		}
	}
	client, ok := login.Client.(*IGClient)
	if !ok || !client.heldReady() {
		return nil, fmt.Errorf("the Instagram account %s is %w", login.RemoteName, errFrozenLoginOffline)
	}
	return client, nil
}

var errUnfreezeRunning = errors.New("an unfreeze of this chat is already running")

// nextBatch returns the chat's held events, or, if there are none left,
// unfreezes it and reports done. Doing both under the lock means an event
// can't slip in between the last replay and the unfreeze.
func (fs *freezeState) nextBatch(ctx context.Context, key networkid.PortalKey) (rows []*frozenRow, done bool, err error) {
	fs.lock.Lock()
	defer fs.lock.Unlock()
	if _, ok := fs.chats[key]; !ok {
		return nil, true, nil
	}
	if rows, err = fs.store.listEvents(ctx, key); err != nil || len(rows) > 0 {
		return rows, false, err
	}
	if err = fs.store.deleteChat(ctx, key); err != nil {
		return nil, false, err
	}
	delete(fs.chats, key)
	return nil, true, nil
}

func (fs *freezeState) startThaw(key networkid.PortalKey) bool {
	fs.lock.Lock()
	defer fs.lock.Unlock()
	if fs.thawing[key] {
		return false
	}
	fs.thawing[key] = true
	return true
}

func (fs *freezeState) endThaw(key networkid.PortalKey) {
	fs.lock.Lock()
	defer fs.lock.Unlock()
	delete(fs.thawing, key)
}

// freezeMediaMargin is how close to expiry a media URL may be and still be
// replayed as is: bridging happens asynchronously after the replay call.
const freezeMediaMargin = 15 * time.Minute

// Looking for a message to refresh walks the thread from the newest message
// down, 20 per page; this caps how far back that goes (500 messages).
const freezeRefreshMaxPages = 25

// mediaURLsExpired reports whether any media URL in the message content of a
// raw delta has expired (or will within margin). Only the message content is
// looked at: profile pictures and the like expire too, and say nothing about
// whether the attachments can be downloaded.
func mediaURLsExpired(raw json.RawMessage, now time.Time, margin time.Duration) bool {
	var d struct {
		Message *struct {
			Content json.RawMessage `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(raw, &d) != nil || d.Message == nil || len(d.Message.Content) == 0 {
		return false
	}
	var content any
	if json.Unmarshal(d.Message.Content, &content) != nil {
		return false
	}
	return anyURLExpired(content, now, margin)
}

func anyURLExpired(v any, now time.Time, margin time.Duration) bool {
	switch val := v.(type) {
	case string:
		if !strings.HasPrefix(val, "http") {
			return false
		}
		exp := mediadl.ParseExpiryFromURL(val)
		return !exp.IsZero() && exp.Before(now.Add(margin))
	case []any:
		for _, item := range val {
			if anyURLExpired(item, now, margin) {
				return true
			}
		}
	case map[string]any:
		for _, item := range val {
			if anyURLExpired(item, now, margin) {
				return true
			}
		}
	}
	return false
}

// deltaMessage returns the message a replayed delta carries, if it is one that
// has media.
func deltaMessage(d *slidetypes.Delta) **slidetypes.Message {
	switch evt := d.Data.(type) {
	case *slidetypes.NewMessageEvent:
		return &evt.Message
	case *slidetypes.AdminMessageEvent:
		return &evt.Message
	}
	return nil
}

// fetchFreshMessages looks the wanted messages up again on Instagram, which
// returns them with new media URLs. It walks the thread from the newest
// message until all are found, the page is older than the oldest wanted
// message, or freezeRefreshMaxPages is reached.
func (ic *IGClient) fetchFreshMessages(ctx context.Context, portal *bridgev2.Portal, want map[string]time.Time) (map[string]*slidetypes.Message, error) {
	meta, err := ic.ensureIGID(ctx, portal)
	if err != nil {
		return nil, err
	}
	var oldest time.Time
	for _, ts := range want {
		if oldest.IsZero() || ts.Before(oldest) {
			oldest = ts
		}
	}
	found := make(map[string]*slidetypes.Message, len(want))
	resp, err := ic.Client.GetThread(ctx, slidetypes.MakeGetThreadInfoRequest(meta.IGID))
	if err != nil {
		return nil, fmt.Errorf("failed to get thread: %w", err)
	} else if resp.ThreadInfo.AsIGDirectThread == nil {
		return nil, errors.New("no thread info received")
	}
	page := resp.ThreadInfo.AsIGDirectThread.SlideMessages
	for i := 0; ; i++ {
		reachedOldest := false
		for _, edge := range page.Edges {
			if _, ok := want[edge.Node.ID]; ok {
				found[edge.Node.ID] = edge.Node
			}
			if edge.Node.TimestampMS.Before(oldest) {
				reachedOldest = true
			}
		}
		if len(found) == len(want) || reachedOldest || !page.PageInfo.HasNextPage || i >= freezeRefreshMaxPages {
			return found, nil
		}
		cursor := page.PageInfo.EndCursor
		next, err := ic.Client.PaginateMessages(ctx, &slidetypes.PaginateMessagesRequest{
			AfterCursor: &cursor,
			ThreadID:    meta.IGID,
			FirstN:      20,

			InitialMessagePageCount: 20,
		})
		if err != nil {
			return found, fmt.Errorf("failed to paginate messages: %w", err)
		}
		page = next.ThreadInfo.AsIGDirectThread.Messages
	}
}

// refreshExpiredMedia replaces, in place, the messages of the given decoded
// deltas whose media URLs have expired with freshly fetched copies. It returns
// the IDs it could not refresh (not found any more, or still expired), so the
// caller can say so; those are bridged as they are and will show Instagram's
// usual "failed to transfer attachment" notice. View-once media that was
// already opened, or any message unsent meanwhile, is simply gone.
func (ic *IGClient) refreshExpiredMedia(ctx context.Context, portal *bridgev2.Portal, deltas []*slidetypes.Delta, now time.Time) (failed []string, err error) {
	stale := make(map[string]*slidetypes.Delta)
	want := make(map[string]time.Time)
	for _, d := range deltas {
		mp := deltaMessage(d)
		if mp == nil || *mp == nil || !mediaURLsExpired(d.Raw, now, freezeMediaMargin) {
			continue
		}
		stale[(*mp).ID] = d
		want[(*mp).ID] = (*mp).TimestampMS.Time
	}
	if len(stale) == 0 {
		return nil, nil
	}
	fresh, err := ic.fetchFreshMessages(ctx, portal, want)
	for id, d := range stale {
		msg := fresh[id]
		if msg == nil {
			failed = append(failed, id)
			continue
		}
		raw, marshalErr := json.Marshal(msg)
		if marshalErr == nil && mediaURLsExpired(wrapMessageForCheck(raw), now, freezeMediaMargin) {
			failed = append(failed, id)
			continue
		}
		*deltaMessage(d) = msg
	}
	return failed, err
}

// wrapMessageForCheck wraps a message the way a delta carries it.
func wrapMessageForCheck(msg json.RawMessage) json.RawMessage {
	out, _ := json.Marshal(map[string]json.RawMessage{"message": msg})
	return out
}

// unfreeze replays the chat's held events in order and then unfreezes it.
// Events arriving meanwhile are held too and replayed in the next batch. If
// one can't be replayed, the chat stays frozen with it and everything after
// it still held, and the error says why. portal is needed to refresh media;
// catchUp is the login used afterwards to fetch anything the socket never
// delivered during the freeze.
func (ic *IGConnector) unfreeze(ctx context.Context, key networkid.PortalKey, portal *bridgev2.Portal, catchUp *IGClient) (replayed, dropped int, notes []string, err error) {
	if !frozen.startThaw(key) {
		return 0, 0, nil, errUnfreezeRunning
	}
	defer frozen.endThaw(key)
	log := zerolog.Ctx(ctx).With().Str("portal_id", string(key.ID)).Logger()
	for {
		rows, done, err := frozen.nextBatch(ctx, key)
		if err != nil {
			return replayed, dropped, notes, err
		} else if done {
			log.Info().Int("replayed", replayed).Int("dropped", dropped).Msg("Unfroze chat")
			if note := ic.catchUpAfterUnfreeze(ctx, portal, catchUp); note != "" {
				notes = append(notes, note)
			}
			return replayed, dropped, notes, nil
		}
		type replay struct {
			row    *frozenRow
			client *IGClient
			delta  *slidetypes.Delta
		}
		var batch []replay
		byClient := make(map[*IGClient][]*slidetypes.Delta)
		for _, r := range rows {
			client, err := ic.frozenClient(ctx, r.LoginID)
			if err != nil {
				return replayed, dropped, notes, err
			}
			delta, decodeErr := decodeFrozenDelta(r.Event)
			if client == nil || decodeErr != nil {
				log.Warn().Err(decodeErr).Int64("uq_seq_id", r.UQSeqID).Msg("Dropping held event of frozen chat")
				if err = frozen.store.deleteEvent(ctx, key, r); err != nil {
					return replayed, dropped, notes, err
				}
				dropped++
				continue
			}
			batch = append(batch, replay{r, client, delta})
			byClient[client] = append(byClient[client], delta)
		}
		if portal != nil {
			for client, deltas := range byClient {
				failed, refreshErr := client.refreshExpiredMedia(ctx, portal, deltas, time.Now())
				if refreshErr != nil {
					log.Warn().Err(refreshErr).Msg("Failed to refresh expired media of frozen messages")
				}
				if len(failed) > 0 {
					log.Warn().Strs("message_ids", failed).Msg("Could not refresh expired media of frozen messages")
					notes = append(notes, fmt.Sprintf("%d message(s) had media links that expired during the freeze and could not be fetched again "+
						"(unsent, view-once already opened, or too far back); they are bridged without the media and show Instagram's usual failed-attachment notice. "+
						"%s", len(failed), freezeRefreshErrNote(refreshErr)))
				}
			}
		}
		for _, item := range batch {
			replayCtx := item.client.UserLogin.Log.WithContext(context.WithValue(ic.Bridge.BackgroundCtx, freezeReplayKey{}, true))
			if err = item.client.handleDelta(replayCtx, item.delta); err != nil {
				return replayed, dropped, notes, fmt.Errorf("event %d could not be bridged: %w", item.row.UQSeqID, err)
			}
			if err = frozen.store.deleteEvent(ctx, key, item.row); err != nil {
				return replayed, dropped, notes, err
			}
			replayed++
		}
	}
}

func freezeRefreshErrNote(err error) string {
	if err == nil {
		return ""
	}
	return "Lookup error: " + err.Error()
}

// catchUpAfterUnfreeze fetches the chat again so anything that never came
// through the socket while it was frozen (a resnapshot discards the socket
// position, and frozen chats skip the backfill that would have found those
// messages) is bridged now. The normal resync comparison decides whether
// there is anything newer than what was just replayed.
func (ic *IGConnector) catchUpAfterUnfreeze(ctx context.Context, portal *bridgev2.Portal, client *IGClient) string {
	if portal == nil || client == nil {
		return ""
	}
	meta, err := client.ensureIGID(ctx, portal)
	if err != nil {
		return fmt.Sprintf("Could not check Instagram for messages missed during the freeze: %v", err)
	}
	if _, err = client.getAndResyncThread(ctx, meta.IGID, "unfreeze catch-up"); err != nil {
		return fmt.Sprintf("Could not check Instagram for messages missed during the freeze: %v", err)
	}
	return ""
}

// startFreeze upgrades the frozen chat tables and loads the frozen chats.
func (ic *IGConnector) startFreeze(ctx context.Context) error {
	log := ic.Bridge.Log.With().Str("db_section", "homestacks_freeze").Logger()
	db := ic.Bridge.DB.Child(freezeVersionTable, freezeUpgrades, dbutil.ZeroLogger(log))
	if err := db.Upgrade(ctx); err != nil {
		return err
	}
	store := &freezeStore{db: db, bridgeID: ic.Bridge.ID}
	chats, err := store.loadChats(ctx)
	if err != nil {
		return err
	}
	frozen.lock.Lock()
	defer frozen.lock.Unlock()
	frozen.store, frozen.chats = store, chats
	return nil
}
