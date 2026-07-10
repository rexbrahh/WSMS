package indexer

// Register the pure-Go sqlite-vec extension distributed with modernc.org/sqlite.
// Registration is process-wide for the modernc driver: ledger connections do not
// create vec0 tables and remain unaffected. Dense projection is still opt-in
// per Index via Options.DenseDimensions.
import _ "modernc.org/sqlite/vec"
