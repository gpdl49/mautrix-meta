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
	"path/filepath"
	"testing"
	"time"

	"go.mau.fi/util/dbutil"
	_ "go.mau.fi/util/dbutil/litestream"

	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"go.mau.fi/mautrix-meta/pkg/metaid"
)

func TestClockMinutes(t *testing.T) {
	good := map[string]int{
		"18:30": 18*60 + 30, "0:00": 0, "8": 8 * 60, "0830": 8*60 + 30, "23:59": 23*60 + 59,
		"8am": 8 * 60, "6:30pm": 18*60 + 30, "12am": 0, "12pm": 12 * 60, "7 PM": 19 * 60,
	}
	for in, want := range good {
		if got, err := clockMinutes(in); err != nil || got != want {
			t.Errorf("clockMinutes(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"", "24:00", "8:5", "8:60", "13pm", "0am", "noon", "-1", ":30"} {
		if got, err := clockMinutes(in); err == nil {
			t.Errorf("clockMinutes(%q) = %d, want error", in, got)
		}
	}
}

func TestParseSendTime(t *testing.T) {
	loc := time.FixedZone("X", 2*3600)
	now := time.Date(2026, 10, 20, 17, 0, 30, 0, loc)
	cases := map[string]time.Time{
		"18:30":  time.Date(2026, 10, 20, 18, 30, 0, 0, loc),
		"9am":    time.Date(2026, 10, 21, 9, 0, 0, 0, loc),
		"17:00":  time.Date(2026, 10, 21, 17, 0, 0, 0, loc), // already passed this minute
		"+45m":   now.Add(45 * time.Minute).Truncate(time.Second),
		"+2h30m": now.Add(150 * time.Minute).Truncate(time.Second),
		"+24h":   now.Add(24 * time.Hour).Truncate(time.Second),
	}
	for in, want := range cases {
		if got, err := parseSendTime(in, now); err != nil || !got.Equal(want) {
			t.Errorf("parseSendTime(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, in := range []string{"+25h", "+0m", "+-5m", "+soon", "tomorrow"} {
		if _, err := parseSendTime(in, now); err == nil {
			t.Errorf("parseSendTime(%q) succeeded, want error", in)
		}
	}
}

func TestOfflineUntil(t *testing.T) {
	loc := time.FixedZone("X", 0)
	at := func(d, h, m int) time.Time { return time.Date(2026, 10, d, h, m, 0, 0, loc) }
	night, err := parseOfflineHours("22:00-08:00")
	if err != nil {
		t.Fatal(err)
	}
	day := &metaid.OfflineHours{Start: 9 * 60, End: 17 * 60}
	cases := []struct {
		oh     *metaid.OfflineHours
		now    time.Time
		inside bool
		until  time.Time
	}{
		{night, at(20, 23, 15), true, at(21, 8, 0)},
		{night, at(21, 2, 0), true, at(21, 8, 0)},
		{night, at(21, 7, 59), true, at(21, 8, 0)},
		{night, at(21, 8, 0), false, time.Time{}},
		{night, at(20, 21, 59), false, time.Time{}},
		{night, at(20, 22, 0), true, at(21, 8, 0)},
		{day, at(20, 9, 0), true, at(20, 17, 0)},
		{day, at(20, 16, 59), true, at(20, 17, 0)},
		{day, at(20, 17, 0), false, time.Time{}},
		{day, at(20, 8, 0), false, time.Time{}},
	}
	for _, c := range cases {
		until, inside := offlineUntil(c.oh, c.now)
		if inside != c.inside || !until.Equal(c.until) {
			t.Errorf("offlineUntil(%s, %v) = %v, %v; want %v, %v",
				describeOfflineHours(c.oh), c.now, until, inside, c.until, c.inside)
		}
	}
}

func TestOfflineUntilAcrossDST(t *testing.T) {
	loc, err := time.LoadLocation("Europe/London")
	if err != nil {
		t.Fatal(err)
	}
	// Clocks go back at 02:00 BST on 2026-10-25; the window still ends at 08:00 GMT.
	now := time.Date(2026, 10, 24, 23, 0, 0, 0, loc)
	until, inside := offlineUntil(&metaid.OfflineHours{Start: 22 * 60, End: 8 * 60}, now)
	if want := time.Date(2026, 10, 25, 8, 0, 0, 0, loc); !inside || !until.Equal(want) || until.Sub(now) != 10*time.Hour {
		t.Errorf("got %v, %v; want %v (10h away)", until, inside, want)
	}
}

func TestParseOfflineHours(t *testing.T) {
	oh, err := parseOfflineHours("10pm-8am")
	if err != nil || oh.Start != 22*60 || oh.End != 8*60 {
		t.Errorf("parseOfflineHours(10pm-8am) = %+v, %v", oh, err)
	}
	for _, in := range []string{"22:00", "8-8", "22:00-25:00", "-8"} {
		if _, err = parseOfflineHours(in); err == nil {
			t.Errorf("parseOfflineHours(%q) succeeded, want error", in)
		}
	}
}

func TestHeldEventRoundTrip(t *testing.T) {
	// Content as received keeps fields the bridge strips from the parsed copy.
	raw := `{"msgtype":"m.image","body":"cat.jpg","url":"mxc://example.com/abc","info":{"mimetype":"image/jpeg"}}`
	evt := &event.Event{
		Type: event.EventMessage, ID: "$evt", RoomID: "!room:example.com", Sender: "@me:example.com",
		Timestamp: 1700000000000,
	}
	if err := evt.Content.UnmarshalJSON([]byte(raw)); err != nil {
		t.Fatal(err)
	}
	_ = evt.Content.ParseRaw(evt.Type)
	evt.Content.AsMessage().Body = "mutated"
	data, err := encodeHeldEvent(evt)
	if err != nil {
		t.Fatal(err)
	}
	got, err := decodeHeldEvent(data)
	if err != nil {
		t.Fatal(err)
	}
	msg := got.Content.AsMessage()
	if got.Type != event.EventMessage || got.ID != evt.ID || got.RoomID != evt.RoomID || got.Sender != evt.Sender ||
		got.Timestamp != evt.Timestamp || msg.MsgType != event.MsgImage || msg.Body != "cat.jpg" || msg.URL != "mxc://example.com/abc" {
		t.Errorf("round trip lost data: %+v %+v", got, msg)
	}

	// Inline `!ig schedule` text has only parsed content.
	inline := &event.Event{Type: event.EventMessage, ID: "$cmd", Content: event.Content{
		Parsed: &event.MessageEventContent{MsgType: event.MsgText, Body: "hi"},
	}}
	if data, err = encodeHeldEvent(inline); err != nil {
		t.Fatal(err)
	}
	if got, err = decodeHeldEvent(data); err != nil || got.Content.AsMessage().Body != "hi" {
		t.Errorf("inline round trip = %+v, %v", got, err)
	}
}

func TestHeldStateArm(t *testing.T) {
	hs := &heldState{armed: map[armKey]armedSchedule{}, releasing: map[id.EventID]time.Time{}}
	now := time.Now()
	at := now.Add(time.Hour)
	hs.arm("!r", "@u", at, now)
	if _, ok := hs.takeArmed("!r", "@other", now); ok {
		t.Error("armed schedule leaked to another user")
	}
	if got, ok := hs.takeArmed("!r", "@u", now.Add(time.Minute)); !ok || !got.Equal(at) {
		t.Error("armed schedule not returned")
	}
	if _, ok := hs.takeArmed("!r", "@u", now); ok {
		t.Error("armed schedule used twice")
	}
	hs.arm("!r", "@u", at, now)
	if _, ok := hs.takeArmed("!r", "@u", now.Add(heldArmTTL+time.Second)); ok {
		t.Error("expired armed schedule used")
	}

	hs.markReleasing("$a", now)
	if !hs.takeReleasing("$a") || hs.takeReleasing("$a") {
		t.Error("releasing mark should pass exactly once")
	}
}

func newHeldTestStore(t *testing.T, bridgeID string) *heldMessageStore {
	t.Helper()
	raw, err := dbutil.NewWithDialect("file:"+filepath.Join(t.TempDir(), "held.db")+"?_txlock=immediate", "sqlite3-fk-wal")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	db := raw.Child(heldVersionTable, heldUpgrades, nil)
	if err = db.Upgrade(context.Background()); err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	return &heldMessageStore{db: db, bridgeID: networkid.BridgeID(bridgeID)}
}

func TestHeldStoreRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := newHeldTestStore(t, "ig")
	now := time.UnixMilli(time.Now().UnixMilli())
	mk := func(evt id.EventID, room id.RoomID, reason string, sendAt time.Time) *heldMessage {
		return &heldMessage{EventID: evt, RoomID: room, LoginID: "login1", Reason: reason, SendAt: sendAt,
			HeldAt: now, Event: []byte(`{"n":1}`), ReactionID: "$react"}
	}
	for _, h := range []*heldMessage{
		mk("$late", "!a", heldReasonOffline, now.Add(time.Hour)),
		mk("$soon", "!a", heldReasonSchedule, now.Add(-time.Minute)),
		mk("$other", "!b", heldReasonSchedule, now.Add(-2*time.Minute)),
	} {
		if err := s.insert(ctx, h); err != nil {
			t.Fatalf("insert %s: %v", h.EventID, err)
		}
	}
	due, err := s.listDue(ctx, now)
	if err != nil || len(due) != 2 || due[0].EventID != "$other" || due[1].EventID != "$soon" {
		t.Fatalf("listDue = %+v, %v; want $other then $soon (oldest first)", due, err)
	}
	if at, ok, err := s.offlineBatchSendAt(ctx, "!a"); err != nil || !ok || !at.Equal(now.Add(time.Hour)) {
		t.Errorf("offlineBatchSendAt = %v, %v, %v", at, ok, err)
	}
	if _, ok, _ := s.offlineBatchSendAt(ctx, "!b"); ok {
		t.Error("room with only a schedule reports an offline batch")
	}

	// `schedule now` makes a held message due without ever moving a due one later.
	if err = s.makeDue(ctx, "$late", now); err != nil {
		t.Fatal(err)
	}
	if err = s.makeDue(ctx, "$soon", now); err != nil {
		t.Fatal(err)
	}
	room, err := s.listRoom(ctx, "!a")
	if err != nil || len(room) != 2 || !room[0].SendAt.Equal(now.Add(-time.Minute)) || !room[1].SendAt.Equal(now) {
		t.Fatalf("after makeDue: %+v, %v", room, err)
	}
	if due, _ = s.listDue(ctx, now); len(due) != 3 {
		t.Errorf("after makeDue %d due, want 3", len(due))
	}

	if taken, err := s.take(ctx, "$soon"); err != nil || !taken {
		t.Errorf("take = %v, %v", taken, err)
	}
	if taken, _ := s.take(ctx, "$soon"); taken {
		t.Error("held message taken twice")
	}
	theirs := &heldMessageStore{db: s.db, bridgeID: "other-bridge"}
	if rows, _ := theirs.listRoom(ctx, "!a"); len(rows) != 0 {
		t.Errorf("other bridge sees %d rows", len(rows))
	}
}

func TestHeldReleaseGapStaysPaced(t *testing.T) {
	if heldReleaseBatch < 1 || heldReleaseGapMin <= 0 || heldReleaseGapMax <= heldReleaseGapMin {
		t.Fatalf("bad pacing constants: batch %d, gap %v-%v", heldReleaseBatch, heldReleaseGapMin, heldReleaseGapMax)
	}
	// A pass must finish well inside the loop interval, or passes would overlap.
	if worst := time.Duration(heldReleaseBatch-1) * heldReleaseGapMax; worst >= heldReleaseInterval {
		t.Errorf("a full batch takes up to %v, longer than the %v interval", worst, heldReleaseInterval)
	}
}
