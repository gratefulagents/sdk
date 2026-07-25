// Package durable defines restart-safe, versioned persistence primitives for
// an agent run. It deliberately does not execute a run: runners use its
// events, snapshots, leases, and recovery decisions to resume work safely.
package durable
