package memory

// MarkStale flags a page as stale.
func MarkStale(p *Page) {
	if p != nil {
		p.Stale = true
	}
}

// MarkInvalidated flags a page as invalidated (stronger than stale).
func MarkInvalidated(p *Page) {
	if p != nil {
		p.Invalidated = true
		p.Stale = true
	}
}
