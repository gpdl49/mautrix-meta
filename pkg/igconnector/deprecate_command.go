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
	"fmt"
	"sort"
	"strings"
	"time"

	"maunium.net/go/mautrix/bridgev2/commands"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
)

// cmdDeprecate marks a DM deprecated (see deprecate.go and HOMESTACKS.md).
// There is no undeprecate command on purpose. Like freeze it runs in the
// chat's own room; there is no @username form.
var cmdDeprecate = &commands.FullHandler{
	Func: fnDeprecate,
	Name: "deprecate",
	Help: commands.HelpMeta{
		Section:     commands.HelpSectionGeneral,
		Description: "Permanently deprecate this DM: their messages arrive in delayed batches and you get one reply per batch. Cannot be undone from chat.",
		Args:        "[confirm|list]",
	},
	RequiresLogin: true,
}

const deprecateUsage = "Usage: `$cmdprefix deprecate confirm` in the DM's room, `$cmdprefix deprecate list` anywhere.\n\n" +
	"In a deprecated chat their messages are held for %s after the first " +
	"one and then delivered together; after that you can send one message, then wait for their next batch. " +
	"It applies to every future chat with them too, and **there is no command to undo it**."

func fnDeprecate(ce *commands.Event) {
	if deprecated.store == nil {
		ce.Reply("Deprecated chats are not available on this bridge.")
		return
	}
	if len(ce.Args) == 1 && strings.EqualFold(ce.Args[0], "list") {
		fnDeprecateList(ce)
		return
	}
	confirm := len(ce.Args) > 0 && strings.EqualFold(ce.Args[len(ce.Args)-1], "confirm")
	if confirm {
		ce.Args = ce.Args[:len(ce.Args)-1]
	}
	if len(ce.Args) > 0 || ce.Portal == nil {
		ce.Reply(deprecateUsage, deprecatedHoldDuration)
		return
	}
	portal := ce.Portal
	key, what := portal.PortalKey, "This chat"
	if portal.RoomType != database.RoomTypeDM || portal.OtherUserID == "" || string(portal.OtherUserID) != string(key.ID) ||
		key.Receiver == "" || string(portal.OtherUserID) == string(key.Receiver) {
		ce.Reply("Only one-to-one chats can be deprecated.")
		return
	}
	loc := userLocation(ce.User.GetDefaultLogin())
	if c, ok := deprecated.get(key); ok {
		ce.Reply("%s has been deprecated since %s. %s", what, c.Since.In(loc).Format("2006-01-02 15:04 MST"),
			describeDeprecated(ce, key, c, loc))
		return
	}
	if !confirm {
		ce.Reply(deprecateUsage+"\n\nTo go ahead, run it again with `confirm` at the end.", deprecatedHoldDuration)
		return
	}
	login := ce.Bridge.GetCachedUserLoginByID(key.Receiver)
	if login == nil || login.UserMXID != ce.User.MXID {
		ce.Reply("That chat doesn't belong to one of your Instagram accounts.")
		return
	}
	canReply := false
	if client, ok := login.Client.(*IGClient); ok {
		var err error
		if canReply, err = client.lastMessageIsTheirs(ce.Ctx, key); err != nil {
			ce.Reply("Failed to read the chat's history: %v", err)
			return
		}
	}
	if _, _, err := deprecated.deprecate(ce.Ctx, key, time.Now(), canReply); err != nil {
		ce.Reply("Failed to deprecate: %v", err)
		return
	}
	reply := "You can't send here until their next messages arrive."
	if canReply {
		reply = "Their message was the last one here, so you can send one reply now."
	}
	ce.Reply("%s is deprecated. Their messages will arrive in batches %s after the first one, with one reply allowed per batch. %s",
		what, deprecatedHoldDuration, reply)
}

func describeDeprecated(ce *commands.Event, key networkid.PortalKey, c deprecatedChat, loc *time.Location) string {
	switch {
	case !c.HoldUntil.IsZero():
		n, _ := deprecated.store.countEvents(ce.Ctx, key)
		return fmt.Sprintf("%d message(s) held until %s.", n, c.HoldUntil.In(loc).Format("15:04 MST"))
	case c.CanReply:
		return "You can send one reply."
	default:
		return "Waiting for their next messages."
	}
}

func fnDeprecateList(ce *commands.Event) {
	logins := make(map[networkid.UserLoginID]bool)
	for _, id := range ce.User.GetUserLoginIDs() {
		logins[id] = true
	}
	loc := userLocation(ce.User.GetDefaultLogin())
	var lines []string
	for key, c := range deprecated.list() {
		if !logins[key.Receiver] {
			continue
		}
		lines = append(lines, fmt.Sprintf("* %s — since %s. %s", freezeChatName(ce, key),
			c.Since.In(loc).Format("2006-01-02"), describeDeprecated(ce, key, c, loc)))
	}
	if len(lines) == 0 {
		ce.Reply("No chats are deprecated.")
		return
	}
	sort.Strings(lines)
	ce.Reply("Deprecated chats:\n\n%s", strings.Join(lines, "\n"))
}
