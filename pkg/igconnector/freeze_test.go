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
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"go.mau.fi/util/dbutil"
	_ "go.mau.fi/util/dbutil/litestream"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"

	"go.mau.fi/mautrix-meta/pkg/instameow/slidetypes"
)

func imageDelta(seq int, msgID, url, profileURL string) string {
	return fmt.Sprintf(`{"__typename":"SlideUQPPNewMessage","uq_seq_id":"%d","thread_fbid":"340282366841710300949128",
	"message":{"id":"%s","message_id":"mid.$%s","sender_fbid":"111","timestamp_ms":"1761390000000",
	"sender":{"id":"111","user_dict":{"profile_pic_url":"%s"}},
	"content":{"__typename":"SlideMessageImageContent","attachments":[{"attachment_fbid":"9","attachment_cdn_url":"%s"}]}}}`,
		seq, msgID, msgID, profileURL, url)
}

func reactionDelta(seq int, msgID string) string {
	return fmt.Sprintf(`{"__typename":"SlideUQPPCreateReaction","uq_seq_id":"%d","thread_fbid":"340282366841710300949128",
	"message_id":"%s","reaction":{"log_message_id":"log1","reaction":"❤️","sender_fbid":"111","reaction_timestamp_ms":"1761390001000"}}`, seq, msgID)
}

func readDelta(seq int) string {
	return fmt.Sprintf(`{"__typename":"SlideUQPPMarkRead","uq_seq_id":"%d","thread_fbid":"340282366841710300949128","read_timestamp_ms":"1761390001000"}`, seq)
}

func mustDelta(t *testing.T, raw string) *slidetypes.Delta {
	t.Helper()
	var d slidetypes.Delta
	if err := json.Unmarshal([]byte(raw), &d); err != nil {
		t.Fatalf("unmarshal delta: %v", err)
	}
	return &d
}

func TestFrozenDeltaRoundTrip(t *testing.T) {
	raw := reactionDelta(77, "m1")
	d := mustDelta(t, raw)
	data, err := encodeFrozenDelta(d)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != raw {
		t.Errorf("stored JSON differs from the delta as received:\n got %s\nwant %s", data, raw)
	}
	got, err := decodeFrozenDelta(data)
	if err != nil {
		t.Fatal(err)
	}
	react, ok := got.Data.(*slidetypes.CreateReactionEvent)
	if !ok || got.UQSeqID != 77 || got.ThreadIGID != "340282366841710300949128" || react.MessageID != "m1" || react.Reaction.Reaction != "❤️" {
		t.Errorf("decoded delta lost data: %+v %+v", got, got.Data)
	}
	if string(got.Raw) != raw {
		t.Error("decoded delta did not keep its raw JSON, so it could not be stored again")
	}
	if _, err = encodeFrozenDelta(&slidetypes.Delta{}); err == nil {
		t.Error("a delta without raw JSON was accepted")
	}
}

func TestFreezesDelta(t *testing.T) {
	for raw, want := range map[string]bool{
		imageDelta(1, "m1", "https://cdn/x.jpg", ""): true,
		reactionDelta(2, "m1"):                       true,
		readDelta(3):                                 false,
	} {
		if got := freezesDelta(mustDelta(t, raw).Data); got != want {
			t.Errorf("freezesDelta(%.40s...) = %v, want %v", raw, got, want)
		}
	}
}

func TestMediaURLsExpired(t *testing.T) {
	now := time.Unix(1762000000, 0)
	url := func(exp time.Time) string {
		return fmt.Sprintf("https://scontent.cdninstagram.com/v/t.jpg?oe=%X&oh=abc", exp.Unix())
	}
	stale, fresh := url(now.Add(-time.Hour)), url(now.Add(24*time.Hour))
	soon := url(now.Add(5 * time.Minute))
	cases := []struct {
		name string
		raw  string
		want bool
	}{
		{"expired attachment", imageDelta(1, "m", stale, fresh), true},
		{"fresh attachment", imageDelta(1, "m", fresh, fresh), false},
		{"expires inside margin", imageDelta(1, "m", soon, fresh), true},
		{"only the profile picture expired", imageDelta(1, "m", fresh, stale), false},
		{"url without expiry", imageDelta(1, "m", "https://cdn/x.jpg", stale), false},
		{"no message", reactionDelta(1, "m"), false},
	}
	for _, c := range cases {
		if got := mediaURLsExpired([]byte(c.raw), now, freezeMediaMargin); got != c.want {
			t.Errorf("%s: mediaURLsExpired = %v, want %v", c.name, got, c.want)
		}
	}
}

// A refreshed message is re-checked after being marshaled again, which only
// works if marshaling keeps the attachment URLs.
func TestRefreshedMessageKeepsMediaURLs(t *testing.T) {
	now := time.Unix(1762000000, 0)
	fresh := fmt.Sprintf("https://scontent.cdninstagram.com/v/t.jpg?oe=%X", now.Add(24*time.Hour).Unix())
	stale := fmt.Sprintf("https://scontent.cdninstagram.com/v/t.jpg?oe=%X", now.Add(-time.Hour).Unix())
	for url, want := range map[string]bool{fresh: false, stale: true} {
		d := mustDelta(t, imageDelta(1, "m1", url, ""))
		msg := d.Data.(*slidetypes.NewMessageEvent).Message
		raw, err := json.Marshal(msg)
		if err != nil {
			t.Fatal(err)
		}
		if got := mediaURLsExpired(wrapMessageForCheck(raw), now, freezeMediaMargin); got != want {
			t.Errorf("after re-marshal, expired(%s) = %v, want %v (%s)", url, got, want, raw)
		}
	}
	if mp := deltaMessage(mustDelta(t, reactionDelta(1, "m"))); mp != nil {
		t.Error("a reaction has no message to refresh")
	}
	d := mustDelta(t, imageDelta(1, "m1", fresh, ""))
	if mp := deltaMessage(d); mp == nil || (*mp).ID != "m1" {
		t.Error("deltaMessage did not find the message")
	}
}

func newFreezeTestState(t *testing.T) *freezeState {
	t.Helper()
	raw, err := dbutil.NewWithDialect("file:"+filepath.Join(t.TempDir(), "freeze.db")+"?_txlock=immediate", "sqlite3-fk-wal")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	db := raw.Child(freezeVersionTable, freezeUpgrades, nil)
	if err = db.Upgrade(context.Background()); err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	return &freezeState{
		store:   &freezeStore{db: db, bridgeID: "ig"},
		chats:   make(map[networkid.PortalKey]time.Time),
		thawing: make(map[networkid.PortalKey]bool),
	}
}

func TestFreezeNextSeqIsStrictlyIncreasing(t *testing.T) {
	fs := &freezeState{}
	now := time.Unix(1761390000, 0)
	a, b := fs.nextSeq(now), fs.nextSeq(now)
	if b <= a {
		t.Errorf("same instant: %d then %d", a, b)
	}
	if c := fs.nextSeq(now.Add(-time.Second)); c <= b {
		t.Errorf("clock went back: %d after %d", c, b)
	}
}

func TestFreezeStoreRoundTrip(t *testing.T) {
	ctx := context.Background()
	fs := newFreezeTestState(t)
	s := fs.store
	other := &freezeStore{db: s.db, bridgeID: "other-bridge"}
	dm := networkid.PortalKey{ID: "thread-1", Receiver: "login1"}
	group := networkid.PortalKey{ID: "thread-2"}
	now := time.UnixMilli(time.Now().UnixMilli())

	for _, key := range []networkid.PortalKey{dm, group} {
		if err := s.putChat(ctx, key, now); err != nil {
			t.Fatalf("putChat: %v", err)
		}
	}
	if err := s.putChat(ctx, dm, now.Add(time.Hour)); err != nil {
		t.Fatalf("putChat again: %v", err)
	}
	chats, err := s.loadChats(ctx)
	if err != nil || len(chats) != 2 || !chats[dm].Equal(now) {
		t.Fatalf("loadChats = %v, %v; want both, dm at its first freeze", chats, err)
	}
	if theirs, _ := other.loadChats(ctx); len(theirs) != 0 {
		t.Errorf("other bridge sees %v", theirs)
	}

	row := func(raw string, seq, uq int64) *frozenRow {
		return &frozenRow{LoginID: "login1", Digest: digestOf([]byte(raw)), Seq: seq, UQSeqID: uq, Event: []byte(raw)}
	}
	// Two different events that carry the same Instagram sequence ID, as can
	// happen after a resnapshot, must both be kept; the same event delivered
	// again after a reconnect must not be.
	rows := []*frozenRow{
		row(`{"n":2}`, 2, 500),
		row(`{"n":1}`, 1, 500),
		row(`{"n":2}`, 3, 500), // redelivery of the first row
	}
	for _, r := range rows {
		if err = s.putEvent(ctx, dm, r); err != nil {
			t.Fatalf("putEvent: %v", err)
		}
	}
	got, err := s.listEvents(ctx, dm)
	if err != nil || len(got) != 2 || string(got[0].Event) != `{"n":1}` || string(got[1].Event) != `{"n":2}` || got[1].Seq != 2 {
		t.Fatalf("listEvents = %+v, %v; want n=1 then n=2 (arrival order), redelivery ignored", got, err)
	}
	if n, _ := s.countEvents(ctx, group); n != 0 {
		t.Errorf("group has %d events", n)
	}
	if err = s.deleteEvent(ctx, dm, got[0]); err != nil {
		t.Fatalf("deleteEvent: %v", err)
	}
	if n, _ := s.countEvents(ctx, dm); n != 1 {
		t.Errorf("after delete: %d events", n)
	}
	if err = s.deleteChat(ctx, dm); err != nil {
		t.Fatalf("deleteChat: %v", err)
	}
	if chats, _ = s.loadChats(ctx); len(chats) != 1 {
		t.Errorf("after deleteChat: %v", chats)
	}
}

func TestFreezeNextBatchUnfreezesWhenEmpty(t *testing.T) {
	ctx := context.Background()
	fs := newFreezeTestState(t)
	key := networkid.PortalKey{ID: "thread-1", Receiver: "login1"}
	if _, already, err := fs.freeze(ctx, key, time.Now()); err != nil || already {
		t.Fatalf("freeze = %v, %v", already, err)
	}
	row := &frozenRow{LoginID: "login1", Digest: "a", Seq: 1, Event: []byte(`{}`)}
	if err := fs.store.putEvent(ctx, key, row); err != nil {
		t.Fatalf("putEvent: %v", err)
	}
	rows, done, err := fs.nextBatch(ctx, key)
	if err != nil || done || len(rows) != 1 {
		t.Fatalf("first batch = %d rows, done %v, %v; want the held row", len(rows), done, err)
	}
	if !fs.startThaw(key) || fs.startThaw(key) {
		t.Error("second concurrent thaw was allowed")
	}
	fs.endThaw(key)
	if err = fs.store.deleteEvent(ctx, key, row); err != nil {
		t.Fatalf("deleteEvent: %v", err)
	}
	if _, done, err = fs.nextBatch(ctx, key); err != nil || !done {
		t.Fatalf("empty batch: done %v, %v", done, err)
	}
	if _, ok := fs.since(key); ok {
		t.Error("chat still frozen in memory")
	}
	if chats, _ := fs.store.loadChats(ctx); len(chats) != 0 {
		t.Errorf("chat still frozen in DB: %v", chats)
	}
}

// withFrozenState swaps the package's frozen state for a test one.
func withFrozenState(t *testing.T) *freezeState {
	t.Helper()
	orig := frozen
	frozen = newFreezeTestState(t)
	t.Cleanup(func() { frozen = orig })
	return frozen
}

func TestCheckFrozen(t *testing.T) {
	ctx := context.Background()
	fs := withFrozenState(t)
	ic := &IGClient{UserLogin: &bridgev2.UserLogin{UserLogin: &database.UserLogin{ID: "login1"}}}
	key := networkid.PortalKey{ID: "thread-1", Receiver: "login1"}
	msg := mustDelta(t, imageDelta(10, "m1", "https://cdn/x.jpg", ""))

	if ic.checkFrozen(ctx, key, msg) {
		t.Fatal("held an event of a chat that isn't frozen")
	}
	if _, _, err := fs.freeze(ctx, key, time.Now()); err != nil {
		t.Fatal(err)
	}
	if !ic.checkFrozen(ctx, key, msg) {
		t.Fatal("did not hold a message of a frozen chat")
	}
	if ic.checkFrozen(ctx, networkid.PortalKey{ID: "thread-2", Receiver: "login1"}, msg) {
		t.Error("held a message of another chat")
	}
	if ic.checkFrozen(ctx, key, mustDelta(t, readDelta(11))) {
		t.Error("held a read receipt")
	}
	if ic.checkFrozen(context.WithValue(ctx, freezeReplayKey{}, true), key, msg) {
		t.Error("held a replayed event again")
	}
	// The socket delivering the same event again after a reconnect.
	if !ic.checkFrozen(ctx, key, mustDelta(t, imageDelta(10, "m1", "https://cdn/x.jpg", ""))) {
		t.Error("did not hold the redelivery")
	}
	if !ic.checkFrozen(ctx, key, mustDelta(t, reactionDelta(12, "m1"))) {
		t.Error("did not hold a reaction")
	}
	rows, err := fs.store.listEvents(ctx, key)
	if err != nil || len(rows) != 2 || rows[0].UQSeqID != 10 || rows[1].UQSeqID != 12 {
		t.Fatalf("held rows = %+v, %v; want the message then the reaction, once each", rows, err)
	}
	// What was stored decodes back to the same events, ready to replay.
	for i, want := range []string{"*slidetypes.NewMessageEvent", "*slidetypes.CreateReactionEvent"} {
		d, err := decodeFrozenDelta(rows[i].Event)
		if err != nil || fmt.Sprintf("%T", d.Data) != want {
			t.Errorf("row %d decodes to %T, %v; want %s", i, d.Data, err, want)
		}
	}
}

func TestFrozenBackfillCheck(t *testing.T) {
	ctx := context.Background()
	fs := withFrozenState(t)
	key := networkid.PortalKey{ID: "thread-1", Receiver: "login1"}
	last := time.Unix(1761390000, 0)
	latest := &database.Message{Timestamp: last}
	newer, older := last.Add(time.Minute), last.Add(-time.Minute)

	cases := []struct {
		name   string
		check  func(context.Context, *database.Message) (bool, error)
		latest *database.Message
		want   bool
	}{
		{"unfrozen, newer activity", frozenBackfillCheck(key, newer), latest, true},
		{"unfrozen, nothing newer", frozenBackfillCheck(key, older), latest, false},
		{"unfrozen, empty portal", frozenBackfillCheck(key, newer), nil, true},
		{"unfrozen, empty portal and no activity", frozenBackfillCheck(key, time.Time{}), nil, false},
	}
	for _, c := range cases {
		if got, err := c.check(ctx, c.latest); err != nil || got != c.want {
			t.Errorf("%s: = %v, %v; want %v", c.name, got, err, c.want)
		}
	}
	if _, _, err := fs.freeze(ctx, key, time.Now()); err != nil {
		t.Fatal(err)
	}
	for _, check := range []func(context.Context, *database.Message) (bool, error){
		frozenBackfillCheck(key, newer), frozenBackfillCheck(key, time.Time{}),
	} {
		if got, err := check(ctx, latest); err != nil || got {
			t.Errorf("frozen chat would backfill: %v, %v", got, err)
		}
	}
	if got, _ := frozenBackfillCheck(networkid.PortalKey{ID: "other"}, newer)(ctx, latest); !got {
		t.Error("freezing one chat stopped another chat's backfill")
	}
	if !frozenPortal(key) || frozenPortal(networkid.PortalKey{ID: "other"}) {
		t.Error("frozenPortal disagrees with the frozen list")
	}
}
