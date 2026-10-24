// Package modelvault installs signed model catalog entries into the current
// macOS application's fixed Application Support vault.
//
// Callers cannot supply a vault root, download URL, staging path, sealed asset
// path, or runtime model path. Catalog signatures authorize one HTTPS origin
// and bounded path templates. Plaintext exists only in a private staging
// directory while a synchronous production Sealer consumes it. Every exit
// wipes and removes staging. Installed handles expose identity and digest
// evidence, while OpenManifest and OpenSealedAsset retain path resolution
// inside this package.
//
// Non-Darwin platforms fail closed. Production must link a trusted Ed25519
// keyring, clock, license verifier, sealer, and independent removal authorizer.
package modelvault
