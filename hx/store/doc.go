// Package store is the record store under the Helix world format: files of
// checksummed records, each found through an index, changed only by atomic
// save rounds recorded in one journal.
//
// # Files
//
// A store is a directory of .hx files plus a journal directory. Each file holds
// records (a header, a key, a body and checksums) and an index of where each
// key's record is. A file starts with a header and two superblock slots, each
// in its own 4 KB sector; a checkpoint writes the file's index as a record and
// then the superblock slot not in use, so a torn write never loses both.
//
// # Save rounds
//
// Commit writes a Batch as one round: every new record goes to free space,
// never over a record still in use; the files are forced to disk; then one
// commit record listing every new location is appended to the journal and
// forced to disk. That append is the moment the round counts: after a crash
// the journal is replayed over each file's last checkpoint, and a round whose
// commit record is missing or torn never happened. Every file changes in the
// same round or none does.
//
// Checkpoints copy the indexes to the files and empty the journal, so opening
// a store reads at most a bounded journal, never the whole store.
//
// # Reads
//
// A read looks its key up in the file's index (kept in memory, within a byte
// budget) and reads the record with one positioned read into the caller's
// buffer. Reads take no lock a save holds for I/O. Space freed by a round is
// reused only once no read that started before the round can still use it
// (generations), so a read never sees a record overwritten under it.
//
// The store never decodes what it holds: bodies are bytes, compressed with
// zstd when that makes them smaller.
package store
