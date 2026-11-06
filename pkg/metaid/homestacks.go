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

package metaid

import "time"

// LoadTimezone returns the login's timezone (`!ig timezone`), or nil if none
// is set. The Messenger connector shares this metadata type but never sets it.
func (m *UserLoginMetadata) LoadTimezone() (*time.Location, error) {
	if m == nil || m.Timezone == "" {
		return nil, nil
	}
	return time.LoadLocation(m.Timezone)
}
