package retrieval

import "errors"

var (
	// ErrSemanticPageMiss is returned when no candidate clears hard filters
	// and abstention. It is not an operational failure.
	ErrSemanticPageMiss = errors.New("SEMANTIC_PAGE_MISS")
	// ErrIndexUnavailable reports a missing or closed warm index.
	ErrIndexUnavailable = errors.New("warm index unavailable")
)
