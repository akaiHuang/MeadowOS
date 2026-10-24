// Package checkpoint provides an isolated durable checkpoint substrate for
// agent tasks. It intentionally has no dependency on agentd or session.
//
// A Store owns one append-only journal and one atomically replaced snapshot.
// Journal records are sequence checked and SHA-256 hash chained. Snapshots
// carry the exact journal byte offset they cover, so recovery only replays a
// bounded tail. Corruption never causes automatic truncation: Recover returns
// the last trusted boundary together with a RecoveryError.
//
// Events contain references and redacted summaries, not raw prompts, tool
// arguments, tool outputs, bearer tokens, or keys. Callers can layer an
// additional Redactor on top of the mandatory credential redactor.
package checkpoint
