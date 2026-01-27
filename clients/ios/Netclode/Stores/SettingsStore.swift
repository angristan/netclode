import Foundation
import SwiftUI

@MainActor
@Observable
final class SettingsStore {
    var serverURL: String {
        didSet {
            UserDefaults.standard.set(serverURL, forKey: "netclode_server_url")
        }
    }

    var preferredColorScheme: ColorScheme? {
        didSet {
            let value: String? = switch preferredColorScheme {
            case .light: "light"
            case .dark: "dark"
            case nil: nil
            @unknown default: nil
            }
            UserDefaults.standard.set(value, forKey: "netclode_color_scheme")
        }
    }

    var hapticFeedbackEnabled: Bool {
        didSet {
            UserDefaults.standard.set(hapticFeedbackEnabled, forKey: "netclode_haptic_feedback")
        }
    }

    /// Optional Connect protocol port override. When empty, uses default port 3001
    /// or derives from serverURL (e.g., :3000 -> :3001 for local dev).
    var connectPort: String {
        didSet {
            UserDefaults.standard.set(connectPort, forKey: "netclode_connect_port")
        }
    }

    // MARK: - Last Selected SDK & Model Preferences

    /// Last selected SDK type
    var lastSelectedSdkType: SdkType {
        didSet {
            UserDefaults.standard.set(lastSelectedSdkType.rawValue, forKey: "netclode_last_sdk_type")
        }
    }

    /// Last selected model ID for Claude SDK
    var lastSelectedClaudeModelId: String {
        didSet {
            UserDefaults.standard.set(lastSelectedClaudeModelId, forKey: "netclode_last_claude_model")
        }
    }

    /// Last selected model ID for OpenCode SDK
    var lastSelectedOpenCodeModelId: String {
        didSet {
            UserDefaults.standard.set(lastSelectedOpenCodeModelId, forKey: "netclode_last_opencode_model")
        }
    }

    /// Last selected model ID for Copilot SDK
    var lastSelectedCopilotModelId: String {
        didSet {
            UserDefaults.standard.set(lastSelectedCopilotModelId, forKey: "netclode_last_copilot_model")
        }
    }

    /// Last selected model ID for Codex SDK
    var lastSelectedCodexModelId: String {
        didSet {
            UserDefaults.standard.set(lastSelectedCodexModelId, forKey: "netclode_last_codex_model")
        }
    }

    init() {
        serverURL = UserDefaults.standard.string(forKey: "netclode_server_url") ?? ""
        connectPort = UserDefaults.standard.string(forKey: "netclode_connect_port") ?? ""

        if let scheme = UserDefaults.standard.string(forKey: "netclode_color_scheme") {
            preferredColorScheme = scheme == "light" ? .light : scheme == "dark" ? .dark : nil
        } else {
            preferredColorScheme = nil
        }

        hapticFeedbackEnabled = UserDefaults.standard.object(forKey: "netclode_haptic_feedback") as? Bool ?? true

        // Load last selected SDK and model preferences
        if let sdkRawValue = UserDefaults.standard.string(forKey: "netclode_last_sdk_type"),
           let sdk = SdkType(rawValue: sdkRawValue) {
            lastSelectedSdkType = sdk
        } else {
            lastSelectedSdkType = .claude
        }

        lastSelectedClaudeModelId = UserDefaults.standard.string(forKey: "netclode_last_claude_model") ?? ModelsStore.defaultModelId
        lastSelectedOpenCodeModelId = UserDefaults.standard.string(forKey: "netclode_last_opencode_model") ?? ModelsStore.defaultModelId
        lastSelectedCopilotModelId = UserDefaults.standard.string(forKey: "netclode_last_copilot_model") ?? CopilotStore.defaultModelId
        lastSelectedCodexModelId = UserDefaults.standard.string(forKey: "netclode_last_codex_model") ?? CodexStore.defaultModelId
    }
}
