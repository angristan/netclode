import SwiftUI

struct PromptSheet: View {
    @Environment(\.dismiss) private var dismiss
    @Environment(ConnectService.self) private var connectService
    @Environment(SettingsStore.self) private var settingsStore
    @Environment(SessionStore.self) private var sessionStore
    @Environment(GitHubStore.self) private var githubStore
    @Environment(ModelsStore.self) private var modelsStore
    @Environment(CopilotStore.self) private var copilotStore
    @Environment(CodexStore.self) private var codexStore

    @State private var promptText = ""
    @State private var repoURL = ""
    @State private var repoAccess: RepoAccess = .read
    @State private var isPrivateRepo = false
    @State private var selectedSdkType: SdkType = .claude
    @State private var selectedClaudeModelId: String = ModelsStore.defaultModelId
    @State private var selectedOpenCodeModelId: String = ModelsStore.defaultModelId
    @State private var selectedCopilotModelId: String = CopilotStore.defaultModelId
    @State private var selectedCodexModelId: String = CodexStore.defaultModelId
    @State private var isSubmitting = false
    @State private var canSubmit = false
    @State private var showModelDropdown = false
    @State private var showRepoDropdown = false
    @State private var showAccessDropdown = false
    @State private var tailnetAccess = false
    @FocusState private var isFocused: Bool

    /// Get available models as PickerModels based on current SDK selection
    private var availablePickerModels: [PickerModel] {
        let models: [PickerModel]
        switch selectedSdkType {
        case .claude, .opencode:
            models = modelsStore.anthropicModels.map { PickerModel.from($0) }
        case .copilot:
            models = copilotStore.models.map { PickerModel.from($0) }
        case .codex:
            models = codexStore.models.map { PickerModel.from($0) }
        }
        return models.sorted { $0.name.localizedCaseInsensitiveCompare($1.name) == .orderedAscending }
    }

    /// Binding to the appropriate model ID based on SDK type
    private var selectedModelIdBinding: Binding<String> {
        switch selectedSdkType {
        case .claude:
            return $selectedClaudeModelId
        case .opencode:
            return $selectedOpenCodeModelId
        case .copilot:
            return $selectedCopilotModelId
        case .codex:
            return $selectedCodexModelId
        }
    }

    /// Whether models are loading
    private var isLoadingModels: Bool {
        switch selectedSdkType {
        case .claude, .opencode:
            return modelsStore.isLoading
        case .copilot:
            return copilotStore.isLoadingModels
        case .codex:
            return codexStore.isLoadingModels
        }
    }

    var body: some View {
        NavigationStack {
            ScrollViewReader { scrollProxy in
                ScrollView {
                    VStack(spacing: 0) {
                    // Text input area
                    TextField(
                        "What do you want to build?",
                        text: $promptText,
                        axis: .vertical
                    )
                    .font(.netclodeBody)
                    .tint(Theme.Colors.brand)
                    .lineLimit(3...12)
                    .padding(Theme.Spacing.md)
                    .glassEffect(.regular, in: RoundedRectangle(cornerRadius: Theme.Radius.lg))
                    .padding(.horizontal, Theme.Spacing.md)
                    .padding(.top, Theme.Spacing.md)
                    .focused($isFocused)

                    // SDK and Model section
                    VStack(alignment: .leading, spacing: Theme.Spacing.sm) {
                        HStack(spacing: Theme.Spacing.xs) {
                            Image(systemName: "cpu")
                                .font(.system(size: 16))
                                .foregroundStyle(.secondary)
                            Text("Agent SDK")
                                .font(.netclodeCaption)
                                .foregroundStyle(.secondary)
                        }

                        SdkPicker(selection: $selectedSdkType)
                            .onTapGesture {
                                isFocused = false
                            }

                        // Model picker (shown for all SDK types)
                        HStack(spacing: Theme.Spacing.xs) {
                            Image(systemName: "sparkles")
                                .font(.system(size: 16))
                                .foregroundStyle(.secondary)
                            Text("Model")
                                .font(.netclodeCaption)
                                .foregroundStyle(.secondary)
                        }
                        .padding(.top, Theme.Spacing.xs)

                        ZStack {
                            // Loading state
                            if isLoadingModels && availablePickerModels.isEmpty {
                                HStack {
                                    ProgressView()
                                        .scaleEffect(0.8)
                                    Text("Loading models...")
                                        .font(.netclodeCaption)
                                        .foregroundStyle(.secondary)
                                    Spacer()
                                }
                                .padding(Theme.Spacing.sm)
                                .glassEffect(.regular, in: RoundedRectangle(cornerRadius: Theme.Radius.md))
                                .transition(.opacity)
                            }
                            
                            // Model picker (show even while loading if we have cached models)
                            if !availablePickerModels.isEmpty {
                                InlineModelPicker(
                                    selectedModelId: selectedModelIdBinding,
                                    models: availablePickerModels,
                                    isExpanded: $showModelDropdown
                                )
                                .transition(.opacity)
                            }
                        }
                        .animation(.smooth(duration: 0.2), value: isLoadingModels)
                        .animation(.smooth(duration: 0.2), value: availablePickerModels.count)
                    }
                    .padding(.horizontal, Theme.Spacing.md)
                    .padding(.top, Theme.Spacing.md)

                    // Repository section
                    VStack(alignment: .leading, spacing: Theme.Spacing.sm) {
                        HStack(spacing: Theme.Spacing.xs) {
                            Image("github-mark")
                                .resizable()
                                .scaledToFit()
                                .frame(width: 14, height: 14)
                                .foregroundStyle(.secondary)
                            Text("Repository (optional)")
                                .font(.netclodeCaption)
                                .foregroundStyle(.secondary)
                        }
                        
                        InlineRepoPicker(
                            selectedRepo: $repoURL,
                            onRepoSelected: { repo in
                                isPrivateRepo = repo.isPrivate
                            },
                            isExpanded: $showRepoDropdown,
                            onSearchFocused: {
                                withAnimation {
                                    scrollProxy.scrollTo("repoSection", anchor: .top)
                                }
                            },
                            onExpanded: {
                                isFocused = false
                            }
                        )
                    }
                    .id("repoSection")
                    .padding(.horizontal, Theme.Spacing.md)
                    .padding(.top, Theme.Spacing.md)
                    
                    // Access level section (always visible)
                    VStack(alignment: .leading, spacing: Theme.Spacing.sm) {
                        HStack(spacing: Theme.Spacing.xs) {
                            Image(systemName: "lock.shield")
                                .font(.system(size: 16))
                                .foregroundStyle(.secondary)
                            Text("GitHub Access")
                                .font(.netclodeCaption)
                                .foregroundStyle(.secondary)
                        }
                        
                        InlineAccessPicker(
                            selectedAccess: $repoAccess,
                            isExpanded: $showAccessDropdown,
                            hasRepo: !repoURL.isEmpty
                        )
                    }
                    .padding(.horizontal, Theme.Spacing.md)
                    .padding(.top, Theme.Spacing.md)
                    
                    // Network section
                    VStack(alignment: .leading, spacing: Theme.Spacing.sm) {
                        HStack(spacing: Theme.Spacing.xs) {
                            Image(systemName: "network")
                                .font(.system(size: 16))
                                .foregroundStyle(.secondary)
                            Text("Network")
                                .font(.netclodeCaption)
                                .foregroundStyle(.secondary)
                        }
                        
                        VStack(spacing: 0) {
                            Toggle(isOn: $tailnetAccess) {
                                HStack(spacing: Theme.Spacing.sm) {
                                    Image(systemName: "point.3.connected.trianglepath.dotted")
                                        .foregroundStyle(Theme.Colors.brand)
                                        .frame(width: 20)
                                    VStack(alignment: .leading, spacing: 2) {
                                        Text("Tailnet Access")
                                            .font(.netclodeBody)
                                        Text("Allow connections to Tailscale network")
                                            .font(.netclodeCaption)
                                            .foregroundStyle(.secondary)
                                    }
                                }
                            }
                            .tint(Theme.Colors.brand)
                            .padding(Theme.Spacing.sm)
                        }
                        .glassEffect(.regular, in: RoundedRectangle(cornerRadius: Theme.Radius.md))
                    }
                    .padding(.horizontal, Theme.Spacing.md)
                    .padding(.top, Theme.Spacing.md)
                    .padding(.bottom, Theme.Spacing.lg)
                }
            }
            .scrollDismissesKeyboard(.interactively)
            }
            .background(Theme.Colors.background)
            .navigationBarTitleDisplayMode(.inline)
            .toolbar {
                ToolbarItem(placement: .cancellationAction) {
                    Button {
                        if settingsStore.hapticFeedbackEnabled {
                            HapticFeedback.light()
                        }
                        dismiss()
                    } label: {
                        Image(systemName: "xmark")
                    }
                    .tint(.red)
                    .accessibilityLabel("Cancel")
                }

                ToolbarItem(placement: .principal) {
                    Text("New Session")
                        .font(.netclodeHeadline)
                }

                ToolbarSpacer(placement: .topBarTrailing)

                ToolbarItem(placement: .confirmationAction) {
                    Button {
                        submitPrompt()
                    } label: {
                        if isSubmitting {
                            ProgressView()
                                .tint(.white)
                        } else {
                            Image(systemName: "paperplane")
                                .symbolVariant(canSubmit ? .fill : .none)
                                .bold()
                        }
                    }
                    .buttonStyle(.glassProminent)
                    .buttonBorderShape(.circle)
                    .tint(Theme.Colors.brand)
                    .disabled(!canSubmit)
                    .keyboardShortcut(.return, modifiers: .command)
                    .accessibilityLabel("Send")
                }
            }
            .onAppear {
                isFocused = true
                // Restore last selected SDK and model preferences
                selectedSdkType = settingsStore.lastSelectedSdkType
                selectedClaudeModelId = settingsStore.lastSelectedClaudeModelId
                selectedOpenCodeModelId = settingsStore.lastSelectedOpenCodeModelId
                selectedCopilotModelId = settingsStore.lastSelectedCopilotModelId
                selectedCodexModelId = settingsStore.lastSelectedCodexModelId
                // Preload all models on sheet open for smooth SDK switching
                preloadAllModels()
            }
            .onChange(of: selectedSdkType) { _, newValue in
                // Close dropdown and animate the transition
                withAnimation(.smooth(duration: 0.2)) {
                    showModelDropdown = false
                }
                // Persist last selected SDK
                settingsStore.lastSelectedSdkType = newValue
            }
            .onChange(of: showModelDropdown) { _, isExpanded in
                // Dismiss keyboard when opening model dropdown
                if isExpanded {
                    isFocused = false
                }
            }
            .onChange(of: promptText) { _, newValue in
                canSubmit = !newValue.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty && !isSubmitting
            }
            .onChange(of: isSubmitting) { _, newValue in
                canSubmit = !promptText.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty && !newValue
            }
            .onChange(of: repoURL) { _, newValue in
                // Reset to read access when repo is cleared
                if newValue.isEmpty {
                    repoAccess = .read
                }
            }
            .onChange(of: selectedClaudeModelId) { _, newValue in
                settingsStore.lastSelectedClaudeModelId = newValue
            }
            .onChange(of: selectedOpenCodeModelId) { _, newValue in
                settingsStore.lastSelectedOpenCodeModelId = newValue
            }
            .onChange(of: selectedCopilotModelId) { _, newValue in
                settingsStore.lastSelectedCopilotModelId = newValue
            }
            .onChange(of: selectedCodexModelId) { _, newValue in
                settingsStore.lastSelectedCodexModelId = newValue
            }
        }
        .presentationDetents([.medium, .large])
        .presentationDragIndicator(.visible)
        .interactiveDismissDisabled(isSubmitting)
    }

    @ViewBuilder
    private func modelLabel(for model: PickerModel) -> some View {
        HStack {
            VStack(alignment: .leading, spacing: 2) {
                Text(model.name)
                if let provider = model.provider {
                    Text(provider)
                        .font(.netclodeCaption)
                        .foregroundStyle(.secondary)
                }
            }
            Spacer()
        }
    }

    private func submitPrompt() {
        let text = promptText.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !text.isEmpty else { return }

        isSubmitting = true

        if settingsStore.hapticFeedbackEnabled {
            HapticFeedback.medium()
        }

        // Store prompt text - will be associated with session when sessionCreated arrives
        sessionStore.pendingPromptText = text
        
        // Parse repo URL if provided
        let repo = repoURL.trimmingCharacters(in: .whitespacesAndNewlines)
        let repoParam = repo.isEmpty ? nil : repo
        let accessParam = repoParam != nil ? repoAccess : nil

        // SDK and model params
        let sdkParam = selectedSdkType
        let modelParam: String?
        
        switch selectedSdkType {
        case .claude:
            modelParam = selectedClaudeModelId
        case .opencode:
            modelParam = selectedOpenCodeModelId
        case .copilot:
            modelParam = selectedCopilotModelId
        case .codex:
            modelParam = selectedCodexModelId
        }
        
        // Build network config (only if tailnet access is requested)
        var networkConfig: NetworkConfig? = nil
        if tailnetAccess {
            networkConfig = NetworkConfig(
                tailnetAccess: tailnetAccess
            )
        }
        
        // Create session
        connectService.send(.sessionCreate(
            name: nil,
            repo: repoParam,
            repoAccess: accessParam,
            initialPrompt: text,
            sdkType: sdkParam,
            model: modelParam,
            copilotBackend: nil,
            networkConfig: networkConfig
        ))

        dismiss()
    }

    private func preloadAllModels() {
        // Refresh Anthropic models if stale (for Claude & OpenCode SDKs)
        if modelsStore.isCacheStale {
            Task {
                await modelsStore.fetchModels()
            }
        }
        
        // Preload Copilot models if not already loaded
        if copilotStore.models.isEmpty && !copilotStore.isLoadingModels {
            copilotStore.setLoadingModels(true)
            connectService.send(.listModels(sdkType: .copilot, copilotBackend: nil))
        }
        
        // Preload Codex models if not already loaded
        if codexStore.models.isEmpty && !codexStore.isLoadingModels {
            codexStore.setLoadingModels(true)
            connectService.send(.listModels(sdkType: .codex, copilotBackend: nil))
        }
    }
}

// MARK: - Inline Access Picker

/// Full-width inline liquid glass picker for repository access levels
struct InlineAccessPicker: View {
    @Binding var selectedAccess: RepoAccess
    @Binding var isExpanded: Bool
    var hasRepo: Bool = false  // Whether a repo is selected

    private var availableOptions: [RepoAccess] {
        return RepoAccess.allCases
    }
    
    private func isOptionDisabled(_ access: RepoAccess) -> Bool {
        // Write is only available when a repo is selected
        return access == .write && !hasRepo
    }

    var body: some View {
        VStack(spacing: 0) {
            // Collapsed state - shows selected access level
            Button {
                withAnimation(.smooth(duration: 0.25)) {
                    isExpanded.toggle()
                }
            } label: {
                HStack(spacing: Theme.Spacing.xs) {
                    Image(systemName: selectedAccess.icon)
                        .font(.system(size: 16))
                        .frame(width: 20)
                        .foregroundStyle(.secondary)
                    Text(selectedAccess.displayName)
                        .font(.netclodeBody)
                        .contentTransition(.numericText())
                    Spacer()
                    Image(systemName: "chevron.up.chevron.down")
                        .font(.system(size: 12, weight: .medium))
                        .foregroundStyle(.secondary)
                        .rotationEffect(.degrees(isExpanded ? 180 : 0))
                }
                .padding(Theme.Spacing.sm)
                .frame(maxWidth: .infinity)
                .contentShape(Rectangle())
                .animation(.smooth(duration: 0.2), value: selectedAccess)
            }
            .buttonStyle(.plain)
            .glassEffect(
                isExpanded ? .regular.tint(Theme.Colors.brand.glassTint).interactive() : .regular.interactive(),
                in: RoundedRectangle(cornerRadius: Theme.Radius.md)
            )

            // Expanded state - shows all options
            if isExpanded {
                ScrollView {
                    LazyVStack(spacing: 2) {
                        ForEach(availableOptions, id: \.self) { access in
                            let disabled = isOptionDisabled(access)
                            Button {
                                withAnimation(.smooth(duration: 0.2)) {
                                    selectedAccess = access
                                    isExpanded = false
                                }
                            } label: {
                                HStack(spacing: Theme.Spacing.xs) {
                                    Image(systemName: access == selectedAccess ? "checkmark.circle.fill" : "circle")
                                        .foregroundStyle(access == selectedAccess ? Theme.Colors.brand : .secondary)
                                        .font(.system(size: 16))
                                        .contentTransition(.symbolEffect(.replace))

                                    Image(systemName: access.icon)
                                        .font(.system(size: 14))
                                        .foregroundStyle(.secondary)

                                    VStack(alignment: .leading, spacing: 2) {
                                        Text(access.displayName)
                                            .font(.netclodeBody)
                                            .foregroundStyle(disabled ? .tertiary : .primary)
                                        Text(disabled ? "Select a repo first" : access.description)
                                            .font(.netclodeCaption)
                                            .foregroundStyle(disabled ? .tertiary : .secondary)
                                    }

                                    Spacer()
                                }
                                .opacity(disabled ? 0.5 : 1.0)
                                .padding(.horizontal, Theme.Spacing.sm)
                                .padding(.vertical, Theme.Spacing.xs)
                                .frame(maxWidth: .infinity, alignment: .leading)
                                .contentShape(Rectangle())
                            }
                            .buttonStyle(.plain)
                            .disabled(disabled)
                        }
                    }
                    .padding(.vertical, Theme.Spacing.xs)
                }
                .frame(maxHeight: 280)
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
}

// MARK: - Preview

#Preview {
    Color.clear
        .sheet(isPresented: .constant(true)) {
            PromptSheet()
                .environment(ConnectService())
                .environment(SettingsStore())
                .environment(SessionStore())
                .environment(GitHubStore())
                .environment(ModelsStore())
                .environment(CopilotStore())
                .environment(CodexStore())
        }
}
