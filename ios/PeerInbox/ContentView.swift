import SwiftUI

/// Root view. Three screen states stitched together without a heavy
/// navigation router — just NavigationStack + a sheet for Settings.
struct ContentView: View {
    @State private var config = ConfigStore()
    @State private var showingSettings = false
    @State private var selectedRoom: Room?

    var body: some View {
        NavigationStack {
            RoomsListView(
                config: config,
                onOpenRoom: { room in selectedRoom = room },
                onOpenSettings: { showingSettings = true }
            )
            .navigationDestination(item: $selectedRoom) { room in
                RoomView(config: config, room: room)
            }
        }
        .sheet(isPresented: $showingSettings) {
            SettingsView(config: config, onSaved: {
                showingSettings = false
            })
            .interactiveDismissDisabled(!config.isReady)
        }
        .onAppear {
            // First launch: no host/token yet → present Settings as the
            // initial screen so the user isn't staring at an "error"
            // rooms list.
            if !config.isReady {
                showingSettings = true
            }
        }
    }
}
