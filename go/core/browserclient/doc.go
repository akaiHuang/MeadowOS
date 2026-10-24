// Package browserclient implements the private Meadow Browser worker client.
//
// The package accepts only an already-connected anonymous Unix-domain stream.
// It does not discover, dial, or launch a worker and has no HTTP or TCP
// transport. The inherited stream and one-shot peer secret form the private
// channel to meadow-browserd.
package browserclient
