package gormquery

import "math"

const (
	// DefaultCurrent is the page number used when Current is nil or not
	// positive.
	DefaultCurrent = 1

	// DefaultPageSize is the page size used when PageSize is nil or not
	// positive.
	DefaultPageSize = 20

	// MaxPageSize is the largest page size a caller can request. It bounds the
	// work a single request can ask the database to do.
	MaxPageSize = 1000
)

// Pagination binds the page number and page size of a list request. The form
// tags carry the defaults an HTTP binder applies; the methods below apply the
// same defaults themselves, so a programmatically built value behaves the same
// as a bound one.
type Pagination struct {
	Current  *int `form:"current,default=1"`
	PageSize *int `form:"pageSize,default=20"`
}

// bounds resolves the effective page number and page size without modifying
// the receiver. A nil receiver, a nil field, or a non-positive value falls back
// to the corresponding default, and the page size is capped at MaxPageSize.
func (p *Pagination) bounds() (current, pageSize int) {
	current, pageSize = DefaultCurrent, DefaultPageSize
	if p == nil {
		return current, pageSize
	}
	if p.Current != nil && *p.Current > 0 {
		current = *p.Current
	}
	if p.PageSize != nil && *p.PageSize > 0 {
		pageSize = *p.PageSize
	}
	if pageSize > MaxPageSize {
		pageSize = MaxPageSize
	}
	// Prevent (current-1)*pageSize from overflowing int, which could otherwise
	// wrap the offset negative. pageSize is at least 1 here.
	if current-1 > math.MaxInt/pageSize {
		current = math.MaxInt/pageSize + 1
	}
	return current, pageSize
}

// OffsetAndLimit converts the page number and page size to an offset and a
// limit without changing the receiver.
func (p *Pagination) OffsetAndLimit() (offset, limit int) {
	current, pageSize := p.bounds()
	return (current - 1) * pageSize, pageSize
}

// Offset returns the row offset implied by the page number and page size.
func (p *Pagination) Offset() int {
	offset, _ := p.OffsetAndLimit()
	return offset
}

// Limit returns the effective page size, capped at MaxPageSize.
func (p *Pagination) Limit() int {
	_, limit := p.OffsetAndLimit()
	return limit
}

// ToOffsetAndLimit converts the page number and page size to an offset and a
// limit, then clears both fields. Callers that embed Pagination in a request
// value object and later pass that object to GORM as a condition rely on the
// clearing so the pagination fields are not mistaken for columns.
func (p *Pagination) ToOffsetAndLimit() (offset, limit int) {
	offset, limit = p.OffsetAndLimit()
	if p != nil {
		p.Current = nil
		p.PageSize = nil
	}
	return offset, limit
}
