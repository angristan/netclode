import SwiftUI

struct ChatInputBar: View {
    @Binding var text: String
    let isProcessing: Bool
    var isFocused: FocusState<Bool>.Binding
    let onSend: () -> Void
    let onInterrupt: () -> Void

    private let inputHeight: CGFloat = 44
    private let maxHeight: CGFloat = 100

    private var canSend: Bool {
        !text.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty
    }

    var body: some View {
        HStack(alignment: .bottom, spacing: Theme.Spacing.xs) {
            // Text input
            ZStack(alignment: .leading) {
                if text.isEmpty {
                    Text("Reply...")
                        .foregroundStyle(.tertiary)
                        .padding(.horizontal, Theme.Spacing.sm)
                }

                TextEditor(text: $text)
                    .focused(isFocused)
                    .scrollContentBackground(.hidden)
                    .frame(minHeight: inputHeight, maxHeight: maxHeight)
                    .fixedSize(horizontal: false, vertical: true)
                    .padding(.horizontal, Theme.Spacing.xs)
            }
            .font(.netclodeBody)
            .glassEffect(.regular, in: RoundedRectangle(cornerRadius: inputHeight / 2))

            // Send/Stop button
            Group {
                if isProcessing {
                    // Stop button
                    Button {
                        onInterrupt()
                    } label: {
                        Image(systemName: "stop.fill")
                            .font(.system(size: 14, weight: .semibold))
                            .foregroundStyle(.white)
                            .frame(width: inputHeight, height: inputHeight)
                            .glassEffect(
                                .regular.interactive().tint(Theme.Colors.error.glassTint),
                                in: Circle()
                            )
                    }
                    .transition(.scale.combined(with: .opacity))
                } else {
                    // Send button
                    Button {
                        onSend()
                    } label: {
                        Image(systemName: "arrow.up")
                            .font(.system(size: 14, weight: .semibold))
                            .foregroundStyle(canSend ? .white : .secondary)
                            .frame(width: inputHeight, height: inputHeight)
                            .glassEffect(
                                canSend
                                    ? .regular.interactive().tint(Theme.Colors.brand.glassTint)
                                    : .regular.interactive(),
                                in: Circle()
                            )
                    }
                    .disabled(!canSend)
                    .transition(.scale.combined(with: .opacity))
                }
            }
            .animation(.snappy, value: isProcessing)
        }
        .padding(.horizontal, Theme.Spacing.sm)
        .padding(.vertical, Theme.Spacing.xs)
    }
}

// MARK: - Streaming Indicator

struct StreamingIndicator: View {
    var body: some View {
        MorphingSymbolsThinking()
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
