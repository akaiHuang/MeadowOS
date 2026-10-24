// Package sessionrecovery provides the durable crash-resume substrate for the
// Meadow Harness without depending on Harness, agent daemon, CLI, Desktop, or
// transport packages.
//
// An integrator records stable turn and tool boundaries through Recorder. The
// store writes a sequence-checked, SHA-256 chained JSONL journal and fsyncs each
// record before reporting success. Snapshots are atomically replaced. Bounded
// compaction first commits a snapshot carrying the journal hash anchor, then
// atomically starts a new append-only journal segment.
//
// The persistence schema is deliberately closed. Prompts, tool arguments,
// guidance, decrypted model material, credentials, and attachment bytes remain
// references only. Non-sensitive tool output and structured run recaps may be
// retained as session-local content-addressed evidence so a restart can rebuild
// observable output without placing raw content in the journal or snapshot.
// Unauthorized attachments cannot carry a storage reference.
package sessionrecovery
