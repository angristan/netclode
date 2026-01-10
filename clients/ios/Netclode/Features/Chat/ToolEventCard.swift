import SwiftUI

/// A card that displays a tool invocation in Claude Code CLI style
struct ToolEventCard: View {
    let event: AgentEvent
    let endEvent: ToolEndEvent?

    @State private var isExpanded = false

    private var isRunning: Bool {
        endEvent == nil
    }

    private var isSuccess: Bool {
        endEvent?.isSuccess ?? true
    }

    private var toolName: String {
        switch event {
        case .toolStart(let e): e.tool
        case .toolEnd(let e): e.tool
        default: "Tool"
        }
    }

    private var toolInput: [String: AnyCodableValue]? {
        if case .toolStart(let e) = event {
            return e.input
        }
        return nil
    }

    var body: some View {
        VStack(alignment: .leading, spacing: 0) {
            // Header row
            Button {
                withAnimation(.snappy(duration: 0.2)) {
                    isExpanded.toggle()
                }
            } label: {
                HStack(spacing: Theme.Spacing.sm) {
                    // Tool badge
                    toolBadge

                    // Summary
                    Text(summaryText)
                        .font(.netclodeMonospacedSmall)
                        .foregroundStyle(.secondary)
                        .lineLimit(1)
                        .truncationMode(.middle)

                    Spacer()

                    // Status indicator
                    statusIndicator

                    // Expand chevron
                    Image(systemName: "chevron.right")
                        .font(.system(size: 10, weight: .semibold))
                        .foregroundStyle(.tertiary)
                        .rotationEffect(.degrees(isExpanded ? 90 : 0))
                }
                .padding(.horizontal, Theme.Spacing.sm)
                .padding(.vertical, Theme.Spacing.xs)
            }
            .buttonStyle(.plain)

            // Expanded content
            if isExpanded {
                expandedContent
                    .padding(.horizontal, Theme.Spacing.sm)
                    .padding(.bottom, Theme.Spacing.sm)
            }
        }
        .background(Theme.Colors.codeBackground.opacity(0.5))
        .clipShape(RoundedRectangle(cornerRadius: Theme.Radius.sm))
    }

    // MARK: - Tool Badge

    @ViewBuilder
    private var toolBadge: some View {
        HStack(spacing: 4) {
            Image(systemName: toolIcon)
                .font(.system(size: 10, weight: .semibold))
            Text(toolName)
                .font(.system(size: 11, weight: .semibold, design: .monospaced))
        }
        .foregroundStyle(badgeColor)
        .padding(.horizontal, 8)
        .padding(.vertical, 4)
        .background(badgeColor.opacity(0.15))
        .clipShape(Capsule())
    }

    private var toolIcon: String {
        switch toolName.lowercased() {
        case "read": return "doc.text"
        case "write": return "square.and.pencil"
        case "edit": return "pencil"
        case "bash": return "terminal"
        case "glob": return "folder.badge.gearshape"
        case "grep": return "magnifyingglass"
        case "task": return "arrow.triangle.branch"
        case "webfetch": return "globe"
        case "websearch": return "magnifyingglass.circle"
        case "todowrite": return "checklist"
        default: return "wrench"
        }
    }

    private var badgeColor: Color {
        if !isSuccess {
            return Theme.Colors.error
        }
        switch toolName.lowercased() {
        case "read", "glob", "grep": return .blue
        case "write", "edit": return .orange
        case "bash": return .green
        case "task": return .purple
        case "webfetch", "websearch": return .cyan
        default: return Theme.Colors.brand
        }
    }

    // MARK: - Status Indicator

    @ViewBuilder
    private var statusIndicator: some View {
        if isRunning {
            ProgressView()
                .scaleEffect(0.6)
        } else if isSuccess {
            Image(systemName: "checkmark.circle.fill")
                .font(.system(size: 14))
                .foregroundStyle(Theme.Colors.success)
        } else {
            Image(systemName: "xmark.circle.fill")
                .font(.system(size: 14))
                .foregroundStyle(Theme.Colors.error)
        }
    }

    // MARK: - Summary Text

    private var summaryText: String {
        guard let input = toolInput else {
            return ""
        }

        // Extract the most relevant parameter based on tool type
        switch toolName.lowercased() {
        case "read":
            if let path = input["file_path"]?.description {
                return formatPath(path)
            }
        case "write":
            if let path = input["file_path"]?.description {
                return formatPath(path)
            }
        case "edit":
            if let path = input["file_path"]?.description {
                return formatPath(path)
            }
        case "bash":
            if let cmd = input["command"]?.description {
                return cmd.prefix(50) + (cmd.count > 50 ? "..." : "")
            }
        case "glob":
            if let pattern = input["pattern"]?.description {
                return pattern
            }
        case "grep":
            if let pattern = input["pattern"]?.description {
                return pattern
            }
        case "webfetch":
            if let url = input["url"]?.description {
                return url
            }
        case "websearch":
            if let query = input["query"]?.description {
                return query
            }
        case "task":
            if let desc = input["description"]?.description {
                return desc
            }
        default:
            break
        }

        // Fallback: show first string parameter
        for (_, value) in input {
            if case .string(let s) = value, !s.isEmpty {
                return String(s.prefix(40)) + (s.count > 40 ? "..." : "")
            }
        }

        return ""
    }

    private func formatPath(_ path: String) -> String {
        // Show just the filename or last 2 components
        let components = path.split(separator: "/")
        if components.count <= 2 {
            return path
        }
        return ".../" + components.suffix(2).joined(separator: "/")
    }

    // MARK: - Expanded Content

    @ViewBuilder
    private var expandedContent: some View {
        VStack(alignment: .leading, spacing: Theme.Spacing.xs) {
            Divider()
                .padding(.bottom, Theme.Spacing.xs)

            // Input section
            if let input = toolInput, !input.isEmpty {
                ExpandableSection(title: "INPUT") {
                    ForEach(Array(input.keys.sorted()), id: \.self) { key in
                        if let value = input[key] {
                            InputRow(key: key, value: formatValue(value))
                        }
                    }
                }
            }

            // Output/Result section
            if let end = endEvent {
                if let result = end.result, !result.isEmpty {
                    ExpandableSection(title: "OUTPUT", defaultExpanded: false) {
                        ScrollView(.horizontal, showsIndicators: false) {
                            Text(result)
                                .font(.netclodeMonospacedSmall)
                                .foregroundStyle(.secondary)
                        }
                    }
                }

                if let error = end.error, !error.isEmpty {
                    ExpandableSection(title: "ERROR", defaultExpanded: true) {
                        Text(error)
                            .font(.netclodeMonospacedSmall)
                            .foregroundStyle(Theme.Colors.error)
                    }
                }
            }
        }
    }

    private func formatValue(_ value: AnyCodableValue) -> String {
        switch value {
        case .string(let s):
            return s
        case .bool(let b):
            return b ? "true" : "false"
        case .int(let i):
            return String(i)
        case .double(let d):
            return String(d)
        case .array(let arr):
            return "[\(arr.map { formatValue($0) }.joined(separator: ", "))]"
        case .dictionary(let dict):
            return "{\(dict.map { "\($0): \(formatValue($1))" }.joined(separator: ", "))}"
        case .null:
            return "null"
        }
    }
}

// MARK: - Supporting Views

private struct InputRow: View {
    let key: String
    let value: String

    var body: some View {
        HStack(alignment: .top, spacing: Theme.Spacing.xs) {
            Text(key)
                .font(.system(size: 10, weight: .medium, design: .monospaced))
                .foregroundStyle(.tertiary)
                .frame(minWidth: 60, alignment: .trailing)

            Text(value)
                .font(.netclodeMonospacedSmall)
                .foregroundStyle(.secondary)
                .lineLimit(3)
                .truncationMode(.tail)
        }
    }
}

private struct ExpandableSection<Content: View>: View {
    let title: String
    var defaultExpanded: Bool = true
    @ViewBuilder let content: () -> Content

    @State private var isExpanded: Bool = true

    init(title: String, defaultExpanded: Bool = true, @ViewBuilder content: @escaping () -> Content) {
        self.title = title
        self.defaultExpanded = defaultExpanded
        self.content = content
        self._isExpanded = State(initialValue: defaultExpanded)
    }

    var body: some View {
        VStack(alignment: .leading, spacing: Theme.Spacing.xxs) {
            Button {
                withAnimation(.snappy(duration: 0.15)) {
                    isExpanded.toggle()
                }
            } label: {
                HStack(spacing: 4) {
                    Image(systemName: "chevron.right")
                        .font(.system(size: 8, weight: .bold))
                        .rotationEffect(.degrees(isExpanded ? 90 : 0))

                    Text(title)
                        .font(.system(size: 9, weight: .semibold))
                        .tracking(0.5)
                }
                .foregroundStyle(.tertiary)
            }
            .buttonStyle(.plain)

            if isExpanded {
                content()
                    .padding(.leading, Theme.Spacing.sm)
            }
        }
    }
}

// MARK: - Command Event Card

struct CommandEventCard: View {
    let startEvent: CommandStartEvent?
    let endEvent: CommandEndEvent?

    @State private var isExpanded = false

    private var isRunning: Bool {
        endEvent == nil
    }

    private var isSuccess: Bool {
        endEvent?.isSuccess ?? true
    }

    private var command: String {
        startEvent?.command ?? endEvent?.command ?? ""
    }

    var body: some View {
        VStack(alignment: .leading, spacing: 0) {
            // Header row
            Button {
                withAnimation(.snappy(duration: 0.2)) {
                    isExpanded.toggle()
                }
            } label: {
                HStack(spacing: Theme.Spacing.sm) {
                    // Bash badge
                    HStack(spacing: 4) {
                        Image(systemName: "terminal")
                            .font(.system(size: 10, weight: .semibold))
                        Text("Bash")
                            .font(.system(size: 11, weight: .semibold, design: .monospaced))
                    }
                    .foregroundStyle(.green)
                    .padding(.horizontal, 8)
                    .padding(.vertical, 4)
                    .background(Color.green.opacity(0.15))
                    .clipShape(Capsule())

                    // Command preview
                    Text("$ " + command.prefix(40))
                        .font(.netclodeMonospacedSmall)
                        .foregroundStyle(.secondary)
                        .lineLimit(1)

                    Spacer()

                    // Exit code if finished
                    if let end = endEvent {
                        Text("exit \(end.exitCode)")
                            .font(.system(size: 10, weight: .medium, design: .monospaced))
                            .foregroundStyle(isSuccess ? Theme.Colors.success : Theme.Colors.error)
                    }

                    // Status indicator
                    if isRunning {
                        ProgressView()
                            .scaleEffect(0.6)
                    } else if isSuccess {
                        Image(systemName: "checkmark.circle.fill")
                            .font(.system(size: 14))
                            .foregroundStyle(Theme.Colors.success)
                    } else {
                        Image(systemName: "xmark.circle.fill")
                            .font(.system(size: 14))
                            .foregroundStyle(Theme.Colors.error)
                    }

                    // Expand chevron
                    Image(systemName: "chevron.right")
                        .font(.system(size: 10, weight: .semibold))
                        .foregroundStyle(.tertiary)
                        .rotationEffect(.degrees(isExpanded ? 90 : 0))
                }
                .padding(.horizontal, Theme.Spacing.sm)
                .padding(.vertical, Theme.Spacing.xs)
            }
            .buttonStyle(.plain)

            // Expanded content
            if isExpanded {
                VStack(alignment: .leading, spacing: Theme.Spacing.xs) {
                    Divider()

                    // Full command
                    Text("$ " + command)
                        .font(.netclodeMonospacedSmall)
                        .foregroundStyle(.primary)
                        .textSelection(.enabled)

                    if let cwd = startEvent?.cwd {
                        Text("in \(cwd)")
                            .font(.system(size: 10, design: .monospaced))
                            .foregroundStyle(.tertiary)
                    }

                    // Output
                    if let output = endEvent?.output, !output.isEmpty {
                        Divider()
                        ScrollView {
                            Text(output)
                                .font(.netclodeMonospacedSmall)
                                .foregroundStyle(.secondary)
                                .frame(maxWidth: .infinity, alignment: .leading)
                        }
                        .frame(maxHeight: 200)
                    }
                }
                .padding(.horizontal, Theme.Spacing.sm)
                .padding(.bottom, Theme.Spacing.sm)
            }
        }
        .background(Theme.Colors.codeBackground.opacity(0.5))
        .clipShape(RoundedRectangle(cornerRadius: Theme.Radius.sm))
    }
}

// MARK: - File Change Card

struct FileChangeCard: View {
    let event: FileChangeEvent

    var body: some View {
        HStack(spacing: Theme.Spacing.sm) {
            // File action badge
            HStack(spacing: 4) {
                Image(systemName: event.action.systemImage)
                    .font(.system(size: 10, weight: .semibold))
                Text(event.action.displayName)
                    .font(.system(size: 11, weight: .semibold, design: .monospaced))
            }
            .foregroundStyle(actionColor)
            .padding(.horizontal, 8)
            .padding(.vertical, 4)
            .background(actionColor.opacity(0.15))
            .clipShape(Capsule())

            // File path
            Text(event.fileName)
                .font(.netclodeMonospacedSmall)
                .foregroundStyle(.secondary)
                .lineLimit(1)

            Spacer()

            // Line changes
            if let added = event.linesAdded, added > 0 {
                Text("+\(added)")
                    .font(.system(size: 11, weight: .medium, design: .monospaced))
                    .foregroundStyle(Theme.Colors.success)
            }
            if let removed = event.linesRemoved, removed > 0 {
                Text("-\(removed)")
                    .font(.system(size: 11, weight: .medium, design: .monospaced))
                    .foregroundStyle(Theme.Colors.error)
            }
        }
        .padding(.horizontal, Theme.Spacing.sm)
        .padding(.vertical, Theme.Spacing.xs)
        .background(Theme.Colors.codeBackground.opacity(0.5))
        .clipShape(RoundedRectangle(cornerRadius: Theme.Radius.sm))
    }

    private var actionColor: Color {
        switch event.action {
        case .create: return Theme.Colors.success
        case .edit: return .orange
        case .delete: return Theme.Colors.error
        }
    }
}

// MARK: - Thinking Card

struct ThinkingCard: View {
    let event: ThinkingEvent
    @State private var isExpanded = false

    var body: some View {
        VStack(alignment: .leading, spacing: Theme.Spacing.xs) {
            // Header row with icon and status
            HStack(spacing: Theme.Spacing.sm) {
                Image(systemName: "brain.head.profile")
                    .font(.system(size: 12))
                    .foregroundStyle(Theme.Colors.brandLight)
                    .opacity(event.partial ? 1.0 : 0.7)

                Text("Thinking")
                    .font(.netclodeCaption)
                    .fontWeight(.medium)
                    .foregroundStyle(Theme.Colors.brandLight)

                if event.partial {
                    // Pulsing indicator for streaming
                    Circle()
                        .fill(Theme.Colors.brandLight)
                        .frame(width: 6, height: 6)
                        .opacity(0.8)
                }

                Spacer()

                if !event.partial && event.content.count > 100 {
                    Button {
                        withAnimation(.easeInOut(duration: 0.2)) {
                            isExpanded.toggle()
                        }
                    } label: {
                        Image(systemName: isExpanded ? "chevron.up" : "chevron.down")
                            .font(.system(size: 10))
                            .foregroundStyle(.secondary)
                    }
                    .buttonStyle(.plain)
                }
            }

            // Content
            Text(event.content)
                .font(.netclodeCaption)
                .foregroundStyle(.secondary)
                .italic()
                .lineLimit(isExpanded || event.partial ? nil : 3)
                .animation(.easeInOut(duration: 0.2), value: event.content)
        }
        .padding(.horizontal, Theme.Spacing.sm)
        .padding(.vertical, Theme.Spacing.xs)
        .background(Theme.Colors.brandLight.opacity(event.partial ? 0.15 : 0.1))
        .clipShape(RoundedRectangle(cornerRadius: Theme.Radius.sm))
        .animation(.easeInOut(duration: 0.2), value: event.partial)
    }
}

// MARK: - Port Exposed Card

struct PortExposedCard: View {
    let event: PortExposedEvent

    var body: some View {
        HStack(spacing: Theme.Spacing.sm) {
            HStack(spacing: 4) {
                Image(systemName: "network")
                    .font(.system(size: 10, weight: .semibold))
                Text(verbatim: "Port \(event.port)")
                    .font(.system(size: 11, weight: .semibold, design: .monospaced))
            }
            .foregroundStyle(.cyan)
            .padding(.horizontal, 8)
            .padding(.vertical, 4)
            .background(Color.cyan.opacity(0.15))
            .clipShape(Capsule())

            if let process = event.process {
                Text(process)
                    .font(.netclodeCaption)
                    .foregroundStyle(.secondary)
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
                } label: {
                    HStack(spacing: 4) {
                        Text("Open")
                            .font(.system(size: 11, weight: .medium))
                        Image(systemName: "chevron.down")
                            .font(.system(size: 8))
                    }
                    .foregroundStyle(.cyan)
                }
            }
        }
        .padding(.horizontal, Theme.Spacing.sm)
        .padding(.vertical, Theme.Spacing.xs)
        .background(Theme.Colors.codeBackground.opacity(0.5))
        .clipShape(RoundedRectangle(cornerRadius: Theme.Radius.sm))
    }
}

// MARK: - Previews

#Preview("Tool Event - Running") {
    VStack(spacing: Theme.Spacing.md) {
        ToolEventCard(
            event: .toolStart(ToolStartEvent(
                id: UUID(),
                timestamp: Date(),
                tool: "Read",
                toolUseId: "123",
                input: ["file_path": .string("/src/components/Button.swift")]
            )),
            endEvent: nil
        )

        ToolEventCard(
            event: .toolStart(ToolStartEvent(
                id: UUID(),
                timestamp: Date(),
                tool: "Bash",
                toolUseId: "124",
                input: ["command": .string("npm run build && npm test")]
            )),
            endEvent: nil
        )
    }
    .padding()
    .background(Theme.Colors.background)
}

#Preview("Tool Event - Completed") {
    VStack(spacing: Theme.Spacing.md) {
        ToolEventCard(
            event: .toolStart(ToolStartEvent(
                id: UUID(),
                timestamp: Date(),
                tool: "Read",
                toolUseId: "123",
                input: ["file_path": .string("/src/components/Button.swift")]
            )),
            endEvent: ToolEndEvent(
                id: UUID(),
                timestamp: Date(),
                tool: "Read",
                toolUseId: "123",
                result: "import SwiftUI\n\nstruct Button: View {\n    var body: some View {\n        Text(\"Hello\")\n    }\n}",
                error: nil
            )
        )

        ToolEventCard(
            event: .toolStart(ToolStartEvent(
                id: UUID(),
                timestamp: Date(),
                tool: "Edit",
                toolUseId: "125",
                input: [
                    "file_path": .string("/src/auth/AuthService.swift"),
                    "old_string": .string("func login()"),
                    "new_string": .string("func login(username: String)")
                ]
            )),
            endEvent: ToolEndEvent(
                id: UUID(),
                timestamp: Date(),
                tool: "Edit",
                toolUseId: "125",
                result: nil,
                error: "File not found"
            )
        )
    }
    .padding()
    .background(Theme.Colors.background)
}

#Preview("Command Card") {
    VStack(spacing: Theme.Spacing.md) {
        CommandEventCard(
            startEvent: CommandStartEvent(
                id: UUID(),
                timestamp: Date(),
                command: "npm run build",
                cwd: "/workspace/myproject"
            ),
            endEvent: nil
        )

        CommandEventCard(
            startEvent: CommandStartEvent(
                id: UUID(),
                timestamp: Date(),
                command: "npm test",
                cwd: "/workspace"
            ),
            endEvent: CommandEndEvent(
                id: UUID(),
                timestamp: Date(),
                command: "npm test",
                exitCode: 0,
                output: "PASS  src/Button.test.ts\nAll tests passed!"
            )
        )
    }
    .padding()
    .background(Theme.Colors.background)
}

#Preview("File Change Card") {
    VStack(spacing: Theme.Spacing.md) {
        FileChangeCard(event: FileChangeEvent(
            id: UUID(),
            timestamp: Date(),
            path: "/src/auth/AuthService.swift",
            action: .edit,
            linesAdded: 25,
            linesRemoved: 10
        ))

        FileChangeCard(event: FileChangeEvent(
            id: UUID(),
            timestamp: Date(),
            path: "/src/models/User.swift",
            action: .create,
            linesAdded: 45,
            linesRemoved: nil
        ))

        FileChangeCard(event: FileChangeEvent(
            id: UUID(),
            timestamp: Date(),
            path: "/src/deprecated/OldService.swift",
            action: .delete,
            linesAdded: nil,
            linesRemoved: 120
        ))
    }
    .padding()
    .background(Theme.Colors.background)
}

