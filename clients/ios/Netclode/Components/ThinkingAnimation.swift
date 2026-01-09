import SwiftUI

// MARK: - Morphing Symbols Thinking

struct MorphingSymbolsThinking: View {
    @State private var currentIndex = 0
    
    private let symbols = ["<>", "{}", "[]", "()"]
    
    var body: some View {
        HStack {
            Text(symbols[currentIndex])
                .font(.system(size: 16, weight: .semibold, design: .monospaced))
                .foregroundStyle(Theme.Colors.brand)
                .contentTransition(.numericText())

            Text("Thinking...")
                .font(.system(size: 13, weight: .medium, design: .rounded))
                .foregroundStyle(Theme.Colors.secondaryText)
            
            Spacer()
        }
        .padding(.horizontal, Theme.Spacing.md)
        .padding(.vertical, Theme.Spacing.sm)
        .glassEffect(.regular, in: RoundedRectangle(cornerRadius: Theme.Radius.md))
        .onAppear {
            Timer.scheduledTimer(withTimeInterval: 0.6, repeats: true) { _ in
                withAnimation(.smooth) {
                    currentIndex = (currentIndex + 1) % symbols.count
                }
            }
        }
    }
}

// MARK: - Preview

#Preview {
    VStack(spacing: 20) {
        MorphingSymbolsThinking()
            .padding(.horizontal)
    }
    .frame(maxWidth: .infinity, maxHeight: .infinity)
    .background(Theme.Colors.background)
}

#Preview("Dark") {
    VStack(spacing: 20) {
        MorphingSymbolsThinking()
            .padding(.horizontal)
    }
    .frame(maxWidth: .infinity, maxHeight: .infinity)
    .background(Theme.Colors.background)
    .preferredColorScheme(.dark)
}
