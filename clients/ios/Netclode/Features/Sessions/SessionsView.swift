import SwiftUI

struct SessionsView: View {
    @Environment(SessionStore.self) private var sessionStore
    @Environment(ConnectService.self) private var connectService
    @Environment(SettingsStore.self) private var settingsStore
    @Environment(GitHubStore.self) private var githubStore
    @Environment(UnifiedModelsStore.self) private var modelsStore

    @State private var showPromptSheet = false
    @State private var showSettingsSheet = false
    @State private var selectedSession: Session?
    @State private var sessionToDelete: Session?

    var body: some View {
        Group {
            if sessionStore.sessions.isEmpty {
                EmptySessionsView(onCreateTapped: { showPromptSheet = true })
            } else {
                sessionListContent
            }
        }
        .frame(maxWidth: .infinity, maxHeight: .infinity)
        .background(Theme.Colors.background)
        .overlay(alignment: .bottomTrailing) {
            NewSessionButton {
                showPromptSheet = true
            }
        }
        .navigationBarTitleDisplayMode(.inline)
        .toolbarBackgroundVisibility(.hidden, for: .navigationBar)
        .toolbar {
            ToolbarItem(placement: .principal) {
                HStack(spacing: 6) {
                    Text("netclode")
                        .font(.system(.headline, design: .monospaced))
                        .fontWeight(.medium)

                    Circle()
                        .fill(connectionColor)
                        .frame(width: 6, height: 6)
                        .pulsing(connectService.connectionState.isConnected)
                }
                .onChange(of: connectService.connectionState) { oldState, newState in
                    handleConnectionChange(from: oldState, to: newState)
                }
            }

            ToolbarItem(placement: .topBarTrailing) {
                GlassIconButton(icon: "gearshape.fill", size: 36) {
                    showSettingsSheet = true
                }
            }
        }
        .sheet(isPresented: $showSettingsSheet) {
            NavigationStack {
                SettingsView()
                    .navigationBarTitleDisplayMode(.inline)
                    .toolbar {
                        ToolbarItem(placement: .confirmationAction) {
                            Button("Done") {
                                showSettingsSheet = false
                            }
                        }
                    }
            }
            .environment(connectService)
            .environment(settingsStore)
        }
        .fullScreenCover(isPresented: $showPromptSheet) {
            PromptSheet()
                .environment(connectService)
                .environment(settingsStore)
                .environment(sessionStore)
                .environment(githubStore)
                .environment(modelsStore)
        }
        .navigationDestination(item: $selectedSession) { session in
            WorkspaceView(sessionId: session.id)
        }
        .onChange(of: sessionStore.pendingSessionId) { _, newId in
            // Auto-navigate to newly created session
            if let sessionId = newId,
               let session = sessionStore.sessions.first(where: { $0.id == sessionId }) {
                selectedSession = session
            }
        }
        .onChange(of: sessionStore.pendingCreationError) { _, newError in
            if newError != nil, settingsStore.hapticFeedbackEnabled {
                HapticFeedback.error()
            }
        }
        .onAppear {
            if connectService.connectionState.isConnected {
                connectService.send(.sessionList)
            }
        }
        .refreshable {
            // Reconnect if needed, then refresh session list
            if !connectService.connectionState.isConnected {
                connectService.ensureConnected(to: settingsStore.serverURL)
                // Wait for connection to actually establish
                let connected = await connectService.waitForConnection(timeout: 15)
                guard connected else { return }
            }
            connectService.send(.sessionList)
        }
        .alert("Delete Session?", isPresented: .init(
            get: { sessionToDelete != nil },
            set: { if !$0 { sessionToDelete = nil } }
        )) {
            Button("Cancel", role: .cancel) {
                sessionToDelete = nil
            }
            Button("Delete", role: .destructive) {
                if let session = sessionToDelete {
                    if settingsStore.hapticFeedbackEnabled {
                        HapticFeedback.warning()
                    }
                    let sessionId = session.id
                    withAnimation {
                        sessionStore.removeSession(id: sessionId)
                    }
                    connectService.send(.sessionDelete(id: sessionId))
                    sessionToDelete = nil
                }
            }
        } message: {
            if let session = sessionToDelete {
                Text("This will permanently delete \"\(session.name)\" and all its data.")
            }
        }
        .alert("Session Creation Failed", isPresented: .init(
            get: { sessionStore.pendingCreationError != nil },
            set: { if !$0 { sessionStore.clearPendingCreationError() } }
        )) {
            Button("OK") {
                sessionStore.clearPendingCreationError()
            }
        } message: {
            if let error = sessionStore.pendingCreationError {
                Text(error)
            }
        }
    }

    private var connectionColor: Color {
        switch connectService.connectionState {
        case .connected: .green
        case .connecting, .reconnecting: .orange
        case .disconnected, .suspended: .red
        }
    }

    private func handleConnectionChange(from oldState: ConnectionState, to newState: ConnectionState) {
        switch newState {
        case .connected:
            HapticFeedback.success()
        case .disconnected where oldState == .connected:
            HapticFeedback.error()
        case .reconnecting(let attempt, _) where attempt == 1:
            HapticFeedback.warning()
        default:
            break
        }
    }

    private var sessionListContent: some View {
        List {
            ForEach(sessionStore.sortedSessions) { session in
                SessionRow(session: session, onDelete: {
                    sessionToDelete = session
                })
                    .listRowBackground(Color.clear)
                    .listRowSeparator(.hidden)
                    .listRowInsets(EdgeInsets(top: 4, leading: 12, bottom: 4, trailing: 12))
                    .onTapGesture {
                        if settingsStore.hapticFeedbackEnabled {
                            HapticFeedback.selection()
                        }
                        selectedSession = session
                    }
            }
        }
        .listStyle(.plain)
        .scrollContentBackground(.hidden)
        .animation(.glassSpring, value: sessionStore.sessions.count)
    }
}

// MARK: - New Session Button

struct NewSessionButton: View {
    let onTap: () -> Void

    var body: some View {
        FloatingActionButton(icon: "plus", action: onTap)
            .padding(.trailing, 20)
            .padding(.bottom, 20)
            .frame(maxWidth: .infinity, alignment: .trailing)
    }
}

// MARK: - Empty State

struct EmptySessionsView: View {
    let onCreateTapped: () -> Void

    var body: some View {
        VStack(spacing: Theme.Spacing.lg) {
            Image(systemName: "rectangle.stack.badge.plus")
                .font(.system(size: 64))
                .foregroundStyle(Theme.Colors.brand.opacity(0.6))

            VStack(spacing: Theme.Spacing.xs) {
                Text("No Sessions Yet")
                    .font(.netclodeHeadline)

                Text("Start a conversation with Claude to create your first session")
                    .font(.netclodeBody)
                    .foregroundStyle(.secondary)
                    .multilineTextAlignment(.center)
            }

            GlassButton("Start Session", icon: "plus") {
                onCreateTapped()
            }
        }
        .padding()
    }
}

// MARK: - Preview

#Preview {
    NavigationStack {
        SessionsView()
    }
    .environment(SessionStore())
    .environment(ChatStore())
    .environment(EventStore())
    .environment(TerminalStore())
    .environment(SettingsStore())
    .environment(ConnectService())
    .environment(MessageRouter.preview)
}

#Preview("With Sessions") {
    let store = SessionStore()
    store.setSessions(Session.previewList)

    return NavigationStack {
        SessionsView()
    }
    .environment(store)
    .environment(ChatStore())
    .environment(EventStore())
    .environment(TerminalStore())
    .environment(SettingsStore())
    .environment(ConnectService())
    .environment(MessageRouter.preview)
}
