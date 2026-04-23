import Foundation
import SwiftUI

// UserDefaults-backed config. Three scalars: host, auth token, viewer
// label. @Observable so SwiftUI views re-render on mutation.

@Observable
final class ConfigStore {
    private let hostKey = "peerWeb.host"
    private let tokenKey = "peerWeb.token"
    private let labelKey = "peerWeb.label"
    private let defaults: UserDefaults

    var host: String
    var token: String
    var label: String

    init(defaults: UserDefaults = .standard) {
        self.defaults = defaults
        self.host = defaults.string(forKey: hostKey) ?? ""
        self.token = defaults.string(forKey: tokenKey) ?? ""
        self.label = defaults.string(forKey: labelKey) ?? "owner"
    }

    /// Config is usable only when host + token are both set. Rooms list
    /// reads are open on the server, but send requires the bearer, and
    /// an empty host fails fast so we don't hit network with nonsense.
    var isReady: Bool {
        !host.trimmingCharacters(in: .whitespaces).isEmpty &&
        !token.trimmingCharacters(in: .whitespaces).isEmpty
    }

    func save() {
        let normalizedHost = host
            .trimmingCharacters(in: .whitespaces)
            .replacingOccurrences(
                of: "/",
                with: "",
                options: .anchored,
                range: host.index(host.endIndex, offsetBy: -1)..<host.endIndex
            )
        defaults.set(normalizedHost, forKey: hostKey)
        defaults.set(token.trimmingCharacters(in: .whitespaces), forKey: tokenKey)
        defaults.set(label.trimmingCharacters(in: .whitespaces).isEmpty ? "owner" : label, forKey: labelKey)
        // Mutate in place so @Observable emits a change after normalizing.
        host = defaults.string(forKey: hostKey) ?? ""
        token = defaults.string(forKey: tokenKey) ?? ""
        label = defaults.string(forKey: labelKey) ?? "owner"
    }
}
