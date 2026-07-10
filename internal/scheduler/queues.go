package scheduler

// QueueName identifies scheduler work queues from the design docs.
type QueueName string

const (
	QueueRealTime      QueueName = "realtime"
	QueueToolDigest    QueueName = "tool_digest"
	QueueConsolidation QueueName = "consolidation"
	QueuePageFault     QueueName = "page_fault"
	QueueCold          QueueName = "cold"
)
