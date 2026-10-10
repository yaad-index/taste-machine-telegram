# ADR 0001: A Telegram frontend for taste-machine

## Context
taste-machine (the engine) works on files from a command line. To be used at a table, it needs a chat interface that a group can answer together. The engine stays generic; this repository is one frontend for it.

## Decision

### 1. Shape
- A Go service in this repository that imports the engine as a library, pinned to a released version.
- Importers are run as the engine's `taste-machine compile <source>` binary, not linked in, so this service carries no source-specific code. The image builds that binary from the same pinned module version, so the library and the files it reads never drift apart.
- Files are the database: each user's shelf and taste files live in a data directory on a volume. No separate database in v1.
- Files are written to a temporary file and renamed into place, so a reader never sees a half-written file. A running session keeps the files it loaded.

### 2. Access
- Only allowlisted Telegram user ids can use the bot. The configuration names one or more admins.
- The admin ids from the environment are always allowed and need not be in the allowlist file.
- **Invites:** an admin runs `/invite` and gets a one-time link (valid 24 hours). Whoever opens it is added to the allowlist, which is kept in a file in the data directory, written by temp file and rename. Tokens come from a cryptographic random source, are consumed atomically so a link admits one person only, are pruned when expired, and are never logged. Whoever opens a link first gets in; a forwarded link is an accepted risk.
- An admin can `/revoke` a user, which deletes their files and removes them from any running session at the next question. A session left with one member continues in single mode; with none it ends.
- Anyone else gets one refusal line, and nothing about them is stored or logged: no message content, no names.
- In a group chat the bot answers only allowlisted members, and only when the chat itself is allowlisted: an admin runs `/allow` in the chat, or `/disallow` to remove it. Chats are kept in the same allowlist file.

### 3. Linking a source account
- `/link <source> <user name>` links the caller to a source account and compiles their shelf and taste. `/refresh` recompiles. Both count towards one limit: at most one compile per hour per user, read from the modification time of the user's taste file, so it survives a restart; a failed compile writes no file and does not use up the hour. Unlinking and linking again skips the limit; that is accepted for an allowlisted group.
- A compile can take a minute and all users share one source key and its rate limit. So the bot replies "compiling…" at once, runs compiles in the background one at a time (a queue), and edits that message when done.
- `/unlink` deletes the caller's files.
- Compile errors are shown to the caller in one line; the source API key never appears in messages or logs.

### 4. Picking alone (private chat)
- `/pick` starts a question flow on the caller's own shelf, as in engine ADR 0002.
- Each question is one message, headed "Step N · K items left", with an inline button per option, plus "none of these" and "skip" (the engine's "other" and "no preference"). Tapping edits the message to show the answer, then the next question follows.
  - N is the number of answers given so far, plus one. The engine picks each question as it goes, so how many are left is not known in advance and is not shown.
- From the second question on, a "back" button takes back the last answer and asks that question again. The engine's Undo would skip the field instead, so the bot replays the answers before it over a new engine session.
- A "show results now" button stops the questions early.
- An answer that leaves nothing gets the engine's empty-result report, with an Undo button.
- At the end the bot shows the top 5 results with names; the one-line reason is each result's top positive contribution. A button shows the full explanation.

### 5. Picking as a group (group chat)
- `/night` opens a session in the chat: members tap "I'm in" to join. A member with no linked files is refused with "use /link in a private chat first"; a member whose files use a different schema from the shelf is refused too.
- `/pick` runs group mode (engine ADR 0003) with the joined members' tastes, on the shelf of the member who ran `/pick`. With one joined member it runs single mode.
- Questions have the same buttons as in section 4, back included. Any joined member can answer a question; the first tap counts and the message shows who answered. One answer per question, as the ADR says.
- Results show the group score and each member's score by their Telegram first name. A duplicate first name gets the member's username added, so labels stay unique.
- A chat runs one session at a time. Sessions live in memory and a restart ends them, accepted for v1; a tap on a button from an ended session gets "this session has ended (the bot restarted or it timed out); start again with /pick, or /night in a group". A session ends with `/done` or after 6 hours.

### 6. Running it
- One container, with a health endpoint for monitoring.
- Configuration from environment variables: the bot token, the source API key, the admin ids, the data directory. Secrets come only from the environment.
- Long polling, so no public endpoint is needed.

## v1 scope
`/link`, `/refresh`, `/unlink`, `/pick` alone and as a group, `/night`, `/done`, `/invite`, `/revoke`, `/allow`, `/disallow`, `/help`.

## Out of scope (later, each with its own decision)
- `check` for items to buy (needs an importer mode that compiles a list of ids).
- "At most / at least" answers on number fields, and asking a votes field early. Both are engine changes (ADR 0002).
- Public sign-up, any web interface, ML or LLM matchers.

## Consequences
- The bot depends on the engine's file format and CLI; an engine release can require a bot release.
- Members' scores are visible to the whole group, as engine ADR 0003 intends.
- Files on one volume do not scale to many users; that is accepted for an allowlisted group.
