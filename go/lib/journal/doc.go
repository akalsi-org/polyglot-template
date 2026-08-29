// Package journal defines a durable archive-drained shared-memory journal.
//
// Journal-ring format version one is separate from MPSC and SPSC format four.
// One producer publishes records. One archiver creates durable batches.
// Any number of passive observers can replay and inspect live records.
//
// Producer commit provides process-visible publication, not power-loss durability.
// A complete batch becomes durable before its ring capacity becomes reusable.
// A power failure can lose the current uncut ring tail.
//
// The normative stored-format and concurrency contract is docs/journal-ring.md.
package journal
