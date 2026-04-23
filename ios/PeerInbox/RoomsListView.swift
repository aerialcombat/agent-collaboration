import SwiftUI

struct RoomsListView: View {
    let config: ConfigStore
    var onOpenRoom: (Room) -> Void
    var onOpenSettings: () -> Void

    @State private var rooms: [Room] = []
    @State private var loading = true
    @State private var error: String?

    var body: some View {
        Group {
            if loading && rooms.isEmpty {
                ProgressView()
            } else if let error, rooms.isEmpty {
                VStack(spacing: 8) {
                    Text("Couldn't load rooms.")
                        .font(.headline)
                    Text(error)
                        .font(.caption)
                        .foregroundStyle(.secondary)
                        .multilineTextAlignment(.center)
                        .padding(.horizontal)
                    Button("Retry") { Task { await load() } }
                        .padding(.top)
                }
            } else {
                List {
                    ForEach(rooms) { room in
                        Button { onOpenRoom(room) } label: {
                            HStack {
                                VStack(alignment: .leading, spacing: 3) {
                                    Text(room.pairKey)
                                        .font(.body)
                                        .foregroundStyle(.primary)
                                    Text(roomSubtitle(room))
                                        .font(.caption)
                                        .foregroundStyle(.secondary)
                                }
                                Spacer()
                                Image(systemName: "chevron.right")
                                    .foregroundStyle(.tertiary)
                                    .imageScale(.small)
                            }
                        }
                    }
                }
                .refreshable { await load() }
            }
        }
        .navigationTitle("Rooms")
        .toolbar {
            ToolbarItem(placement: .topBarTrailing) {
                Button {
                    onOpenSettings()
                } label: {
                    Image(systemName: "gear")
                }
            }
        }
        .task { await load() }
    }

    private func roomSubtitle(_ r: Room) -> String {
        var parts: [String] = []
        if let t = r.total { parts.append("\(t) messages") }
        if let tc = r.turnCount { parts.append("\(tc) turns") }
        if let a = r.activity { parts.append(a) }
        return parts.joined(separator: " · ")
    }

    @MainActor
    private func load() async {
        do {
            var list = try await PeerWebClient.listRooms(config: config)
            list.sort { lhs, rhs in
                let la = lhs.lastActiveAt ?? ""
                let ra = rhs.lastActiveAt ?? ""
                if la != ra { return la > ra }
                return (lhs.total ?? 0) > (rhs.total ?? 0)
            }
            rooms = list
            error = nil
        } catch {
            self.error = String(describing: error)
        }
        loading = false
    }
}
