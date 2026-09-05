# Working directory and path rules

- Treat the active working directory supplied for the session as the workspace root. For this repository, that root is the directory containing this `AGENTS.md` file.
- Run commands with the workspace root as their working directory unless the task explicitly requires a particular subdirectory.
- When the user names a directory or file in chat, use that exact location for searching, reading, creating, editing, moving, or saving files. The user's most recent path instruction overrides defaults in this file.
- Resolve relative paths against the active working directory unless the user explicitly says they are relative to another directory.
- Before creating or editing a file, verify that its resolved path is inside the workspace root or inside the exact external location explicitly named by the user.
- Put new files in the destination the user requested. If no destination was given, choose the most appropriate existing directory inside the workspace; do not write to the user profile, a system temporary directory, or another repository as a substitute.
- Keep searches within the directory the user requested. If no search directory was given, search from the workspace root.
- Do not silently change a requested path because a similar directory exists elsewhere. If a requested path cannot be found or is ambiguous, inspect likely locations first and then ask one concise question if the choice would materially affect the result.
- Use explicit working-directory arguments for shell and tool calls whenever available. Avoid depending on a prior command's directory change.
- When delegating work, include the workspace root plus every user-specified search and output path in the delegated task.
- In the final response, report the actual paths of files created or changed.
