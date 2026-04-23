import SwiftUI

struct SettingsView: View {
    @Bindable var config: ConfigStore
    var onSaved: () -> Void

    @State private var saving = false

    var body: some View {
        NavigationStack {
            Form {
                Section {
                    TextField("http://100.75.236.84:8789", text: $config.host)
                        .textContentType(.URL)
                        .autocorrectionDisabled()
                        .textInputAutocapitalization(.never)
                        .keyboardType(.URL)
                } header: {
                    Text("Host URL")
                } footer: {
                    Text("Leave trailing slash off.")
                }

                Section {
                    SecureField("bearer token", text: $config.token)
                        .autocorrectionDisabled()
                        .textInputAutocapitalization(.never)
                } header: {
                    Text("Auth token")
                } footer: {
                    Text("From sessions.auth_token for a session registered with --agent human --role owner.")
                }

                Section {
                    TextField("owner", text: $config.label)
                        .autocorrectionDisabled()
                        .textInputAutocapitalization(.never)
                } header: {
                    Text("Viewer label")
                } footer: {
                    Text("The label messages will be sent from.")
                }

                Section {
                    Button {
                        saving = true
                        config.save()
                        saving = false
                        onSaved()
                    } label: {
                        HStack {
                            Spacer()
                            Text(saving ? "Saving…" : "Save & continue")
                                .fontWeight(.semibold)
                            Spacer()
                        }
                    }
                    .disabled(saving || config.host.isEmpty || config.token.isEmpty)
                }

                Section("Token mint recipe") {
                    Text("On the host:")
                        .font(.caption)
                    Text("agent-collab session register \\\n  --agent human --role owner \\\n  --label mobile-<device> --pair-key K")
                        .font(.system(.caption, design: .monospaced))
                    Text("sqlite3 ~/.agent-collab/sessions.db \\\n  \"SELECT auth_token FROM sessions \\\n    WHERE label='mobile-<device>';\"")
                        .font(.system(.caption, design: .monospaced))
                }
            }
            .navigationTitle("Settings")
        }
    }
}
