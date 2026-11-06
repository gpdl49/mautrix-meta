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
	"maunium.net/go/mautrix/bridgev2/networkid"
)

// cmdFreeze and cmdUnfreeze manage the homestacks frozen chats (see freeze.go
// and HOMESTACKS.md). They run in the chat's own room; freezing by
// @username would need a resolver for starting chats and is not supported.
var cmdFreeze = &commands.FullHandler{
	Func: fnFreeze,
	Name: "freeze",
	Help: commands.HelpMeta{
		Section:     commands.HelpSectionGeneral,
		Description: "Stop bridging new Instagram messages in this chat until it is unfrozen.",
		Args:        "[list]",
	},
	RequiresLogin: true,
}

var cmdUnfreeze = &commands.FullHandler{
	Func: fnUnfreeze,
	Name: "unfreeze",
	Help: commands.HelpMeta{
		Section:     commands.HelpSectionGeneral,
		Description: "Unfreeze this chat and bridge the messages that arrived meanwhile.",
	},
	RequiresLogin:  true,
	RequiresPortal: true,
}

const freezeUsage = "Usage: `$cmdprefix freeze` and `$cmdprefix unfreeze` in the chat's room, `$cmdprefix freeze list` anywhere.\n\n" +
	"Instagram messages in a frozen chat are kept by the bridge, not shown here; unfreezing brings them all in, " +
	"in order. You can't send in a frozen chat."

func freezeChatName(ce *commands.Event, key networkid.PortalKey) string {
	portal, err := ce.Bridge.GetExistingPortalByKey(ce.Ctx, key)
	if err != nil || portal == nil {
		return string(key.ID)
	} else if portal.Name != "" {
		return portal.Name
	} else if portal.OtherUserID != "" {
		if ghost, err := ce.Bridge.GetExistingGhostByID(ce.Ctx, portal.OtherUserID); err == nil && ghost != nil && ghost.Name != "" {
			return ghost.Name
		}
	}
	return string(key.ID)
}

func fnFreeze(ce *commands.Event) {
	if frozen.store == nil {
		ce.Reply("Freezing is not available on this bridge.")
		return
	}
	if len(ce.Args) == 1 && strings.EqualFold(ce.Args[0], "list") {
		fnFreezeList(ce)
		return
	} else if len(ce.Args) > 0 || ce.Portal == nil {
		ce.Reply(freezeUsage)
		return
	}
	since, already, err := frozen.freeze(ce.Ctx, ce.Portal.PortalKey, time.Now())
	if err != nil {
		ce.Reply("Failed to freeze: %v", err)
		return
	}
	loc := userLocation(ce.User.GetDefaultLogin())
	if already {
		n, _ := frozen.store.countEvents(ce.Ctx, ce.Portal.PortalKey)
		ce.Reply("This chat has been frozen since %s; %d message(s) waiting.", since.In(loc).Format("2006-01-02 15:04 MST"), n)
		return
	}
	ce.Reply("This chat is frozen: new Instagram messages are kept back until `$cmdprefix unfreeze`, and you can't send here.")
}

func fnFreezeList(ce *commands.Event) {
	chats := frozen.list()
	logins := make(map[networkid.UserLoginID]bool)
	for _, id := range ce.User.GetUserLoginIDs() {
		logins[id] = true
	}
	loc := userLocation(ce.User.GetDefaultLogin())
	var lines []string
	for key, since := range chats {
		// Only DMs carry a receiver; a shared group shows for everyone in it.
		if key.Receiver != "" && !logins[key.Receiver] {
			continue
		}
		n, _ := frozen.store.countEvents(ce.Ctx, key)
		lines = append(lines, fmt.Sprintf("* %s — since %s, %d waiting", freezeChatName(ce, key), since.In(loc).Format("2006-01-02 15:04 MST"), n))
	}
	if len(lines) == 0 {
		ce.Reply("No chats are frozen.")
		return
	}
	sort.Strings(lines)
	ce.Reply("Frozen chats:\n\n%s", strings.Join(lines, "\n"))
}

func fnUnfreeze(ce *commands.Event) {
	if frozen.store == nil {
		ce.Reply("Freezing is not available on this bridge.")
		return
	}
	if len(ce.Args) > 0 {
		ce.Reply(freezeUsage)
		return
	}
	key := ce.Portal.PortalKey
	if _, isFrozen := frozen.since(key); !isFrozen {
		ce.Reply("This chat isn't frozen.")
		return
	}
	var catchUp *IGClient
	if login, _, err := ce.Portal.FindPreferredLogin(ce.Ctx, ce.User, false); err == nil && login != nil {
		catchUp, _ = login.Client.(*IGClient)
	}
	ic := ce.Bridge.Network.(*IGConnector)
	replayed, dropped, notes, err := ic.unfreeze(ce.Ctx, key, ce.Portal, catchUp)
	if err != nil {
		ce.Reply("This chat is still frozen: bridged %d held message(s), then stopped: %v. Run it again to retry.", replayed, err)
		return
	}
	msg := fmt.Sprintf("This chat is unfrozen; %d held event(s) bridged.", replayed)
	if dropped > 0 {
		msg += fmt.Sprintf(" %d could not be read (or their account is gone) and were dropped.", dropped)
	}
	for _, note := range notes {
		msg += "\n\n⚠️ " + note
	}
	ce.Reply("%s", msg)
}
