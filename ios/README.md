# peer-inbox iOS companion

Native SwiftUI app that connects to a running `peer-web` and lets you
read rooms, scroll message history, and send broadcasts from your
iPhone. Built for personal use; not App Store-ready.

## Prerequisites

- **Xcode 16+** (deploys against iOS 18)
- **xcodegen** — `brew install xcodegen`
- A running `peer-web` reachable from the phone (LAN, VPN, or the
  local simulator on the same machine)
- A bearer token for the session that should own outgoing messages

## First-time setup

```bash
cd ios
xcodegen generate          # builds PeerInbox.xcodeproj from project.yml
open PeerInbox.xcodeproj
```

Pick a simulator (or your phone) in Xcode's scheme picker and press **⌘R**.

`PeerInbox.xcodeproj` and the generated `PeerInbox/Info.plist` are
gitignored — regenerate after every edit to `project.yml`. Source files
under `PeerInbox/*.swift` and the xcodegen spec itself (`project.yml`)
are the committed artifacts.

## Configure on first launch

App opens a Settings sheet. Fill in:

| Field | Example | Notes |
|---|---|---|
| Host URL | `http://100.75.236.84:8789` | No trailing slash. Must be reachable from the phone. |
| Auth token | `23L4tn2s…` | From `sessions.auth_token` for a session registered as the owner of a pair-key. |
| Viewer label | `owner` | The `from` label on outgoing messages. |

### Minting a bearer token

On the host running `peer-web`:

```bash
agent-collab session register \
    --agent human --role owner \
    --label mobile-<device> \
    --pair-key <ROOM_PAIR_KEY>

sqlite3 ~/.agent-collab/sessions.db \
    "SELECT auth_token FROM sessions WHERE label='mobile-<device>';"
```

Paste that token into the app. Reads (`/api/index`, `/api/messages`,
`/api/rooms`) work without auth in multi-room mode; sends require the
bearer.

## Project structure

```
ios/
├── project.yml                   xcodegen spec
├── PeerInbox/
│   ├── PeerInboxApp.swift        @main
│   ├── ContentView.swift         root NavigationStack + Settings sheet
│   ├── SettingsView.swift        config form
│   ├── RoomsListView.swift       rooms list + pull-to-refresh
│   ├── RoomView.swift            messages + composer + infinite scroll
│   ├── ConfigStore.swift         @Observable UserDefaults wrapper
│   ├── PeerWebClient.swift       URLSession REST client
│   └── Models.swift              Codable Room / Message / MessagesResponse
└── .gitignore
```

No third-party Swift packages. Everything is Foundation + SwiftUI.

## Feature parity with `peer-web`

| Capability | iOS | Web |
|---|---|---|
| List rooms with recency / activity | ✅ | ✅ |
| Open room, load newest 100 messages | ✅ | ✅ |
| Scroll-up loads next 100 older | ✅ | ✅ |
| Land on newest message on open | ✅ | ✅ |
| Compose + send `@room` broadcast | ✅ | ✅ |
| 3s tail poll for new arrivals | ✅ | ✅ |
| Bearer-token auth on send | ✅ | ✅ |
| State dots (🟢/🟡/🔴) in roster | ⏳ follow-up | ✅ |
| Compose to specific label | ⏳ follow-up | ✅ |
| Mention highlighting | ⏳ follow-up | ✅ |
| Push notifications (APNs) | ⏳ follow-up | n/a |
| WebSocket streaming | ⏳ follow-up | ⏳ |

## Simulator vs. device

- **Simulator** talks to `http://localhost:<port>` directly — use the
  laptop's peer-web (default `:18081`) for the fastest loop.
- **Physical device** needs a host URL reachable over the network.
  Tailscale IPs (`http://100.x.x.x:8789`) work without any certificate
  dance. For pure-LAN, use the laptop's LAN IP — not `localhost`.

HTTP is allowed because `NSAllowsArbitraryLoads` is on in `project.yml`.
Tighten to `NSExceptionDomains` before shipping to TestFlight so
third-party networks can't be hit cleartext.

## Developing

The typical edit loop:

```bash
# edit a .swift file
cd ios && xcodegen generate    # only needed after project.yml changes
# ⌘R in Xcode
```

For headless build verification:

```bash
xcodebuild -project PeerInbox.xcodeproj -scheme PeerInbox \
    -destination 'generic/platform=iOS Simulator' \
    -configuration Debug -sdk iphonesimulator build \
    CODE_SIGNING_ALLOWED=NO
```

For scripted installs on a booted simulator:

```bash
SIMID=$(xcrun simctl list devices booted | grep -oE '[0-9A-F-]{36}' | head -1)
xcrun simctl install $SIMID \
    ~/Library/Developer/Xcode/DerivedData/PeerInbox-*/Build/Products/Debug-iphonesimulator/PeerInbox.app
xcrun simctl launch $SIMID co.ooocorp.peerinbox
```

## Deferred follow-ups

Tracked in `CHANGELOG.md`. Rough priority:

1. **Push notifications** — requires Apple Developer account, APNs
   cert, `/api/register-push` endpoint on `peer-web`, and
   `UNUserNotificationCenter` on device.
2. **WebSocket streaming** — replaces the 3s poll with real-time;
   unblocks instant push.
3. **State dots in roster** — `/api/rooms` already returns
   `state_display`, just render a colored circle next to each member.
4. **Compose to-label picker + mention highlighting** — parity with
   web composer.
5. **Tighten NSAppTransportSecurity** before TestFlight.
