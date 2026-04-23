import Foundation

// peer-web JSON response shapes. Decoded by PeerWebClient.

struct Member: Decodable, Hashable {
    let label: String?
    let agent: String?
    let role: String?
    let lastSeenAt: String?
    let state: String?
    let stateDisplay: String?

    enum CodingKeys: String, CodingKey {
        case label
        case agent
        case role
        case lastSeenAt = "last_seen_at"
        case state
        case stateDisplay = "state_display"
    }
}

struct Room: Decodable, Hashable, Identifiable {
    let pairKey: String
    let total: Int?
    let turnCount: Int?
    let lastId: Int?
    let activity: String?
    let lastActiveAt: String?
    let members: [String: Member]?

    var id: String { pairKey }

    enum CodingKeys: String, CodingKey {
        case pairKey = "pair_key"
        case total
        case turnCount = "turn_count"
        case lastId = "last_id"
        case activity
        case lastActiveAt = "last_active_at"
        case members
    }
}

struct Message: Decodable, Hashable, Identifiable {
    let id: Int
    let from: String
    let to: String
    let body: String
    let createdAt: String
    let read: Bool?

    enum CodingKeys: String, CodingKey {
        case id
        case from
        case to
        case body
        case createdAt = "created_at"
        case read
    }
}

struct MessagesResponse: Decodable {
    let messages: [Message]
    let hasMore: Bool
    let oldestId: Int
    let pairKey: String?

    enum CodingKeys: String, CodingKey {
        case messages
        case hasMore = "has_more"
        case oldestId = "oldest_id"
        case pairKey = "pair_key"
    }
}

struct IndexResponse: Decodable {
    // /api/index returns either `rooms: [Room]` (multi-room Go server) or
    // `pairs: {key: Room}` (legacy). We decode whichever is present.
    let rooms: [Room]?
    let pairs: [String: Room]?

    var allRooms: [Room] {
        if let rooms { return rooms }
        if let pairs {
            return pairs.map { key, room in
                // The legacy shape doesn't include pair_key inside the
                // Room dict; fill it from the map key so the app has a
                // stable identifier.
                Room(
                    pairKey: key,
                    total: room.total,
                    turnCount: room.turnCount,
                    lastId: room.lastId,
                    activity: room.activity,
                    lastActiveAt: room.lastActiveAt,
                    members: room.members
                )
            }
        }
        return []
    }
}
