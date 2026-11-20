package gormkit

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
// to the corresponding default. A positive maxPageSize caps the page size; a
// non-positive maxPageSize leaves it uncapped.
func (p *Pagination) bounds(maxPageSize int) (current, pageSize int) {
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
	if maxPageSize > 0 && pageSize > maxPageSize {
		pageSize = maxPageSize
	}
	// Prevent (current-1)*pageSize from overflowing int, which could otherwise
	// wrap the offset negative. pageSize is at least 1 here.
	if current-1 > math.MaxInt/pageSize {
		current = math.MaxInt/pageSize + 1
	}
	return current, pageSize
}

// OffsetAndLimit converts the page number and page size to an offset and a
// limit without changing the receiver. The page size is capped at MaxPageSize.
func (p *Pagination) OffsetAndLimit() (offset, limit int) {
	return p.OffsetAndLimitMax(MaxPageSize)
}

// OffsetAndLimitMax converts the page number and page size to an offset and a
// limit without changing the receiver, like OffsetAndLimit, but caps the page
// size at maxPageSize instead of MaxPageSize. An export or batch endpoint that
// legitimately needs more rows uses this to raise the cap at its own call site
// without changing the package default. A non-positive maxPageSize removes the
// cap entirely; the caller then accepts responsibility for the query size,
// since the database is asked for as many rows as the request allows.
func (p *Pagination) OffsetAndLimitMax(maxPageSize int) (offset, limit int) {
	current, pageSize := p.bounds(maxPageSize)
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
// clearing so the pagination fields are not mistaken for columns. The page size
// is capped at MaxPageSize.
func (p *Pagination) ToOffsetAndLimit() (offset, limit int) {
	return p.ToOffsetAndLimitMax(MaxPageSize)
}

// ToOffsetAndLimitMax behaves like ToOffsetAndLimit, but caps the page size at
// maxPageSize instead of MaxPageSize and clears both fields afterwards. A
// non-positive maxPageSize removes the cap; see OffsetAndLimitMax.
func (p *Pagination) ToOffsetAndLimitMax(maxPageSize int) (offset, limit int) {
	offset, limit = p.OffsetAndLimitMax(maxPageSize)
	if p != nil {
		p.Current = nil
		p.PageSize = nil
	}
	return offset, limit
}
