import SwiftUI

struct PromptSheet: View {
    @Environment(\.dismiss) private var dismiss
    @Environment(ConnectService.self) private var connectService
    @Environment(SettingsStore.self) private var settingsStore
    @Environment(SessionStore.self) private var sessionStore
    @Environment(GitHubStore.self) private var githubStore
    @Environment(UnifiedModelsStore.self) private var modelsStore

    @State private var promptText = ""
    @State private var selectedRepos: [String] = []
    @State private var repoAccess: RepoAccess = .read
    @State private var selectedSdkType: SdkType = .claude
    @State private var selectedClaudeModelId = ""
    @State private var selectedOpenCodeModelId = ""
    @State private var selectedCopilotModelId = ""
    @State private var selectedCodexModelId = ""
    @State private var isSubmitting = false
    @State private var canSubmit = false
    @State private var showModelDropdown = false
    @State private var showRepoDropdown = false
    @State private var showAccessDropdown = false
    @State private var tailnetAccess = false
    @State private var customResourcesEnabled = false
    @State private var vcpus: Int32 = 0      // Initialized from server defaults in onAppear
    @State private var memoryMB: Int32 = 0   // Initialized from server defaults in onAppear
    @FocusState private var isFocused: Bool
    
    // Speech
    @State private var speechService = SpeechService()
    private var isRecording: Bool { speechService.state == .recording }
    private var isTranscribing: Bool { speechService.state == .processing }
    private var isPreparing: Bool { speechService.state == .preparingModel }

    /// Get available models as PickerModels based on current SDK selection
    private var availablePickerModels: [PickerModel] {
        let models = modelsStore.models(for: selectedSdkType).map { PickerModel.from($0) }
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
        modelsStore.isLoading(for: selectedSdkType)
    }

    /// Available vCPU options: 4 power-of-2 options up to max
    private var vcpusOptions: [Int32] {
        let max = modelsStore.resourceLimits?.maxVcpus ?? 8
        // Powers of 2: 1, 2, 4, 8 (up to max)
        return [1, 2, 4, 8].filter { $0 <= max }
    }

    /// Available memory options: 4 power-of-2 options from 2GB to max (in MB)
    private var memoryOptions: [Int32] {
        let max = modelsStore.resourceLimits?.maxMemoryMB ?? 16384
        // Powers of 2 in GB: 2, 4, 8, 16 (up to max)
        return [2048, 4096, 8192, 16384].filter { $0 <= max }
    }

    var body: some View {
        NavigationStack {
            ScrollViewReader { scrollProxy in
                ScrollView {
                    VStack(spacing: 0) {
                    // Text input area - replaces with recording UI when recording
                    ZStack {
                        // Normal text input with mic button
                        HStack(alignment: .bottom, spacing: 0) {
                            TextField(
                                "What do you want to build?",
                                text: $promptText,
                                axis: .vertical
                            )
                            .font(.netclodeBody)
                            .tint(Theme.Colors.brand)
                            .lineLimit(3...12)
                            .padding(Theme.Spacing.md)
                            .padding(.trailing, 0)
                            .focused($isFocused)
                            .blur(radius: isRecording || isTranscribing || isPreparing ? 4 : 0)
                            .opacity(isRecording || isTranscribing || isPreparing ? 0 : 1)
                            
                            // Mic button (hidden during recording)
                            Button {
                                Task {
                                    try? await speechService.startRecording()
                                }
                            } label: {
                                Circle()
                                    .fill(.white.opacity(0.001))
                                    .frame(width: 36, height: 36)
                                    .glassEffect(.regular.interactive(), in: Circle())
                                    .overlay {
                                        Image(systemName: "mic.fill")
                                            .font(.system(size: 16, weight: .medium))
                                            .foregroundStyle(.white)
                                    }
                            }
                            .padding(.trailing, 8)
                            .padding(.bottom, 8)
                            .scaleEffect(isRecording || isTranscribing || isPreparing ? 0.8 : 1)
                            .opacity(isRecording || isTranscribing || isPreparing ? 0 : 1)
                        }
                        .background(.ultraThinMaterial, in: RoundedRectangle(cornerRadius: Theme.Radius.lg))
                        
                        // Recording overlay - same size as text input
                        if isRecording || isTranscribing || isPreparing {
                            HStack(spacing: Theme.Spacing.sm) {
                                if isTranscribing || isPreparing {
                                    ProgressView()
                                        .scaleEffect(0.8)
                                    Text(isPreparing ? "Preparing..." : "Transcribing...")
                                        .font(.netclodeBody)
                                        .foregroundStyle(.secondary)
                                    Spacer()
                                } else {
                                    // Recording indicator
                                    Circle()
                                        .fill(Theme.Colors.brand)
                                        .frame(width: 10, height: 10)
                                        .modifier(PulsingModifier())
                                    
                                    // Waveform - takes full width
                                    AudioWaveformView(level: speechService.audioLevel)
                                        .frame(maxWidth: .infinity)
                                }
                                
                                // Stop button
                                Button {
                                    Task {
                                        await speechService.stopRecording()
                                        if !speechService.currentTranscript.isEmpty {
                                            promptText = speechService.currentTranscript
                                        }
                                    }
                                } label: {
                                    Circle()
                                        .fill(.white.opacity(0.001))
                                        .frame(width: 36, height: 36)
                                        .glassEffect(.regular.interactive(), in: Circle())
                                        .overlay {
                                            Image(systemName: "stop.fill")
                                                .font(.system(size: 16, weight: .semibold))
                                                .foregroundStyle(.white)
                                        }
                                }
                            }
                            .padding(.horizontal, Theme.Spacing.md)
                            .frame(maxWidth: .infinity, maxHeight: .infinity)
                            .background(.ultraThinMaterial, in: RoundedRectangle(cornerRadius: Theme.Radius.lg))
                            .overlay {
                                RoundedRectangle(cornerRadius: Theme.Radius.lg)
                                    .stroke(Theme.Colors.brand.opacity(0.6), lineWidth: 2)
                            }
                            .transition(.asymmetric(
                                insertion: .opacity.combined(with: .scale(scale: 0.95)),
                                removal: .opacity.combined(with: .scale(scale: 1.02))
                            ))
                        }
                    }
                    .animation(.spring(duration: 0.35, bounce: 0.15), value: isRecording)
                    .animation(.spring(duration: 0.35, bounce: 0.15), value: isTranscribing)
                    .animation(.spring(duration: 0.35, bounce: 0.15), value: isPreparing)
                    .padding(.horizontal, Theme.Spacing.md)
                    .padding(.top, Theme.Spacing.xs)

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
                                    isExpanded: $showModelDropdown,
                                    copilotQuota: selectedSdkType == .copilot ? modelsStore.copilotStatus?.quota : nil
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
                            Text("Repositories (optional)")
                                .font(.netclodeCaption)
                                .foregroundStyle(.secondary)
                        }

                        InlineRepoPicker(
                            selectedRepos: $selectedRepos,
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
                            hasRepo: !selectedRepos.isEmpty
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
                                    Image("tailscale-logo")
                                        .renderingMode(.template)
                                        .resizable()
                                        .scaledToFit()
                                        .foregroundStyle(.secondary)
                                        .frame(width: 20, height: 20)
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

                    // Compute Resources section
                    VStack(alignment: .leading, spacing: Theme.Spacing.sm) {
                        HStack(spacing: Theme.Spacing.xs) {
                            Image(systemName: "memorychip")
                                .font(.system(size: 16))
                                .foregroundStyle(.secondary)
                            Text("Compute Resources")
                                .font(.netclodeCaption)
                                .foregroundStyle(.secondary)
                        }

                        VStack(spacing: 0) {
                            Toggle(isOn: $customResourcesEnabled) {
                                HStack(spacing: Theme.Spacing.sm) {
                                    Image(systemName: "slider.horizontal.3")
                                        .foregroundStyle(.secondary)
                                        .frame(width: 20, height: 20)
                                    VStack(alignment: .leading, spacing: 2) {
                                        Text("Custom Resources")
                                            .font(.netclodeBody)
                                        Text("Configure VM CPU and memory")
                                            .font(.netclodeCaption)
                                            .foregroundStyle(.secondary)
                                    }
                                }
                            }
                            .tint(Theme.Colors.brand)
                            .padding(Theme.Spacing.sm)

                            if customResourcesEnabled {
                                Divider()
                                    .padding(.horizontal, Theme.Spacing.sm)

                                // vCPUs picker
                                VStack(alignment: .leading, spacing: Theme.Spacing.xs) {
                                    HStack {
                                        Text("vCPUs")
                                            .font(.netclodeBody)
                                        Spacer()
                                        Text("\(vcpus)")
                                            .font(.netclodeBody)
                                            .monospacedDigit()
                                            .foregroundStyle(.secondary)
                                    }
                                    Picker("vCPUs", selection: $vcpus) {
                                        ForEach(vcpusOptions, id: \.self) { value in
                                            Text("\(value)").tag(value)
                                        }
                                    }
                                    .pickerStyle(.segmented)
                                }
                                .padding(.horizontal, Theme.Spacing.sm)
                                .padding(.vertical, Theme.Spacing.xs)

                                // Memory picker
                                VStack(alignment: .leading, spacing: Theme.Spacing.xs) {
                                    HStack {
                                        Text("Memory")
                                            .font(.netclodeBody)
                                        Spacer()
                                        Text(formatMemory(memoryMB))
                                            .font(.netclodeBody)
                                            .monospacedDigit()
                                            .foregroundStyle(.secondary)
                                    }
                                    Picker("Memory", selection: $memoryMB) {
                                        ForEach(memoryOptions, id: \.self) { value in
                                            Text(formatMemory(value)).tag(value)
                                        }
                                    }
                                    .pickerStyle(.segmented)
                                }
                                .padding(.horizontal, Theme.Spacing.sm)
                                .padding(.vertical, Theme.Spacing.xs)
                                .padding(.bottom, Theme.Spacing.xs)
                            }
                        }
                        .glassEffect(.regular, in: RoundedRectangle(cornerRadius: Theme.Radius.md))
                        .animation(.smooth(duration: 0.25), value: customResourcesEnabled)
                    }
                    .padding(.horizontal, Theme.Spacing.md)
                    .padding(.top, Theme.Spacing.md)
                    .padding(.bottom, Theme.Spacing.lg)
                }
            }
            .scrollDismissesKeyboard(.interactively)
            }
            .background(Theme.Colors.background)
            .navigationBarHidden(true)
            .safeAreaInset(edge: .top) {
                // Custom header with circular glass buttons
                HStack {
                    // Cancel button
                    Button {
                        if settingsStore.hapticFeedbackEnabled {
                            HapticFeedback.light()
                        }
                        dismiss()
                    } label: {
                        Circle()
                            .fill(.white.opacity(0.001))
                            .frame(width: 44, height: 44)
                            .glassEffect(.regular.interactive(), in: Circle())
                            .overlay {
                                Image(systemName: "xmark")
                                    .font(.system(size: 17, weight: .semibold))
                                    .foregroundStyle(.white)
                            }
                    }
                    .accessibilityLabel("Cancel")
                    
                    Spacer()
                    
                    Text("New Session")
                        .font(.netclodeHeadline)
                    
                    Spacer()
                    
                    // Send button
                    Button {
                        submitPrompt()
                    } label: {
                        Circle()
                            .fill(.white.opacity(0.001))
                            .frame(width: 44, height: 44)
                            .glassEffect(.regular.interactive().tint(Theme.Colors.brand.glassTint), in: Circle())
                            .overlay {
                                if isSubmitting {
                                    ProgressView()
                                        .tint(.white)
                                } else {
                                    Image(systemName: "paperplane")
                                        .symbolVariant(canSubmit ? .fill : .none)
                                        .font(.system(size: 17, weight: .semibold))
                                        .foregroundStyle(canSubmit ? .white : .secondary)
                                }
                            }
                    }
                    .disabled(!canSubmit)
                    .keyboardShortcut(.return, modifiers: .command)
                    .accessibilityLabel("Send")
                }
                .padding(.horizontal, Theme.Spacing.md)
                .padding(.vertical, Theme.Spacing.sm)
            }
            .onAppear {
                isFocused = true
                initializePreferredModels()
                // Initialize resource defaults from server (if already loaded)
                if let limits = modelsStore.resourceLimits {
                    vcpus = limits.defaultVcpus
                    memoryMB = limits.defaultMemoryMB
                }
                // Preload all models on sheet open for smooth SDK switching
                preloadAllModels()
            }
            .onChange(of: selectedSdkType) { _, _ in
                // Close dropdown and animate the transition
                withAnimation(.smooth(duration: 0.2)) {
                    showModelDropdown = false
                }

                reconcileSelectedModel(for: selectedSdkType)
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
            .onChange(of: selectedRepos) { _, newValue in
                // Reset to read access when repos are cleared
                if newValue.isEmpty {
                    repoAccess = .read
                }
            }
            .onChange(of: modelsStore.resourceLimits) { _, newLimits in
                // Set defaults from server when limits arrive (only if custom resources not yet enabled)
                if let limits = newLimits, !customResourcesEnabled {
                    vcpus = limits.defaultVcpus
                    memoryMB = limits.defaultMemoryMB
                }
            }
            .onChange(of: modelsStore.models(for: .claude).map(\.id)) { _, _ in
                reconcileSelectedModel(for: .claude)
            }
            .onChange(of: modelsStore.models(for: .opencode).map(\.id)) { _, _ in
                reconcileSelectedModel(for: .opencode)
            }
            .onChange(of: modelsStore.models(for: .copilot).map(\.id)) { _, _ in
                reconcileSelectedModel(for: .copilot)
            }
            .onChange(of: modelsStore.models(for: .codex).map(\.id)) { _, _ in
                reconcileSelectedModel(for: .codex)
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

    private func formatMemory(_ mb: Int32) -> String {
        if mb >= 1024 {
            let gb = Double(mb) / 1024.0
            if gb == Double(Int(gb)) {
                return "\(Int(gb)) GB"
            } else {
                return String(format: "%.1f GB", gb)
            }
        }
        return "\(mb) MB"
    }

    private func initializePreferredModels() {
        setSelectedModelId(
            modelsStore.preferredModelId(
                for: .claude,
                lastUsedModelId: settingsStore.lastUsedModelId(for: .claude)
            ),
            for: .claude
        )
        setSelectedModelId(
            modelsStore.preferredModelId(
                for: .opencode,
                lastUsedModelId: settingsStore.lastUsedModelId(for: .opencode)
            ),
            for: .opencode
        )
        setSelectedModelId(
            modelsStore.preferredModelId(
                for: .copilot,
                lastUsedModelId: settingsStore.lastUsedModelId(for: .copilot)
            ),
            for: .copilot
        )
        setSelectedModelId(
            modelsStore.preferredModelId(
                for: .codex,
                lastUsedModelId: settingsStore.lastUsedModelId(for: .codex)
            ),
            for: .codex
        )
    }

    private func currentSelectedModelId(for sdkType: SdkType) -> String {
        switch sdkType {
        case .claude:
            return selectedClaudeModelId
        case .opencode:
            return selectedOpenCodeModelId
        case .copilot:
            return selectedCopilotModelId
        case .codex:
            return selectedCodexModelId
        }
    }

    private func setSelectedModelId(_ modelId: String, for sdkType: SdkType) {
        switch sdkType {
        case .claude:
            selectedClaudeModelId = modelId
        case .opencode:
            selectedOpenCodeModelId = modelId
        case .copilot:
            selectedCopilotModelId = modelId
        case .codex:
            selectedCodexModelId = modelId
        }
    }

    private func reconcileSelectedModel(for sdkType: SdkType) {
        let current = currentSelectedModelId(for: sdkType)
        let models = modelsStore.models(for: sdkType)
        let hasPersistedLastUsed = settingsStore.lastUsedModelId(for: sdkType) != nil
        let isCurrentValid = !current.isEmpty && models.contains(where: { $0.id == current })
        if hasPersistedLastUsed && isCurrentValid {
            return
        }

        let preferred = modelsStore.preferredModelId(
            for: sdkType,
            lastUsedModelId: settingsStore.lastUsedModelId(for: sdkType)
        )
        if preferred == current {
            return
        }
        setSelectedModelId(preferred, for: sdkType)
    }

    private func submitPrompt() {
        let text = promptText.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !text.isEmpty else { return }

        isSubmitting = true
        sessionStore.clearPendingCreationError()

        if settingsStore.hapticFeedbackEnabled {
            HapticFeedback.medium()
        }

        // Store prompt text - will be associated with session when sessionCreated arrives
        sessionStore.pendingPromptText = text

        // Parse repo URL if provided
        let reposParam = selectedRepos
            .map { $0.trimmingCharacters(in: .whitespacesAndNewlines) }
            .filter { !$0.isEmpty }
        let accessParam = reposParam.isEmpty ? nil : repoAccess

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

        if let modelParam {
            settingsStore.setLastUsedModelId(modelParam, for: selectedSdkType)
        }

        // Build network config (only if tailnet access is requested)
        var networkConfig: NetworkConfig? = nil
        if tailnetAccess {
            networkConfig = NetworkConfig(
                tailnetAccess: tailnetAccess
            )
        }

        // Build resources config (only if custom resources is enabled)
        var resources: SandboxResources? = nil
        if customResourcesEnabled {
            resources = SandboxResources(
                vcpus: vcpus,
                memoryMB: memoryMB
            )
        }

        // Create session
        connectService.send(.sessionCreate(
            name: nil,
            repos: reposParam.isEmpty ? nil : reposParam,
            repoAccess: accessParam,
            initialPrompt: text,
            sdkType: sdkParam,
            model: modelParam,
            copilotBackend: nil,
            networkConfig: networkConfig,
            resources: resources
        ))

        dismiss()
    }

    private func preloadAllModels() {
        // Request models for all SDK types via control-plane (no client caching)
        for sdkType in SdkType.allCases {
            if modelsStore.models(for: sdkType).isEmpty && !modelsStore.isLoading(for: sdkType) {
                modelsStore.setLoading(true, for: sdkType)
                connectService.send(.listModels(sdkType: sdkType, copilotBackend: nil))
            }
        }
        // Request Copilot quota status
        if modelsStore.copilotStatus == nil && !modelsStore.isLoadingCopilotStatus {
            modelsStore.setLoadingCopilotStatus(true)
            connectService.send(.getCopilotStatus)
        }
        // Request resource limits
        if modelsStore.resourceLimits == nil && !modelsStore.isLoadingResourceLimits {
            modelsStore.setLoadingResourceLimits(true)
            connectService.send(.getResourceLimits)
        }
    }
}

// MARK: - Inline Access Picker

/// Full-width inline liquid glass picker for repository access levels
struct InlineAccessPicker: View {
    @Binding var selectedAccess: RepoAccess
    @Binding var isExpanded: Bool
    var hasRepo: Bool = false  // Whether repos are selected

    private var availableOptions: [RepoAccess] {
        return RepoAccess.allCases
    }

    private func isOptionDisabled(_ access: RepoAccess) -> Bool {
        // Write is only available when a repo is selected
        return access == .write && !hasRepo
    }

    var body: some View {
        VStack(spacing: 0) {
            // Collapsed state - shows selected access level or "None" when no repo
            Button {
                guard hasRepo else { return }
                withAnimation(.smooth(duration: 0.25)) {
                    isExpanded.toggle()
                }
            } label: {
                HStack(spacing: Theme.Spacing.xs) {
                    Image(systemName: hasRepo ? selectedAccess.icon : "eye")
                        .font(.system(size: 14))
                        .frame(width: 20)
                        .foregroundStyle(.secondary)
                    Text(hasRepo ? selectedAccess.displayName : "Read only")
                        .font(.netclodeBody)
                    Spacer()
                    Text(hasRepo ? selectedAccess.description : "No token · Select repositories")
                        .font(.netclodeCaption)
                        .foregroundStyle(.tertiary)
                    if hasRepo {
                        Image(systemName: "chevron.up.chevron.down")
                            .font(.system(size: 12, weight: .medium))
                            .foregroundStyle(.secondary)
                            .rotationEffect(.degrees(isExpanded ? 180 : 0))
                    }
                }
                .padding(Theme.Spacing.sm)
                .frame(maxWidth: .infinity)
                .contentShape(Rectangle())
                .animation(.smooth(duration: 0.2), value: selectedAccess)
            }
            .buttonStyle(.plain)
            .disabled(!hasRepo)
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
                                        .frame(width: 20)
                                        .foregroundStyle(.secondary)

                                    Text(access.displayName)
                                        .font(.netclodeBody)
                                        .foregroundStyle(disabled ? .tertiary : .primary)
                                    Spacer()
                                    Text(disabled ? "Select repositories first" : access.description)
                                        .font(.netclodeCaption)
                                        .foregroundStyle(disabled ? .tertiary : .secondary)
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
                .environment(UnifiedModelsStore())
        }
}
