import SwiftUI

/// Unified model for the picker (works across all providers)
struct PickerModel: Identifiable, Hashable {
    let id: String
    let name: String
    let provider: String?
    let supportsVision: Bool
    let supportsReasoning: Bool
    let inputCost: Double?
    let outputCost: Double?
    let reasoningEffort: String?  // For Codex: "High", "Med", "Low", "xHigh"

    /// Create from CopilotModel (unified model from control-plane)
    static func from(_ model: CopilotModel) -> PickerModel {
        PickerModel(
            id: model.id,
            name: model.name,
            provider: model.provider,
            supportsVision: model.capabilities.contains("vision"),
            supportsReasoning: model.capabilities.contains("reasoning"),
            inputCost: nil,
            outputCost: nil,
            reasoningEffort: model.reasoningEffort
        )
    }
}

struct ModelPickerSheet: View {
    @Environment(\.dismiss) private var dismiss
    @Binding var selectedModelId: String
    let models: [PickerModel]
    let title: String
    let isLoading: Bool

    var body: some View {
        NavigationStack {
            Group {
                if isLoading && models.isEmpty {
                    VStack(spacing: Theme.Spacing.md) {
                        ProgressView()
                        Text("Loading models...")
                            .font(.netclodeCaption)
                            .foregroundStyle(.secondary)
                    }
                    .frame(maxWidth: .infinity, maxHeight: .infinity)
                } else if models.isEmpty {
                    ContentUnavailableView(
                        "No Models Available",
                        systemImage: "sparkles",
                        description: Text("Could not load models")
                    )
                } else {
                    ScrollView {
                        LazyVStack(spacing: Theme.Spacing.xs) {
                            ForEach(models) { model in
                                ModelRowGlass(
                                    model: model,
                                    isSelected: model.id == selectedModelId,
                                    onTap: {
                                        withAnimation(.smooth(duration: 0.3)) {
                                            selectedModelId = model.id
                                        }
                                        DispatchQueue.main.asyncAfter(deadline: .now() + 0.15) {
                                            dismiss()
                                        }
                                    }
                                )
                            }
                        }
                        .padding(.horizontal, Theme.Spacing.md)
                        .padding(.vertical, Theme.Spacing.sm)
                    }
                }
            }
            .background(Theme.Colors.background)
            .navigationTitle(title)
            .navigationBarTitleDisplayMode(.inline)
            .toolbar {
                ToolbarItem(placement: .cancellationAction) {
                    Button("Cancel") {
                        dismiss()
                    }
                }
            }
        }
        .presentationDetents([.medium, .large])
        .presentationDragIndicator(.visible)
    }
}

struct ModelRow: View {
    let model: PickerModel
    let isSelected: Bool

    var body: some View {
        HStack(spacing: Theme.Spacing.sm) {
            // Selection indicator
            Image(systemName: isSelected ? "checkmark.circle.fill" : "circle")
                .foregroundStyle(isSelected ? Theme.Colors.brand : .secondary)
                .font(.system(size: 20))
                .contentTransition(.symbolEffect(.replace))

            // Provider logo (inferred from model name)
            ProviderLogo(provider: model.provider, modelName: model.name, size: 20)
                .foregroundStyle(.secondary)

            // Model info
            VStack(alignment: .leading, spacing: 2) {
                // Model name + reasoning effort (purple)
                HStack(spacing: 4) {
                    Text(model.name)
                        .font(.netclodeBody)
                        .foregroundStyle(.primary)
                    
                    if let effort = model.reasoningEffort {
                        Text(effort)
                            .font(.netclodeBody.weight(.semibold))
                            .foregroundStyle(Theme.Colors.brand)
                    }
                }

                // Capabilities and cost row
                HStack(spacing: Theme.Spacing.sm) {
                    if model.supportsVision {
                        Label("Vision", systemImage: "eye")
                            .font(.netclodeCaption)
                            .foregroundStyle(.secondary)
                    }

                    if model.supportsReasoning {
                        Label("Reasoning", systemImage: "brain")
                            .font(.netclodeCaption)
                            .foregroundStyle(.secondary)
                    }

                    if let input = model.inputCost, let output = model.outputCost {
                        Text("$\(formatCost(input))/$\(formatCost(output))")
                            .font(.netclodeCaption)
                            .foregroundStyle(.tertiary)
                    }
                }
            }

            Spacer()
            
            // Right-aligned provider
            if let provider = model.provider {
                Text(provider)
                    .font(.netclodeCaption)
                    .foregroundStyle(.tertiary)
            }
        }
        .padding(Theme.Spacing.sm)
        .contentShape(Rectangle())
    }

    private func formatCost(_ cost: Double) -> String {
        if cost < 1.0 {
            return String(format: "%.2f", cost)
        } else {
            return String(format: "%.0f", cost)
        }
    }
}

/// Model row with liquid glass effect
struct ModelRowGlass: View {
    let model: PickerModel
    let isSelected: Bool
    let onTap: () -> Void

    var body: some View {
        Button(action: onTap) {
            ModelRow(model: model, isSelected: isSelected)
        }
        .buttonStyle(.plain)
        .glassEffect(
            isSelected
                ? .regular.tint(Theme.Colors.brand.glassTint).interactive()
                : .regular.interactive(),
            in: RoundedRectangle(cornerRadius: Theme.Radius.md)
        )
    }
}

/// A provider section with its models
private struct ProviderSection: Identifiable {
    let id: String  // provider name
    let provider: String
    let models: [PickerModel]
}

/// Full-width inline liquid glass picker with provider sections
struct InlineModelPicker: View {
    @Binding var selectedModelId: String
    let models: [PickerModel]
    @Binding var isExpanded: Bool
    var copilotQuota: CopilotPremiumQuota? = nil

    private var selectedModel: PickerModel? {
        models.first { $0.id == selectedModelId }
    }

    /// Effective model to display (auto-selects first if default doesn't match)
    private var effectiveModel: PickerModel? {
        selectedModel ?? models.first
    }

    /// Models grouped by provider
    private var providerSections: [ProviderSection] {
        let grouped = Dictionary(grouping: models) { $0.provider ?? "Other" }
        return grouped.map { ProviderSection(id: $0.key, provider: $0.key, models: $0.value) }
            .sorted { $0.provider.localizedCaseInsensitiveCompare($1.provider) == .orderedAscending }
    }

    var body: some View {
        VStack(spacing: 0) {
            // Collapsed state - shows selected model
            Button {
                withAnimation(.smooth(duration: 0.25)) {
                    isExpanded.toggle()
                }
            } label: {
                HStack(spacing: Theme.Spacing.xs) {
                    if let model = effectiveModel {
                        ProviderLogo(provider: model.provider, modelName: model.name, size: 16)
                            .frame(width: 20)
                            .foregroundStyle(.secondary)

                        Text(model.name)
                            .font(.netclodeBody)
                            .foregroundStyle(.primary)
                            .contentTransition(.numericText())

                        if let effort = model.reasoningEffort {
                            Text(effort)
                                .font(.netclodeBody.weight(.semibold))
                                .foregroundStyle(Theme.Colors.brand)
                                .contentTransition(.numericText())
                        }

                        Spacer()

                        if let provider = model.provider {
                            Text(provider)
                                .font(.netclodeCaption)
                                .foregroundStyle(.tertiary)
                                .contentTransition(.numericText())
                        }
                    } else {
                        Text("Select a model")
                            .font(.netclodeBody)
                            .foregroundStyle(.secondary)
                        Spacer()
                    }

                    Image(systemName: "chevron.up.chevron.down")
                        .font(.system(size: 12, weight: .medium))
                        .foregroundStyle(.secondary)
                        .rotationEffect(.degrees(isExpanded ? 180 : 0))
                }
                .padding(Theme.Spacing.sm)
                .frame(maxWidth: .infinity)
                .contentShape(Rectangle())
                .animation(.smooth(duration: 0.2), value: effectiveModel?.id)
            }
            .buttonStyle(.plain)
            .glassEffect(
                isExpanded ? .regular.tint(Theme.Colors.brand.glassTint).interactive() : .regular.interactive(),
                in: RoundedRectangle(cornerRadius: Theme.Radius.md)
            )
            .onAppear {
                // Auto-select if current selection doesn't match any available model
                if selectedModel == nil {
                    withAnimation(.smooth(duration: 0.2)) {
                        selectedModelId = findBestModel(in: models, preferring: selectedModelId)?.id ?? selectedModelId
                    }
                }
            }
            .onChange(of: models) { _, newModels in
                // Re-validate selection when models change
                if !newModels.contains(where: { $0.id == selectedModelId }) {
                    withAnimation(.smooth(duration: 0.2)) {
                        selectedModelId = findBestModel(in: newModels, preferring: selectedModelId)?.id ?? selectedModelId
                    }
                }
            }

            // Expanded state - shows all options grouped by provider
            if isExpanded {
                ScrollViewReader { proxy in
                    ScrollView {
                        LazyVStack(spacing: Theme.Spacing.sm) {
                            ForEach(providerSections) { section in
                                VStack(alignment: .leading, spacing: 2) {
                                    // Section header
                                    sectionHeader(for: section)

                                    // Models in this section
                                    ForEach(section.models) { model in
                                        modelRow(for: model)
                                            .id(model.id)
                                    }
                                }
                            }
                        }
                        .padding(.vertical, Theme.Spacing.xs)
                    }
                    .onAppear {
                        scrollToSelectedModel(using: proxy, animated: false)
                    }
                    .onChange(of: selectedModelId) { _, _ in
                        scrollToSelectedModel(using: proxy, animated: true)
                    }
                }
                .frame(maxHeight: 320)
                .glassEffect(.regular, in: RoundedRectangle(cornerRadius: Theme.Radius.md))
                .transition(.asymmetric(
                    insertion: .opacity.combined(with: .scale(scale: 0.95, anchor: .top)),
                    removal: .opacity
                ))
                .padding(.top, Theme.Spacing.xs)
            }
        }
        .animation(.smooth(duration: 0.25), value: isExpanded)
    }

    @ViewBuilder
    private func sectionHeader(for section: ProviderSection) -> some View {
        HStack(spacing: Theme.Spacing.xs) {
            ProviderLogo(provider: section.provider, modelName: nil, size: 14)
                .foregroundStyle(.secondary)

            Text(section.provider)
                .font(.netclodeCaption.weight(.semibold))
                .foregroundStyle(.secondary)

            // Show quota for Copilot section (premium requests)
            if section.provider == "Copilot", let quota = copilotQuota {
                Spacer()
                Text("\(quota.remaining)/\(quota.limit) premium reqs left")
                    .font(.netclodeCaption)
                    .foregroundStyle(quota.remaining < 50 ? .orange : .secondary)
            }
        }
        .padding(.horizontal, Theme.Spacing.sm)
        .padding(.top, Theme.Spacing.xs)
    }

    @ViewBuilder
    private func modelRow(for model: PickerModel) -> some View {
        Button {
            withAnimation(.smooth(duration: 0.2)) {
                selectedModelId = model.id
                isExpanded = false
            }
        } label: {
            HStack(spacing: Theme.Spacing.xs) {
                Image(systemName: model.id == selectedModelId ? "checkmark.circle.fill" : "circle")
                    .foregroundStyle(model.id == selectedModelId ? Theme.Colors.brand : .secondary)
                    .font(.system(size: 16))
                    .contentTransition(.symbolEffect(.replace))

                Text(model.name)
                    .font(.netclodeBody)
                    .foregroundStyle(.primary)

                if let effort = model.reasoningEffort {
                    Text(effort)
                        .font(.netclodeBody.weight(.semibold))
                        .foregroundStyle(Theme.Colors.brand)
                }

                Spacer()

                if let provider = model.provider {
                    Text(provider)
                        .font(.netclodeCaption)
                        .foregroundStyle(.tertiary)
                }
            }
            .padding(.horizontal, Theme.Spacing.sm)
            .padding(.vertical, Theme.Spacing.xs)
            .frame(maxWidth: .infinity, alignment: .leading)
            .contentShape(Rectangle())
        }
        .buttonStyle(.plain)
    }

    private func scrollToSelectedModel(using proxy: ScrollViewProxy, animated: Bool) {
        guard models.contains(where: { $0.id == selectedModelId }) else { return }
        let action = {
            proxy.scrollTo(selectedModelId, anchor: .center)
        }
        if animated {
            withAnimation(.smooth(duration: 0.2)) {
                action()
            }
        } else {
            action()
        }
    }

    /// Find the best matching model when exact ID match isn't found
    private func findBestModel(in models: [PickerModel], preferring preferredId: String) -> PickerModel? {
        // First try exact match
        if let exact = models.first(where: { $0.id == preferredId }) {
            return exact
        }

        // Try to find a model matching the preferred pattern (e.g., "sonnet-4.5" or "sonnet 4.5")
        let preferredLower = preferredId.lowercased()

        // Extract key parts from preferred ID (e.g., "claude-sonnet-4.5" -> ["sonnet", "4.5"])
        let keyParts = preferredLower.components(separatedBy: CharacterSet(charactersIn: "-. "))
            .filter { $0.count > 1 && $0 != "claude" && $0 != "gpt" && $0 != "anthropic" }

        // Find model whose name contains all key parts
        if !keyParts.isEmpty {
            if let match = models.first(where: { model in
                let nameLower = model.name.lowercased()
                return keyParts.allSatisfy { nameLower.contains($0) }
            }) {
                return match
            }
        }

        // Fall back to first model
        return models.first
    }

}

/// A button that shows the selected model and opens the picker
struct ModelPickerButton: View {
    let selectedModel: PickerModel?
    let placeholder: String
    let isLoading: Bool
    let action: () -> Void

    var body: some View {
        Button(action: action) {
            HStack(spacing: Theme.Spacing.xs) {
                if isLoading {
                    ProgressView()
                        .scaleEffect(0.8)
                    Spacer()
                } else if let model = selectedModel {
                    ProviderLogo(provider: model.provider, modelName: model.name, size: 18)
                        .foregroundStyle(.secondary)

                    Text(model.name)
                        .font(.netclodeBody)
                        .foregroundStyle(.primary)
                    
                    if let effort = model.reasoningEffort {
                        Text(effort)
                            .font(.netclodeBody.weight(.semibold))
                            .foregroundStyle(Theme.Colors.brand)
                    }
                    
                    Spacer()
                    
                    if let provider = model.provider {
                        Text(provider)
                            .font(.netclodeCaption)
                            .foregroundStyle(.tertiary)
                    }
                } else {
                    Text(placeholder)
                        .font(.netclodeBody)
                        .foregroundStyle(.secondary)
                    Spacer()
                }

                Image(systemName: "chevron.up.chevron.down")
                    .font(.system(size: 12, weight: .medium))
                    .foregroundStyle(.secondary)
            }
            .padding(Theme.Spacing.sm)
            .frame(maxWidth: .infinity)
            .glassEffect(.regular.interactive(), in: .rect(cornerRadius: Theme.Radius.md))
        }
        .buttonStyle(.plain)
    }

}

#Preview("Model Picker Sheet") {
    ModelPickerSheet(
        selectedModelId: .constant("claude-sonnet-4-20250514"),
        models: [
            PickerModel(id: "claude-sonnet-4-20250514", name: "Claude Sonnet 4", provider: "Anthropic", supportsVision: true, supportsReasoning: true, inputCost: 3.0, outputCost: 15.0, reasoningEffort: nil),
            PickerModel(id: "codex-mini:oauth:high", name: "Codex Mini", provider: "ChatGPT", supportsVision: false, supportsReasoning: false, inputCost: nil, outputCost: nil, reasoningEffort: "High"),
            PickerModel(id: "codex-mini:oauth:med", name: "Codex Mini", provider: "ChatGPT", supportsVision: false, supportsReasoning: false, inputCost: nil, outputCost: nil, reasoningEffort: "Med"),
            PickerModel(id: "codex-mini:api:low", name: "Codex Mini", provider: "OpenAI", supportsVision: false, supportsReasoning: false, inputCost: nil, outputCost: nil, reasoningEffort: "Low"),
        ],
        title: "Select Model",
        isLoading: false
    )
}

#Preview("Model Picker Button") {
    VStack(spacing: 20) {
        ModelPickerButton(
            selectedModel: PickerModel(id: "claude-sonnet-4", name: "Claude Sonnet 4", provider: "Anthropic", supportsVision: true, supportsReasoning: true, inputCost: nil, outputCost: nil, reasoningEffort: nil),
            placeholder: "Select a model",
            isLoading: false,
            action: {}
        )

        ModelPickerButton(
            selectedModel: PickerModel(id: "codex-mini:oauth:high", name: "Codex Mini", provider: "ChatGPT", supportsVision: false, supportsReasoning: false, inputCost: nil, outputCost: nil, reasoningEffort: "High"),
            placeholder: "Select a model",
            isLoading: false,
            action: {}
        )

        ModelPickerButton(
            selectedModel: nil,
            placeholder: "Select a model",
            isLoading: true,
            action: {}
        )
    }
    .padding()
    .background(Theme.Colors.background)
}
