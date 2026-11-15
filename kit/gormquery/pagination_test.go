package gormquery

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func intRef(value int) *int { return &value }

func TestPaginationOffsetAndLimit(t *testing.T) {
	t.Run("nil receiver uses defaults", func(t *testing.T) {
		var pagination *Pagination
		offset, limit := pagination.OffsetAndLimit()
		assert.Equal(t, 0, offset)
		assert.Equal(t, DefaultPageSize, limit)
	})

	t.Run("nil fields use defaults", func(t *testing.T) {
		offset, limit := (&Pagination{}).OffsetAndLimit()
		assert.Equal(t, 0, offset)
		assert.Equal(t, DefaultPageSize, limit)
	})

	t.Run("non-positive fields use defaults", func(t *testing.T) {
		offset, limit := (&Pagination{Current: intRef(0), PageSize: intRef(-5)}).OffsetAndLimit()
		assert.Equal(t, 0, offset)
		assert.Equal(t, DefaultPageSize, limit)
	})

	t.Run("explicit values are honored", func(t *testing.T) {
		offset, limit := (&Pagination{Current: intRef(3), PageSize: intRef(50)}).OffsetAndLimit()
		assert.Equal(t, 100, offset)
		assert.Equal(t, 50, limit)
	})

	t.Run("page size is capped", func(t *testing.T) {
		offset, limit := (&Pagination{Current: intRef(1), PageSize: intRef(10000)}).OffsetAndLimit()
		assert.Equal(t, 0, offset)
		assert.Equal(t, MaxPageSize, limit)
	})

	t.Run("huge current keeps the offset non-negative", func(t *testing.T) {
		maxInt := int(^uint(0) >> 1)
		offset, limit := (&Pagination{Current: intRef(maxInt), PageSize: intRef(MaxPageSize)}).OffsetAndLimit()
		assert.Equal(t, (maxInt/MaxPageSize)*MaxPageSize, offset)
		assert.GreaterOrEqual(t, offset, 0)
		assert.Equal(t, MaxPageSize, limit)
	})

	t.Run("offset accessors delegate to the same bounds", func(t *testing.T) {
		pagination := &Pagination{Current: intRef(2), PageSize: intRef(25)}
		assert.Equal(t, 25, pagination.Offset())
		assert.Equal(t, 25, pagination.Limit())
	})
}

func TestPaginationToOffsetAndLimit(t *testing.T) {
	t.Run("clears fields after conversion", func(t *testing.T) {
		pagination := &Pagination{Current: intRef(3), PageSize: intRef(50)}
		offset, limit := pagination.ToOffsetAndLimit()
		assert.Equal(t, 100, offset)
		assert.Equal(t, 50, limit)
		assert.Nil(t, pagination.Current)
		assert.Nil(t, pagination.PageSize)

		offset, limit = pagination.ToOffsetAndLimit()
		assert.Equal(t, 0, offset, "a cleared pagination must fall back to the defaults")
		assert.Equal(t, DefaultPageSize, limit)
	})

	t.Run("pure accessors leave fields in place", func(t *testing.T) {
		pagination := &Pagination{Current: intRef(3), PageSize: intRef(50)}
		offset, limit := pagination.OffsetAndLimit()
		assert.Equal(t, 100, offset)
		assert.Equal(t, 50, limit)
		assert.Equal(t, 3, *pagination.Current)
		assert.Equal(t, 50, *pagination.PageSize)

		assert.Equal(t, 100, pagination.Offset())
		assert.Equal(t, 50, pagination.Limit())
		assert.Equal(t, 3, *pagination.Current)
		assert.Equal(t, 50, *pagination.PageSize)
	})

	t.Run("nil receiver is safe", func(t *testing.T) {
		var pagination *Pagination
		offset, limit := pagination.ToOffsetAndLimit()
		assert.Equal(t, 0, offset)
		assert.Equal(t, DefaultPageSize, limit)
	})

	t.Run("defaults stay within the cap", func(t *testing.T) {
		assert.Equal(t, 1, DefaultCurrent)
		assert.LessOrEqual(t, DefaultPageSize, MaxPageSize)
	})
}
