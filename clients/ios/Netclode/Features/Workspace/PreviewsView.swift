import SwiftUI

struct PreviewsView: View {
    let sessionId: String

    @Environment(EventStore.self) private var eventStore
    @Environment(ConnectService.self) private var connectService

    @State private var showExposePortSheet = false
    @State private var portToExpose = ""
    @State private var isContentVisible = false

    /// Currently active exposed ports derived from port_exposed/port_unexposed event history.
    var activePortEvents: [PortExposedEvent] {
        var byPort: [Int: PortExposedEvent] = [:]
        for event in eventStore.events(for: sessionId) {
            switch event {
            case .portExposed(let e):
                byPort[e.port] = e
            case .portUnexposed(let e):
                byPort.removeValue(forKey: e.port)
            default:
                break
            }
        }
        return byPort.values.sorted { $0.port < $1.port }
    }

    var body: some View {
        ZStack(alignment: .bottomTrailing) {
            if activePortEvents.isEmpty {
                ContentUnavailableView {
                    Label("No Previews", systemImage: "globe")
                } description: {
                    Text("Exposed ports will appear here")
                }
            } else {
                ScrollView {
                    LazyVStack(spacing: Theme.Spacing.md) {
                        ForEach(activePortEvents) { event in
                            PreviewCard(event: event) { port in
                                connectService.send(.portUnexpose(sessionId: sessionId, port: port))
                            }
                        }
                    }
                    .padding()
                    .padding(.bottom, 80) // Space for FAB
                }
            }

            FloatingActionButton(icon: "plus", tint: .cyan) {
                showExposePortSheet = true
            }
            .padding()
        }
        .opacity(isContentVisible ? 1 : 0)
        .task(id: sessionId) {
            isContentVisible = false
            try? await Task.sleep(for: .milliseconds(50))
            withAnimation(.easeOut(duration: 0.10)) {
                isContentVisible = true
            }
        }
        .sheet(isPresented: $showExposePortSheet) {
            ExposePortSheet(
                portText: $portToExpose,
                onExpose: { port in
                    connectService.send(.portExpose(sessionId: sessionId, port: port))
                    showExposePortSheet = false
                    portToExpose = ""
                }
            )
        }
    }
}

// MARK: - Preview Card

struct PreviewCard: View {
    let event: PortExposedEvent
    let onRemove: (Int) -> Void

    var body: some View {
        GlassCard {
            VStack(alignment: .leading, spacing: Theme.Spacing.sm) {
                HStack {
                    Image(systemName: "globe")
                        .font(.title2)
                        .foregroundStyle(.cyan)

                    VStack(alignment: .leading, spacing: 2) {
                        Text(verbatim: "Port \(event.port)")
                            .font(.netclodeHeadline)

                        if let process = event.process {
                            Text(process)
                                .font(.netclodeCaption)
                                .foregroundStyle(.secondary)
                        }
                    }

                    Spacer()

                    if let url = event.previewUrl {
                        Menu {
                            if let link = URL(string: url) {
                                Link(destination: link) {
                                    Label("Open in Safari", systemImage: "safari")
                                }
                            }
                            Button {
                                UIPasteboard.general.string = url
                            } label: {
                                Label("Copy URL", systemImage: "doc.on.doc")
                            }
                            Button(role: .destructive) {
                                onRemove(event.port)
                            } label: {
                                Label("Remove Port", systemImage: "trash")
                            }
                        } label: {
                            HStack(spacing: 4) {
                                Text("Open")
                                    .font(.system(size: 13, weight: .medium))
                                Image(systemName: "chevron.down")
                                    .font(.system(size: 10))
                            }
                            .foregroundStyle(.cyan)
                            .padding(.horizontal, 12)
                            .padding(.vertical, 6)
                            .background(.cyan.opacity(0.15), in: Capsule())
                        }
                    }
                }

                if let url = event.previewUrl {
                    Text(url)
                        .font(.netclodeCaption)
                        .foregroundStyle(.secondary)
                        .lineLimit(1)
                        .truncationMode(.middle)
                }
            }
        }
    }
}

// MARK: - Preview

#Preview {
    NavigationStack {
        PreviewsView(sessionId: "test")
    }
    .environment(EventStore())
    .environment(ConnectService())
}
