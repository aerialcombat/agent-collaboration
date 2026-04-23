import Foundation

// REST client for peer-web. Stateless; reads host + bearer from the
// ConfigStore passed in on each call. All methods throw; the caller
// surfaces errors in the UI.

struct PeerWebError: LocalizedError {
    let status: Int
    let detail: String
    var errorDescription: String? { "HTTP \(status): \(detail)" }
}

enum PeerWebClient {
    /// Sanitize body before JSONDecoder. Server can emit raw control
    /// chars inside message.body (pre-existing bug); Foundation's
    /// JSONDecoder rejects them. Replace with space — same approach as
    /// the mobile-JS client.
    private static func sanitized(_ data: Data) -> Data {
        var out = Data()
        out.reserveCapacity(data.count)
        for byte in data {
            if byte >= 0x20 || byte == 0x09 || byte == 0x0a || byte == 0x0d {
                out.append(byte)
            } else {
                out.append(0x20)
            }
        }
        return out
    }

    private static func request(config: ConfigStore, path: String, method: String = "GET", body: Data? = nil) throws -> URLRequest {
        guard !config.host.isEmpty, let base = URL(string: config.host) else {
            throw PeerWebError(status: 0, detail: "host not configured")
        }
        guard let url = URL(string: path, relativeTo: base) else {
            throw PeerWebError(status: 0, detail: "bad path: \(path)")
        }
        var req = URLRequest(url: url)
        req.httpMethod = method
        req.setValue("application/json", forHTTPHeaderField: "Accept")
        if !config.token.isEmpty {
            req.setValue("Bearer \(config.token)", forHTTPHeaderField: "Authorization")
        }
        if let body {
            req.httpBody = body
            req.setValue("application/json", forHTTPHeaderField: "Content-Type")
        }
        return req
    }

    private static func run<T: Decodable>(_ req: URLRequest, as: T.Type = T.self) async throws -> T {
        let (data, resp) = try await URLSession.shared.data(for: req)
        guard let http = resp as? HTTPURLResponse else {
            throw PeerWebError(status: 0, detail: "non-http response")
        }
        guard (200..<300).contains(http.statusCode) else {
            let preview = String(data: data.prefix(200), encoding: .utf8) ?? ""
            throw PeerWebError(status: http.statusCode, detail: preview)
        }
        let decoder = JSONDecoder()
        return try decoder.decode(T.self, from: sanitized(data))
    }

    static func listRooms(config: ConfigStore) async throws -> [Room] {
        let req = try request(config: config, path: "/api/index")
        let resp: IndexResponse = try await run(req)
        return resp.allRooms
    }

    static func fetchRoomMembers(config: ConfigStore, pairKey: String) async throws -> Room? {
        let pk = pairKey.addingPercentEncoding(withAllowedCharacters: .urlQueryAllowed) ?? pairKey
        let req = try request(config: config, path: "/api/rooms?pair_key=\(pk)")
        struct R: Decodable { let rooms: [Room]? }
        let resp: R = try await run(req)
        return resp.rooms?.first
    }

    static func fetchMessages(
        config: ConfigStore,
        pairKey: String,
        before: Int? = nil,
        after: Int? = nil,
        limit: Int = 100
    ) async throws -> MessagesResponse {
        var comps = URLComponents()
        comps.queryItems = [
            URLQueryItem(name: "pair_key", value: pairKey),
            URLQueryItem(name: "limit", value: String(limit)),
        ]
        if let before { comps.queryItems?.append(URLQueryItem(name: "before", value: String(before))) }
        if let after { comps.queryItems?.append(URLQueryItem(name: "after", value: String(after))) }
        let path = "/api/messages?" + (comps.percentEncodedQuery ?? "")
        let req = try request(config: config, path: path)
        return try await run(req)
    }

    static func sendMessage(
        config: ConfigStore,
        pairKey: String,
        body: String,
        to: String = "@room"
    ) async throws {
        let payload: [String: String] = [
            "from": config.label.isEmpty ? "owner" : config.label,
            "to": to,
            "body": body,
        ]
        let data = try JSONSerialization.data(withJSONObject: payload)
        let pk = pairKey.addingPercentEncoding(withAllowedCharacters: .urlQueryAllowed) ?? pairKey
        let req = try request(config: config, path: "/api/send?pair_key=\(pk)", method: "POST", body: data)
        // /api/send returns a JSON envelope; we don't need the fields.
        struct Ack: Decodable {}
        let _: Ack = try await run(req)
    }
}
