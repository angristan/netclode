import Foundation

/// Network configuration for sandbox sessions
struct NetworkConfig: Sendable {
    /// Whether Tailnet access is enabled (default: false)
    var tailnetAccess: Bool = false
}

/// Custom VM resource allocation for sandbox sessions
/// When specified, the session bypasses the warm pool and creates a sandbox directly.
struct SandboxResources: Sendable {
    /// Number of vCPUs for the VM (1-8, limited to 50% of host)
    var vcpus: Int32
    /// Memory in MiB for the VM (512-8192, limited to 50% of host)
    var memoryMB: Int32
}

enum ClientMessage: Encodable, Sendable {
    case sessionCreate(name: String?, repos: [String]?, repoAccess: RepoAccess?, initialPrompt: String?, sdkType: SdkType?, model: String?, copilotBackend: CopilotBackend?, networkConfig: NetworkConfig?, resources: SandboxResources?)
    case sessionList
    case sessionResume(id: String)
    case sessionPause(id: String)
    case sessionDelete(id: String)
    case sessionDeleteAll
    case prompt(sessionId: String, text: String)
    case promptInterrupt(sessionId: String)
    case terminalInput(sessionId: String, data: String)
    case terminalResize(sessionId: String, cols: Int, rows: Int)
    case portExpose(sessionId: String, port: Int)
    case portUnexpose(sessionId: String, port: Int)
    // Sync messages
    case sync
    case sessionOpen(id: String, lastMessageId: String?, lastNotificationId: String?)
    // GitHub
    case githubReposList
    // Git operations
    case gitStatus(sessionId: String)
    case gitDiff(sessionId: String, file: String?)
    // Copilot
    case listModels(sdkType: SdkType, copilotBackend: CopilotBackend?)
    case getCopilotStatus
    // Snapshots
    case listSnapshots(sessionId: String)
    case restoreSnapshot(sessionId: String, snapshotId: String)
    // Repo access
    case updateRepoAccess(sessionId: String, repoAccess: RepoAccess)
    // Resource limits
    case getResourceLimits

    private enum CodingKeys: String, CodingKey {
        case type
        case name, repos, repoAccess, id, sessionId, text, data, cols, rows, port, lastMessageId, lastNotificationId, initialPrompt, file, sdkType, model, copilotBackend, snapshotId
    }

    func encode(to encoder: Encoder) throws {
        var container = encoder.container(keyedBy: CodingKeys.self)

        switch self {
        case .sessionCreate(let name, let repos, let repoAccess, let initialPrompt, let sdkType, let model, let copilotBackend, _, _):
            // Note: networkConfig and resources are handled via proto in ConnectService, not JSON encoding
            try container.encode("session.create", forKey: .type)
            try container.encodeIfPresent(name, forKey: .name)
            try container.encodeIfPresent(repos, forKey: .repos)
            try container.encodeIfPresent(repoAccess?.rawValue, forKey: .repoAccess)
            try container.encodeIfPresent(initialPrompt, forKey: .initialPrompt)
            try container.encodeIfPresent(sdkType?.rawValue, forKey: .sdkType)
            try container.encodeIfPresent(model, forKey: .model)
            try container.encodeIfPresent(copilotBackend?.rawValue, forKey: .copilotBackend)

        case .sessionList:
            try container.encode("session.list", forKey: .type)

        case .sessionResume(let id):
            try container.encode("session.resume", forKey: .type)
            try container.encode(id, forKey: .id)

        case .sessionPause(let id):
            try container.encode("session.pause", forKey: .type)
            try container.encode(id, forKey: .id)

        case .sessionDelete(let id):
            try container.encode("session.delete", forKey: .type)
            try container.encode(id, forKey: .id)

        case .sessionDeleteAll:
            try container.encode("session.deleteAll", forKey: .type)

        case .prompt(let sessionId, let text):
            try container.encode("prompt", forKey: .type)
            try container.encode(sessionId, forKey: .sessionId)
            try container.encode(text, forKey: .text)

        case .promptInterrupt(let sessionId):
            try container.encode("prompt.interrupt", forKey: .type)
            try container.encode(sessionId, forKey: .sessionId)

        case .terminalInput(let sessionId, let data):
            try container.encode("terminal.input", forKey: .type)
            try container.encode(sessionId, forKey: .sessionId)
            try container.encode(data, forKey: .data)

        case .terminalResize(let sessionId, let cols, let rows):
            try container.encode("terminal.resize", forKey: .type)
            try container.encode(sessionId, forKey: .sessionId)
            try container.encode(cols, forKey: .cols)
            try container.encode(rows, forKey: .rows)

        case .portExpose(let sessionId, let port):
            try container.encode("port.expose", forKey: .type)
            try container.encode(sessionId, forKey: .sessionId)
            try container.encode(port, forKey: .port)

        case .portUnexpose(let sessionId, let port):
            try container.encode("port.unexpose", forKey: .type)
            try container.encode(sessionId, forKey: .sessionId)
            try container.encode(port, forKey: .port)

        case .sync:
            try container.encode("sync", forKey: .type)

        case .sessionOpen(let id, let lastMessageId, let lastNotificationId):
            try container.encode("session.open", forKey: .type)
            try container.encode(id, forKey: .id)
            try container.encodeIfPresent(lastMessageId, forKey: .lastMessageId)
            try container.encodeIfPresent(lastNotificationId, forKey: .lastNotificationId)

        case .githubReposList:
            try container.encode("github.repos.list", forKey: .type)

        case .gitStatus(let sessionId):
            try container.encode("git.status", forKey: .type)
            try container.encode(sessionId, forKey: .sessionId)

        case .gitDiff(let sessionId, let file):
            try container.encode("git.diff", forKey: .type)
            try container.encode(sessionId, forKey: .sessionId)
            try container.encodeIfPresent(file, forKey: .file)

        case .listModels(let sdkType, let copilotBackend):
            try container.encode("models.list", forKey: .type)
            try container.encode(sdkType.rawValue, forKey: .sdkType)
            try container.encodeIfPresent(copilotBackend?.rawValue, forKey: .copilotBackend)

        case .getCopilotStatus:
            try container.encode("copilot.status", forKey: .type)

        case .listSnapshots(let sessionId):
            try container.encode("snapshots.list", forKey: .type)
            try container.encode(sessionId, forKey: .sessionId)

        case .restoreSnapshot(let sessionId, let snapshotId):
            try container.encode("snapshot.restore", forKey: .type)
            try container.encode(sessionId, forKey: .sessionId)
            try container.encode(snapshotId, forKey: .snapshotId)

        case .updateRepoAccess(let sessionId, let repoAccess):
            try container.encode("repo.access.update", forKey: .type)
            try container.encode(sessionId, forKey: .sessionId)
            try container.encode(repoAccess.rawValue, forKey: .repoAccess)

        case .getResourceLimits:
            try container.encode("resource.limits.get", forKey: .type)
        }
    }
}
