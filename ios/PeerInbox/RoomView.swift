import SwiftUI

struct RoomView: View {
    let config: ConfigStore
    let room: Room

    @State private var messages: [Message] = []
    @State private var hasMore = true
    @State private var oldestId: Int = 0
    @State private var latestId: Int = 0
    @State private var loading = true
    @State private var loadingOlder = false
    @State private var composeBody: String = ""
    @State private var sending = false
    @State private var error: String?

    private let pollInterval: TimeInterval = 3.0

    var body: some View {
        VStack(spacing: 0) {
            if let error {
                Text(error)
                    .font(.caption)
                    .foregroundStyle(.red)
                    .padding(.horizontal)
                    .padding(.vertical, 8)
                    .frame(maxWidth: .infinity, alignment: .leading)
                    .background(Color.red.opacity(0.12))
            }
            messagesList
            composer
        }
        .navigationTitle(room.pairKey)
        .navigationBarTitleDisplayMode(.inline)
        .task { await initialLoad() }
        .task {
            // Poll loop tied to view lifetime via .task. Cancels on dismiss.
            while !Task.isCancelled {
                try? await Task.sleep(for: .seconds(pollInterval))
                await tailPoll()
            }
        }
    }

    @ViewBuilder
    private var messagesList: some View {
        ScrollViewReader { proxy in
            List {
                Section {
                    ForEach(messages) { m in
                        MessageRow(message: m)
                            .id(m.id)
                            .listRowSeparator(.hidden)
                            .onAppear {
                                // When the first (oldest) rendered row appears,
                                // fetch the previous page.
                                if m.id == messages.first?.id {
                                    Task { await loadOlder() }
                                }
                            }
                    }
                } header: {
                    if loadingOlder {
                        ProgressView().frame(maxWidth: .infinity, alignment: .center)
                    } else if !hasMore {
                        Text("start of history")
                            .font(.caption2)
                            .foregroundStyle(.tertiary)
                            .frame(maxWidth: .infinity, alignment: .center)
                            .padding(.vertical, 4)
                    } else {
                        Text("scroll up for older")
                            .font(.caption2)
                            .foregroundStyle(.tertiary)
                            .frame(maxWidth: .infinity, alignment: .center)
                            .padding(.vertical, 4)
                    }
                }
            }
            .listStyle(.plain)
            // iOS 18: anchor initial scroll to the newest row so users
            // land on the live conversation (matches web behavior).
            // defaultScrollAnchor handles the first-paint case that
            // onChange below misses because it fires before rows lay out.
            .defaultScrollAnchor(.bottom)
            .onChange(of: messages.last?.id) { _, newId in
                guard let newId else { return }
                // Auto-stick to bottom on new arrivals (after initial load).
                withAnimation(.easeOut(duration: 0.15)) {
                    proxy.scrollTo(newId, anchor: .bottom)
                }
            }
        }
    }

    @ViewBuilder
    private var composer: some View {
        HStack(spacing: 8) {
            TextField("Message…", text: $composeBody, axis: .vertical)
                .lineLimit(1...5)
                .textFieldStyle(.plain)
                .padding(.horizontal, 14)
                .padding(.vertical, 10)
                .background(Color(.secondarySystemBackground))
                .clipShape(RoundedRectangle(cornerRadius: 18))
            Button {
                Task { await send() }
            } label: {
                if sending {
                    ProgressView()
                } else {
                    Text("Send").fontWeight(.semibold)
                }
            }
            .buttonStyle(.borderedProminent)
            .disabled(composeBody.trimmingCharacters(in: .whitespaces).isEmpty || sending)
        }
        .padding(.horizontal, 10)
        .padding(.vertical, 8)
        .background(.bar)
    }

    // MARK: - Networking

    @MainActor
    private func initialLoad() async {
        guard messages.isEmpty else { return }
        do {
            let resp = try await PeerWebClient.fetchMessages(
                config: config, pairKey: room.pairKey, limit: 100
            )
            messages = resp.messages
            hasMore = resp.hasMore
            oldestId = resp.oldestId
            latestId = resp.messages.last?.id ?? 0
            error = nil
        } catch {
            self.error = String(describing: error)
        }
        loading = false
    }

    @MainActor
    private func loadOlder() async {
        guard !loadingOlder, hasMore, oldestId > 0 else { return }
        loadingOlder = true
        defer { loadingOlder = false }
        do {
            let resp = try await PeerWebClient.fetchMessages(
                config: config, pairKey: room.pairKey, before: oldestId, limit: 100
            )
            if resp.messages.isEmpty {
                hasMore = false
                return
            }
            messages = resp.messages + messages
            oldestId = resp.messages.first?.id ?? oldestId
            hasMore = resp.hasMore
        } catch {
            self.error = String(describing: error)
        }
    }

    @MainActor
    private func tailPoll() async {
        guard latestId > 0 else { return }
        do {
            let resp = try await PeerWebClient.fetchMessages(
                config: config, pairKey: room.pairKey, after: latestId, limit: 100
            )
            guard !resp.messages.isEmpty else { return }
            messages.append(contentsOf: resp.messages)
            latestId = resp.messages.last?.id ?? latestId
        } catch {
            // Swallow poll errors — next tick retries; don't flash red
            // on a transient hiccup.
        }
    }

    @MainActor
    private func send() async {
        let trimmed = composeBody.trimmingCharacters(in: .whitespaces)
        guard !trimmed.isEmpty else { return }
        sending = true
        defer { sending = false }
        do {
            try await PeerWebClient.sendMessage(
                config: config, pairKey: room.pairKey, body: trimmed
            )
            composeBody = ""
            // Immediate poll so the just-sent message appears fast.
            await tailPoll()
        } catch {
            self.error = String(describing: error)
        }
    }
}

private struct MessageRow: View {
    let message: Message

    var body: some View {
        VStack(alignment: .leading, spacing: 2) {
            Text("\(message.from) → \(message.to)")
                .font(.caption2)
                .foregroundStyle(.secondary)
            Text(message.body)
                .font(.body)
                .foregroundStyle(.primary)
                .fixedSize(horizontal: false, vertical: true)
            Text(message.createdAt.replacingOccurrences(of: "T", with: " ").replacingOccurrences(of: "Z", with: ""))
                .font(.caption2)
                .foregroundStyle(.tertiary)
        }
        .padding(.vertical, 4)
    }
}
