// Package keybroker provides device-bound, versioned secret storage for
// Meadow model data, resource vaults, and license device state.
//
// Production storage is backed by the macOS Security framework. Secret bytes
// are passed only through in-process buffers: this package never places them
// in process arguments, environment variables, account names, or logs.
package keybroker
