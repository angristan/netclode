import SwiftUI

struct ChatInputBar: View {
    @Binding var text: String
    let isProcessing: Bool
    var isFocused: FocusState<Bool>.Binding
    let onSend: () -> Void
    let onInterrupt: () -> Void
    
    /// Whether the connection is usable (if false, messages will be queued)
    var isConnected: Bool = true
    /// Whether there's already a queued message (only one allowed)
    var hasQueuedMessage: Bool = false

    /// Speech service for voice input
    @State private var speechService = SpeechService()
    
    private let buttonSize: CGFloat = 44
    private let minInputHeight: CGFloat = 22
    private let maxInputHeight: CGFloat = 120

    private var hasText: Bool {
        !text.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty
    }
    
    private var canSend: Bool {
        if !isConnected && hasQueuedMessage {
            return false
        }
        return hasText
    }
    
    private var willQueue: Bool { !isConnected }
    
    // Speech states
    private var isRecording: Bool { speechService.state == .recording }
    private var isTranscribing: Bool { speechService.state == .processing }
    private var isPreparing: Bool { speechService.state == .preparingModel }
    
    /// What to show on the right button
    private enum RightButtonMode {
        case mic       // Empty text, not recording
        case send      // Has text
        case stop      // Recording
        case loading   // Transcribing/preparing
        case interrupt // Agent processing
    }
    
    private var rightButtonMode: RightButtonMode {
        if isProcessing {
            return .interrupt
        } else if isRecording {
            return .stop
        } else if isTranscribing || isPreparing {
            return .loading
        } else if hasText {
            return .send
        } else {
            return .mic
        }
    }

    var body: some View {
        HStack(alignment: .bottom, spacing: 8) {
            // Input container
            inputContainer
            
            // Button outside
            rightButton
        }
        .padding(.horizontal, 12)
        .padding(.top, 10)
        .padding(.bottom, 16)
        .animation(.smooth(duration: 0.2), value: isRecording)
        .animation(.smooth(duration: 0.2), value: hasText)
    }
    
    // MARK: - Input Container
    
    @ViewBuilder
    private var inputContainer: some View {
        ZStack(alignment: .leading) {
            // Always show text input (keeps keyboard visible)
            textInputView
            
            // Overlay recording waveform when recording
            if isRecording {
                recordingOverlay
            }
            
            // Overlay transcribing state
            if isTranscribing || isPreparing {
                transcribingOverlay
            }
        }
        .frame(minHeight: buttonSize)
        .adaptiveGlassInteractive(in: RoundedRectangle(cornerRadius: 22))
        .overlay {
            if isRecording {
                RoundedRectangle(cornerRadius: 22)
                    .stroke(Theme.Colors.brand, lineWidth: 2)
            }
        }
    }
    
    // MARK: - Recording Overlay (Waveform)
    
    private var recordingOverlay: some View {
        HStack(spacing: Theme.Spacing.sm) {
            // Recording indicator
            Circle()
                .fill(Theme.Colors.brand)
                .frame(width: 8, height: 8)
                .modifier(PulsingModifier())
            
            // Real waveform visualization - takes full width
            AudioWaveformView(level: speechService.audioLevel)
        }
        .padding(.horizontal, 16)
        .frame(height: buttonSize)
        .frame(maxWidth: .infinity)
        .background(Theme.Colors.background.opacity(0.95))
    }
    
    // MARK: - Transcribing Overlay
    
    private var transcribingOverlay: some View {
        HStack(spacing: Theme.Spacing.sm) {
            ProgressView()
                .scaleEffect(0.8)
            
            Text(isPreparing ? "Preparing..." : "Transcribing...")
                .font(.netclodeBody)
                .foregroundStyle(.secondary)
            
            Spacer(minLength: 0)
        }
        .padding(.horizontal, 16)
        .frame(height: buttonSize)
        .frame(maxWidth: .infinity)
        .background(Theme.Colors.background.opacity(0.95))
    }
    
    // MARK: - Text Input View
    
    private var textInputView: some View {
        TextField(placeholderText, text: $text, axis: .vertical)
            .font(.netclodeBody)
            .focused(isFocused)
            .tint(Theme.Colors.brand)
            .lineLimit(1...5)
            .padding(.horizontal, 16)
            .padding(.vertical, 12)
    }
    
    private var placeholderText: String {
        if willQueue {
            return "Reply (queued)..."
        } else {
            return "Reply..."
        }
    }
    
    // MARK: - Right Button
    
    private var rightButton: some View {
        ZStack {
            // Mic button
            Button {
                Task { await startRecording() }
            } label: {
                Circle()
                    .fill(.white.opacity(0.001))
                    .frame(width: buttonSize, height: buttonSize)
                    .glassEffect(.regular.interactive(), in: Circle())
                    .overlay {
                        Image(systemName: "mic.fill")
                            .font(.system(size: 17, weight: .medium))
                            .foregroundStyle(.white)
                    }
            }
            .opacity(rightButtonMode == .mic ? 1 : 0)
            .scaleEffect(rightButtonMode == .mic ? 1 : 0.5)
            
            // Send button
            Button(action: onSend) {
                Circle()
                    .fill(.white.opacity(0.001))
                    .frame(width: buttonSize, height: buttonSize)
                    .glassEffect(
                        canSend ? .regular.interactive().tint((willQueue ? Color.orange : Theme.Colors.brand).glassTint) : .regular.interactive(),
                        in: Circle()
                    )
                    .overlay {
                        Image(systemName: "arrow.up")
                            .font(.system(size: 17, weight: .semibold))
                            .foregroundStyle(.white)
                    }
            }
            .disabled(!canSend || rightButtonMode != .send)
            .keyboardShortcut(.return, modifiers: .command)
            .opacity(rightButtonMode == .send ? 1 : 0)
            .scaleEffect(rightButtonMode == .send ? 1 : 0.5)
            
            // Stop recording button
            Button {
                Task { await stopRecording() }
            } label: {
                Circle()
                    .fill(.white.opacity(0.001))
                    .frame(width: buttonSize, height: buttonSize)
                    .glassEffect(.regular.interactive().tint(Theme.Colors.brand.glassTint), in: Circle())
                    .overlay {
                        Image(systemName: "stop.fill")
                            .font(.system(size: 17, weight: .semibold))
                            .foregroundStyle(.white)
                    }
            }
            .opacity(rightButtonMode == .stop ? 1 : 0)
            .scaleEffect(rightButtonMode == .stop ? 1 : 0.5)
            
            // Loading indicator
            Circle()
                .fill(.white.opacity(0.001))
                .frame(width: buttonSize, height: buttonSize)
                .glassEffect(.regular.interactive(), in: Circle())
                .overlay {
                    ProgressView()
                        .tint(.white)
                }
                .opacity(rightButtonMode == .loading ? 1 : 0)
                .scaleEffect(rightButtonMode == .loading ? 1 : 0.5)
            
            // Interrupt agent button
            Button(action: onInterrupt) {
                Circle()
                    .fill(.white.opacity(0.001))
                    .frame(width: buttonSize, height: buttonSize)
                    .glassEffect(.regular.interactive().tint(Theme.Colors.error.glassTint), in: Circle())
                    .overlay {
                        Image(systemName: "stop.fill")
                            .font(.system(size: 17, weight: .semibold))
                            .foregroundStyle(.white)
                    }
            }
            .opacity(rightButtonMode == .interrupt ? 1 : 0)
            .scaleEffect(rightButtonMode == .interrupt ? 1 : 0.5)
        }
        .frame(width: buttonSize, height: buttonSize)
        .animation(.spring(response: 0.3, dampingFraction: 0.7), value: rightButtonMode)
    }
    
    // MARK: - Actions
    
    private func startRecording() async {
        do {
            try await speechService.startRecording()
        } catch {
            // Error logged by SpeechService
        }
    }
    
    private func stopRecording() async {
        await speechService.stopRecording()
        let transcript = speechService.currentTranscript
        if !transcript.isEmpty {
            text = transcript
        }
    }
}

// MARK: - Audio Waveform View

struct AudioWaveformView: View {
    var level: Float
    
    private let barCount = 40
    private let barWidth: CGFloat = 3
    private let barSpacing: CGFloat = 2
    
    // Keep history of levels for waveform visualization
    @State private var levelHistory: [CGFloat] = Array(repeating: 0.15, count: 40)
    
    var body: some View {
        GeometryReader { geo in
            HStack(spacing: barSpacing) {
                ForEach(0..<levelHistory.count, id: \.self) { index in
                    RoundedRectangle(cornerRadius: 1.5)
                        .fill(Theme.Colors.brand)
                        .frame(width: barWidth, height: max(4, levelHistory[index] * 20))
                }
            }
            .frame(maxWidth: .infinity, maxHeight: .infinity, alignment: .center)
        }
        .frame(height: 24)
        .onChange(of: level) { _, newLevel in
            withAnimation(.linear(duration: 0.05)) {
                levelHistory.removeFirst()
                levelHistory.append(CGFloat(max(0.15, newLevel)))
            }
        }
    }
}

// MARK: - Streaming Indicator

struct StreamingIndicator: View {
    @State private var animatingDot = 0
    @State private var animationTimer: Timer?

    var body: some View {
        HStack(alignment: .top, spacing: Theme.Spacing.sm) {
            // Avatar
            Image(systemName: "brain.head.profile")
                .font(.system(size: TypeScale.body, weight: .medium))
                .foregroundStyle(.white)
                .frame(width: 28, height: 28)
                .adaptiveGlass(tint: Theme.Colors.brand, in: Circle())

            // Typing indicator
            HStack(spacing: 4) {
                ForEach(0..<3, id: \.self) { index in
                    Circle()
                        .fill(Theme.Colors.brand)
                        .frame(width: 8, height: 8)
                        .offset(y: animatingDot == index ? -4 : 0)
                }
            }
            .padding(.horizontal, Theme.Spacing.md)
            .padding(.vertical, Theme.Spacing.sm)
            .adaptiveGlass(in: RoundedRectangle(cornerRadius: Theme.Radius.lg))

            Spacer()
        }
        .onAppear {
            startAnimation()
        }
        .onDisappear {
            stopAnimation()
        }
    }

    private func startAnimation() {
        // Invalidate any existing timer first
        animationTimer?.invalidate()
        animationTimer = Timer.scheduledTimer(withTimeInterval: 0.3, repeats: true) { _ in
            MainActor.assumeIsolated {
                withAnimation(.bouncy) {
                    animatingDot = (animatingDot + 1) % 3
                }
            }
        }
    }

    private func stopAnimation() {
        animationTimer?.invalidate()
        animationTimer = nil
    }
}

// MARK: - Preview

#Preview {
    VStack {
        Spacer()

        ChatInputBar(
            text: .constant(""),
            isProcessing: false,
            isFocused: FocusState<Bool>().projectedValue,
            onSend: {},
            onInterrupt: {}
        )
    }
    .background(Theme.Colors.background)
}

#Preview("With Text") {
    VStack {
        Spacer()

        ChatInputBar(
            text: .constant("Hello, this is a test message"),
            isProcessing: false,
            isFocused: FocusState<Bool>().projectedValue,
            onSend: {},
            onInterrupt: {}
        )
    }
    .background(Theme.Colors.background)
}

#Preview("Processing") {
    VStack {
        StreamingIndicator()
            .padding()

        Spacer()

        ChatInputBar(
            text: .constant("Hello"),
            isProcessing: true,
            isFocused: FocusState<Bool>().projectedValue,
            onSend: {},
            onInterrupt: {}
        )
    }
    .background(Theme.Colors.background)
}
