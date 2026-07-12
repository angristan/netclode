import { spawn } from "child_process";
import { existsSync, writeFileSync, mkdirSync, readFileSync } from "fs";
import { dirname, join } from "path";

export function repoDirName(repoUrl: string): string {
  let cleaned = repoUrl.trim();
  cleaned = cleaned.replace(/^https?:\/\//, "");
  cleaned = cleaned.replace(/^github\.com\//, "");
  cleaned = cleaned.replace(/\.git$/, "");

  const parts = cleaned.split("/").filter(Boolean);
  let name = parts.length >= 2 ? `${parts[parts.length - 2]}__${parts[parts.length - 1]}` : parts[0] || cleaned;

  name = name.replace(/[^A-Za-z0-9_.-]+/g, "_");
  return name;
}

/**
 * Get the directory path for a repo based on whether it's a single or multi-repo setup.
 * Single repo: clones directly to workspaceDir
 * Multi-repo: clones to workspaceDir/owner__repo
 */
export function getRepoPath(repoUrl: string, totalRepos: number, workspaceDir: string): string {
  if (totalRepos === 1) {
    return workspaceDir;
  }
  return `${workspaceDir}/${repoDirName(repoUrl)}`;
}

/**
 * Get the prefix for a repo (used for path prefixing in git status/diff).
 * Single repo: empty string (files are at workspace root)
 * Multi-repo: owner__repo
 */
export function getRepoPrefix(repoUrl: string, totalRepos: number): string {
  if (totalRepos === 1) {
    return "";
  }
  return repoDirName(repoUrl);
}

/**
 * Run a git command and return the output.
 */
export function runGit(args: string[], cwd?: string): Promise<{ code: number; stdout: string; stderr: string }> {
  return new Promise((resolve) => {
    const proc = spawn("git", args, {
      cwd,
      env: process.env,
    });

    let stdout = "";
    let stderr = "";

    proc.stdout.on("data", (data) => {
      stdout += data.toString();
    });

    proc.stderr.on("data", (data) => {
      stderr += data.toString();
      // Log progress output (git clone --progress writes to stderr)
      process.stderr.write(data);
    });

    proc.on("close", (code) => {
      resolve({ code: code ?? 1, stdout, stderr });
    });
  });
}

/**
 * Configure git credentials for GitHub token authentication.
 * Also sets GITHUB_TOKEN env var for gh CLI.
 */
export async function configureGitCredentials(token: string): Promise<void> {
  const credentialsFile = "/agent/.git-credentials";
  const configDir = dirname(credentialsFile);

  if (!existsSync(configDir)) {
    mkdirSync(configDir, { recursive: true });
  }

  writeFileSync(credentialsFile, `https://x-access-token:${token}@github.com\n`, { mode: 0o600 });

  // Set GITHUB_TOKEN for gh CLI and other tools
  process.env.GITHUB_TOKEN = token;

  await runGit(["config", "--global", "credential.helper", `store --file=${credentialsFile}`]);
  await runGit(["config", "--global", "user.name", "Netclode Agent"]);
  await runGit(["config", "--global", "user.email", "agent@netclode.local"]);
}

/**
 * Clone or update a repository. Progress is emitted over the authenticated
 * AgentService stream by initializeSessionRepos.
 */
export async function setupRepository(repoUrl: string, repoDir: string, githubToken?: string): Promise<void> {
  console.log(`[git] Setting up repository: ${repoUrl}`);

  if (githubToken) {
    await configureGitCredentials(githubToken);
  }

  if (existsSync(`${repoDir}/.git`)) {
    await runGit(["config", "--add", "safe.directory", repoDir], repoDir);
    const result = await runGit(["pull", "--ff-only"], repoDir);
    if (result.code !== 0) {
      console.warn("[git] Pull failed, continuing with existing repository state");
    }
    return;
  }

  const result = await runGit(["clone", "--progress", repoUrl, repoDir]);
  if (result.code !== 0) {
    throw new Error(result.stderr || `git clone exited with status ${result.code}`);
  }
  await runGit(["config", "--add", "safe.directory", repoDir], repoDir);
}

/**
 * Parsed git file change from status --porcelain
 */
export interface GitFileChange {
  path: string;
  status: "modified" | "added" | "deleted" | "renamed" | "copied" | "untracked" | "ignored" | "unmerged";
  staged: boolean;
  linesAdded?: number;
  linesRemoved?: number;
}

/**
 * Parse git status --porcelain=v1 output into structured data
 */
export function parseGitStatus(output: string): GitFileChange[] {
  const files: GitFileChange[] = [];
  const lines = output.split("\n").filter((line) => line.length > 0);

  for (const line of lines) {
    const indexStatus = line[0];
    const workTreeStatus = line[1];
    const path = line.substring(3).split(" -> ").pop() || line.substring(3);

    let status: GitFileChange["status"];
    let staged = false;

    if (workTreeStatus === "M") {
      status = "modified";
    } else if (workTreeStatus === "D") {
      status = "deleted";
    } else if (workTreeStatus === "?") {
      status = "untracked";
    } else if (workTreeStatus === "!") {
      status = "ignored";
    } else if (workTreeStatus === "U" || indexStatus === "U") {
      status = "unmerged";
    } else if (indexStatus === "M") {
      status = "modified";
      staged = true;
    } else if (indexStatus === "A") {
      status = "added";
      staged = true;
    } else if (indexStatus === "D") {
      status = "deleted";
      staged = true;
    } else if (indexStatus === "R") {
      status = "renamed";
      staged = true;
    } else if (indexStatus === "C") {
      status = "copied";
      staged = true;
    } else {
      status = "modified";
    }

    files.push({ path, status, staged });
  }

  return files;
}

/**
 * Parse git diff --numstat output to get line counts per file.
 * Returns a map of file path to { added, removed } counts.
 */
export function parseDiffNumstat(output: string): Map<string, { added: number; removed: number }> {
  const stats = new Map<string, { added: number; removed: number }>();
  const lines = output.split("\n").filter((line) => line.length > 0);

  for (const line of lines) {
    // Format: "added<TAB>removed<TAB>path" or "-<TAB>-<TAB>path" for binary
    const parts = line.split("\t");
    if (parts.length >= 3) {
      const added = parts[0] === "-" ? 0 : parseInt(parts[0], 10);
      const removed = parts[1] === "-" ? 0 : parseInt(parts[1], 10);
      // Handle renames: "old path => new path" or just path
      const pathPart = parts.slice(2).join("\t");
      const path = pathPart.includes(" => ") ? pathPart.split(" => ").pop()! : pathPart;
      stats.set(path, { added: isNaN(added) ? 0 : added, removed: isNaN(removed) ? 0 : removed });
    }
  }

  return stats;
}

/**
 * Get the git status for a workspace with diff stats
 */
export async function getGitStatus(workspaceDir: string): Promise<GitFileChange[]> {
  console.log("[git] Getting git status");
  const result = await runGit(["status", "--porcelain=v1"], workspaceDir);
  const files = parseGitStatus(result.stdout);

  if (files.length === 0) {
    console.log("[git] Git status: 0 changed files");
    return files;
  }

  // Get diff stats for unstaged changes
  const unstagedResult = await runGit(["diff", "--numstat"], workspaceDir);
  const unstagedStats = parseDiffNumstat(unstagedResult.stdout);

  // Get diff stats for staged changes
  const stagedResult = await runGit(["diff", "--numstat", "--cached"], workspaceDir);
  const stagedStats = parseDiffNumstat(stagedResult.stdout);

  // Merge stats into file changes
  for (const file of files) {
    const stats = file.staged ? stagedStats.get(file.path) : unstagedStats.get(file.path);
    if (stats) {
      file.linesAdded = stats.added;
      file.linesRemoved = stats.removed;
    } else if (file.status === "untracked" || file.status === "added") {
      // For untracked/new files, count lines in the file
      try {
        const filePath = join(workspaceDir, file.path);
        if (existsSync(filePath)) {
          const content = readFileSync(filePath, "utf-8");
          const lineCount = content.split("\n").length;
          file.linesAdded = lineCount;
          file.linesRemoved = 0;
        }
      } catch {
        // Ignore errors reading file
      }
    }
  }

  console.log(`[git] Git status: ${files.length} changed files`);
  return files;
}

/**
 * Get the git diff for a workspace or specific file.
 * For untracked files, generates a synthetic diff showing all lines as additions.
 */
export async function getGitDiff(workspaceDir: string, file?: string, pathPrefix?: string): Promise<string> {
  console.log(`[git] Getting git diff for: ${file || "all files"}`);

  // First, try regular git diff
  const args = ["diff", "--no-color"];
  if (pathPrefix) {
    args.push(`--src-prefix=${pathPrefix}/`, `--dst-prefix=${pathPrefix}/`);
  }
  if (file) {
    args.push("--", file);
  }
  const result = await runGit(args, workspaceDir);

  // If we got a diff, return it
  if (result.stdout.length > 0) {
    console.log(`[git] Git diff: ${result.stdout.length} chars`);
    return result.stdout;
  }

  // If no diff and a specific file was requested, check if it's untracked or new
  if (file) {
    const statusResult = await runGit(["status", "--porcelain=v1", "--", file], workspaceDir);
    const statusLine = statusResult.stdout.trim();

    // Check if file is untracked (??) or added (A )
    if (statusLine.startsWith("??") || statusLine.startsWith("A ")) {
      console.log(`[git] File is untracked/new, generating synthetic diff for: ${file}`);
      return generateSyntheticDiff(workspaceDir, file, pathPrefix);
    }
  }

  console.log(`[git] Git diff: ${result.stdout.length} chars`);
  return result.stdout;
}

/**
 * Generate a synthetic unified diff for a new/untracked file.
 * Shows all lines as additions.
 */
function generateSyntheticDiff(workspaceDir: string, file: string, pathPrefix?: string): string {
  const filePath = join(workspaceDir, file);
  const prefixedFile = pathPrefix ? `${pathPrefix}/${file}` : file;

  if (!existsSync(filePath)) {
    console.log(`[git] File does not exist: ${filePath}`);
    return "";
  }

  try {
    const content = readFileSync(filePath, "utf-8");
    const lines = content.split("\n");

    // Build unified diff format
    const diffLines: string[] = [
      `diff --git a/${prefixedFile} b/${prefixedFile}`,
      `new file mode 100644`,
      `--- /dev/null`,
      `+++ b/${prefixedFile}`,
      `@@ -0,0 +1,${lines.length} @@`,
    ];

    // Add all lines as additions
    for (const line of lines) {
      diffLines.push(`+${line}`);
    }

    const diff = diffLines.join("\n");
    console.log(`[git] Generated synthetic diff: ${diff.length} chars`);
    return diff;
  } catch (error) {
    console.error(`[git] Failed to read file for synthetic diff: ${error}`);
    return "";
  }
}
