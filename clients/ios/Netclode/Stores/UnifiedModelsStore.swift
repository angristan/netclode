import Foundation

/// State for a single SDK type's models
struct SdkModelState: Sendable {
    var models: [CopilotModel] = []
    var isLoading: Bool = false
    var errorMessage: String? = nil
}

/// Unified store for all SDK model types - fetches via control-plane, no client caching
@MainActor
@Observable
final class UnifiedModelsStore {
    /// Models state per SDK type
    private(set) var claudeState = SdkModelState()
    private(set) var opencodeState = SdkModelState()
    private(set) var copilotState = SdkModelState()
    private(set) var codexState = SdkModelState()

    /// Copilot-specific status (auth & quota)
    private(set) var copilotStatus: CopilotStatus?
    private(set) var isLoadingCopilotStatus = false

    /// Sandbox resource limits
    private(set) var resourceLimits: ResourceLimits?
    private(set) var isLoadingResourceLimits = false

    /// Default model IDs per SDK
    static let defaultClaudeModelId = "claude-sonnet-4-5"
    static let defaultOpenCodeModelId = "anthropic/claude-sonnet-4-5"
    static let defaultCopilotModelId = "claude-sonnet-4.5"
    static let defaultCodexModelId = "gpt-5.2-codex:oauth:high"

    // MARK: - Accessors

    /// Get models for an SDK type
    func models(for sdkType: SdkType) -> [CopilotModel] {
        state(for: sdkType).models
    }

    /// Whether models are loading for an SDK type
    func isLoading(for sdkType: SdkType) -> Bool {
        state(for: sdkType).isLoading
    }

    /// Error message for an SDK type
    func errorMessage(for sdkType: SdkType) -> String? {
        state(for: sdkType).errorMessage
    }

    private func state(for sdkType: SdkType) -> SdkModelState {
        switch sdkType {
        case .claude: return claudeState
        case .opencode: return opencodeState
        case .copilot: return copilotState
        case .codex: return codexState
        }
    }

    // MARK: - Updates from server

    /// Update models from server response
    func updateModels(_ models: [CopilotModel], sdkType: SdkType) {
        switch sdkType {
        case .claude:
            claudeState.models = models
            claudeState.isLoading = false
            claudeState.errorMessage = nil
        case .opencode:
            opencodeState.models = models
            opencodeState.isLoading = false
            opencodeState.errorMessage = nil
        case .copilot:
            copilotState.models = models
            copilotState.isLoading = false
            copilotState.errorMessage = nil
        case .codex:
            codexState.models = models
            codexState.isLoading = false
            codexState.errorMessage = nil
        }
    }

    /// Set loading state for SDK type
    func setLoading(_ loading: Bool, for sdkType: SdkType) {
        switch sdkType {
        case .claude: claudeState.isLoading = loading
        case .opencode: opencodeState.isLoading = loading
        case .copilot: copilotState.isLoading = loading
        case .codex: codexState.isLoading = loading
        }
    }

    /// Set error for SDK type
    func setError(_ error: String, for sdkType: SdkType) {
        switch sdkType {
        case .claude:
            claudeState.errorMessage = error
            claudeState.isLoading = false
        case .opencode:
            opencodeState.errorMessage = error
            opencodeState.isLoading = false
        case .copilot:
            copilotState.errorMessage = error
            copilotState.isLoading = false
        case .codex:
            codexState.errorMessage = error
            codexState.isLoading = false
        }
    }

    /// Update Copilot status
    func updateCopilotStatus(_ status: CopilotStatus) {
        self.copilotStatus = status
        self.isLoadingCopilotStatus = false
    }

    /// Set Copilot status loading
    func setLoadingCopilotStatus(_ loading: Bool) {
        isLoadingCopilotStatus = loading
    }

    /// Update resource limits from server response
    func updateResourceLimits(_ limits: ResourceLimits) {
        self.resourceLimits = limits
        self.isLoadingResourceLimits = false
    }

    /// Set resource limits loading
    func setLoadingResourceLimits(_ loading: Bool) {
        isLoadingResourceLimits = loading
    }

    // MARK: - Lookups

    /// Find model by ID for a given SDK type
    func model(id: String, sdkType: SdkType) -> CopilotModel? {
        models(for: sdkType).first { $0.id == id }
    }

    /// Get default model ID for SDK type
    static func defaultModelId(for sdkType: SdkType) -> String {
        switch sdkType {
        case .claude: return defaultClaudeModelId
        case .opencode: return defaultOpenCodeModelId
        case .copilot: return defaultCopilotModelId
        case .codex: return defaultCodexModelId
        }
    }

    /// Resolve the preferred model for an SDK:
    /// 1) last used model (if still available)
    /// 2) latest versioned family match from backend list
    /// 3) static default (legacy fallback)
    func preferredModelId(for sdkType: SdkType, lastUsedModelId: String?) -> String {
        let availableModels = models(for: sdkType)
        let defaultModelId = Self.defaultModelId(for: sdkType)

        if let lastUsedModelId, availableModels.contains(where: { $0.id == lastUsedModelId }) {
            return lastUsedModelId
        }

        switch sdkType {
        case .codex:
            if let latestCodex = findLatestCodexModelId(in: availableModels) {
                return latestCodex
            }
        case .claude, .opencode, .copilot:
            if let latestClaudeOpus = findLatestClaudeOpusModelId(in: availableModels) {
                return latestClaudeOpus
            }
        }

        if availableModels.contains(where: { $0.id == defaultModelId }) {
            return defaultModelId
        }

        return defaultModelId
    }

    private static let codexEffortOrder: [String] = ["medium", "low", "high", "xhigh", "minimal"]
    private static let codexAuthOrder: [String] = ["oauth", "api"]

    private func findLatestCodexModelId(in models: [CopilotModel]) -> String? {
        struct Candidate {
            let model: CopilotModel
            let baseId: String
            let version: [Int]
            let auth: String?
            let effort: String?
        }

        let candidates: [Candidate] = models.compactMap { model in
            let (baseId, auth, effort) = parseCodexModelParts(id: model.id)
            guard let version = extractVersion(in: baseId, family: "gpt", suffix: "codex") else {
                return nil
            }
            return Candidate(model: model, baseId: baseId, version: version, auth: auth, effort: effort)
        }

        guard !candidates.isEmpty else { return nil }

        guard let bestVersion = candidates.map(\.version).max(by: isVersionLess) else {
            return nil
        }

        let sameVersion = candidates.filter { !isVersionLess($0.version, bestVersion) && !isVersionLess(bestVersion, $0.version) }
        let sameBase = sameVersion.filter { $0.baseId == sameVersion.map(\.baseId).sorted().first }
        let target = sameBase.isEmpty ? sameVersion : sameBase

        let sorted = target.sorted { lhs, rhs in
            let lhsEffortRank = Self.codexEffortOrder.firstIndex(of: lhs.effort ?? "") ?? Int.max
            let rhsEffortRank = Self.codexEffortOrder.firstIndex(of: rhs.effort ?? "") ?? Int.max
            if lhsEffortRank != rhsEffortRank { return lhsEffortRank < rhsEffortRank }

            let lhsAuthRank = Self.codexAuthOrder.firstIndex(of: lhs.auth ?? "") ?? Int.max
            let rhsAuthRank = Self.codexAuthOrder.firstIndex(of: rhs.auth ?? "") ?? Int.max
            if lhsAuthRank != rhsAuthRank { return lhsAuthRank < rhsAuthRank }

            return lhs.model.id < rhs.model.id
        }

        return sorted.first?.model.id
    }

    private func findLatestClaudeOpusModelId(in models: [CopilotModel]) -> String? {
        struct Candidate {
            let model: CopilotModel
            let version: [Int]
            let hasSuffix: Bool
        }

        let candidates: [Candidate] = models.compactMap { model in
            guard let version = extractClaudeOpusVersion(for: model) else {
                return nil
            }
            return Candidate(
                model: model,
                version: version,
                hasSuffix: model.id.contains(":")
            )
        }

        guard !candidates.isEmpty else { return nil }

        let sorted = candidates.sorted { lhs, rhs in
            if !isVersionEqual(lhs.version, rhs.version) {
                return isVersionLess(rhs.version, lhs.version)
            }
            if lhs.hasSuffix != rhs.hasSuffix {
                return !lhs.hasSuffix
            }
            return lhs.model.id < rhs.model.id
        }
        return sorted.first?.model.id
    }

    private func extractClaudeOpusVersion(for model: CopilotModel) -> [Int]? {
        let nameVersion = extractVersion(
            in: normalizeModelToken(model.name),
            family: "claude-opus",
            suffix: nil,
            maxComponents: 2
        )
        let idVersion = extractVersion(
            in: normalizeModelToken(model.id),
            family: "claude-opus",
            suffix: nil,
            maxComponents: 2
        )

        // Prefer human-facing name parse to avoid date-coded IDs dominating ranking.
        let raw = nameVersion ?? idVersion
        guard var version = raw else { return nil }

        // Drop date-like second component (e.g. 20250514 from claude-opus-4-20250514).
        if version.count >= 2 && version[1] >= 1000 {
            version = [version[0]]
        }
        return version
    }

    private func parseCodexModelParts(id: String) -> (baseId: String, auth: String?, effort: String?) {
        let parts = id.split(separator: ":").map(String.init)
        guard !parts.isEmpty else { return (id, nil, nil) }
        let baseId = parts[0]

        if parts.count >= 3 {
            return (baseId, parts[1].lowercased(), parts[2].lowercased())
        }
        if parts.count == 2 {
            let maybe = parts[1].lowercased()
            if Self.codexAuthOrder.contains(maybe) {
                return (baseId, maybe, nil)
            }
            if Self.codexEffortOrder.contains(maybe) {
                return (baseId, nil, maybe)
            }
        }
        return (baseId, nil, nil)
    }

    private func extractVersion(in rawText: String, family: String, suffix: String?, maxComponents: Int? = nil) -> [Int]? {
        let normalized = normalizeModelToken(rawText)
        let escapedFamily = NSRegularExpression.escapedPattern(for: family)
        let escapedSuffix = suffix.map(NSRegularExpression.escapedPattern(for:)) ?? ""
        let separatorClause: String
        if let maxComponents {
            let extra = max(0, maxComponents - 1)
            separatorClause = "(?:[.-][0-9]+){0,\(extra)}"
        } else {
            separatorClause = "(?:[.-][0-9]+)*"
        }
        let pattern: String

        if suffix == nil {
            pattern = "\(escapedFamily)-([0-9]+\(separatorClause))"
        } else {
            pattern = "\(escapedFamily)-([0-9]+\(separatorClause))-\(escapedSuffix)"
        }

        guard let regex = try? NSRegularExpression(pattern: pattern, options: [.caseInsensitive]) else {
            return nil
        }
        let nsText = normalized as NSString
        guard let match = regex.firstMatch(in: normalized, options: [], range: NSRange(location: 0, length: nsText.length)),
              match.numberOfRanges >= 2 else {
            return nil
        }

        let versionString = nsText.substring(with: match.range(at: 1))
        let parts = versionString
            .split(whereSeparator: { $0 == "." || $0 == "-" })
            .compactMap { Int($0) }
        return parts.isEmpty ? nil : parts
    }

    private func normalizeModelToken(_ token: String) -> String {
        token.lowercased()
            .replacingOccurrences(of: "_", with: "-")
            .replacingOccurrences(of: " ", with: "-")
            .replacingOccurrences(of: "/", with: "-")
    }

    private func isVersionLess(_ lhs: [Int], _ rhs: [Int]) -> Bool {
        let count = max(lhs.count, rhs.count)
        for idx in 0..<count {
            let left = idx < lhs.count ? lhs[idx] : 0
            let right = idx < rhs.count ? rhs[idx] : 0
            if left != right {
                return left < right
            }
        }
        return false
    }

    private func isVersionEqual(_ lhs: [Int], _ rhs: [Int]) -> Bool {
        !isVersionLess(lhs, rhs) && !isVersionLess(rhs, lhs)
    }
}
