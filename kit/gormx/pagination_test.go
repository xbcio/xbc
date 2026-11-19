package gormx

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

func TestPaginationOffsetAndLimitMax(t *testing.T) {
	t.Run("custom cap applies", func(t *testing.T) {
		offset, limit := (&Pagination{Current: intRef(2), PageSize: intRef(5000)}).OffsetAndLimitMax(2000)
		assert.Equal(t, 2000, offset)
		assert.Equal(t, 2000, limit)
	})

	t.Run("cap above MaxPageSize is honored", func(t *testing.T) {
		offset, limit := (&Pagination{Current: intRef(2), PageSize: intRef(5000)}).OffsetAndLimitMax(10000)
		assert.Equal(t, 5000, offset)
		assert.Equal(t, 5000, limit)
	})

	t.Run("non-positive cap disables the cap", func(t *testing.T) {
		for _, maxPageSize := range []int{0, -1} {
			offset, limit := (&Pagination{Current: intRef(3), PageSize: intRef(10000)}).OffsetAndLimitMax(maxPageSize)
			assert.Equal(t, 20000, offset, "maxPageSize=%d", maxPageSize)
			assert.Equal(t, 10000, limit, "maxPageSize=%d", maxPageSize)
		}
	})

	t.Run("huge page size and current keep the offset non-negative", func(t *testing.T) {
		maxInt := int(^uint(0) >> 1)
		offset, limit := (&Pagination{Current: intRef(maxInt), PageSize: intRef(maxInt)}).OffsetAndLimitMax(0)
		assert.Equal(t, maxInt, limit)
		assert.Equal(t, maxInt, offset)
		assert.GreaterOrEqual(t, offset, 0)
	})

	t.Run("nil receiver uses defaults", func(t *testing.T) {
		var pagination *Pagination
		offset, limit := pagination.OffsetAndLimitMax(0)
		assert.Equal(t, 0, offset)
		assert.Equal(t, DefaultPageSize, limit)
	})

	t.Run("cap below the default applies to defaults", func(t *testing.T) {
		offset, limit := (&Pagination{}).OffsetAndLimitMax(10)
		assert.Equal(t, 0, offset)
		assert.Equal(t, 10, limit)
	})

	t.Run("matches OffsetAndLimit at MaxPageSize", func(t *testing.T) {
		maxInt := int(^uint(0) >> 1)
		cases := []*Pagination{
			nil,
			{},
			{Current: intRef(0), PageSize: intRef(-5)},
			{Current: intRef(3), PageSize: intRef(50)},
			{Current: intRef(1), PageSize: intRef(10000)},
			{Current: intRef(maxInt), PageSize: intRef(MaxPageSize)},
		}
		for _, pagination := range cases {
			wantOffset, wantLimit := pagination.OffsetAndLimit()
			gotOffset, gotLimit := pagination.OffsetAndLimitMax(MaxPageSize)
			assert.Equal(t, wantOffset, gotOffset)
			assert.Equal(t, wantLimit, gotLimit)
		}
	})

	t.Run("original accessors keep the MaxPageSize cap", func(t *testing.T) {
		pagination := &Pagination{Current: intRef(2), PageSize: intRef(10000)}
		offset, limit := pagination.OffsetAndLimit()
		assert.Equal(t, MaxPageSize, offset)
		assert.Equal(t, MaxPageSize, limit)
		assert.Equal(t, MaxPageSize, pagination.Limit())
		assert.Equal(t, 2, *pagination.Current, "pure accessors must not modify the receiver")
		assert.Equal(t, 10000, *pagination.PageSize, "pure accessors must not modify the receiver")
	})
}

func TestPaginationToOffsetAndLimitMax(t *testing.T) {
	t.Run("clears fields after conversion", func(t *testing.T) {
		pagination := &Pagination{Current: intRef(3), PageSize: intRef(5000)}
		offset, limit := pagination.ToOffsetAndLimitMax(10000)
		assert.Equal(t, 10000, offset)
		assert.Equal(t, 5000, limit)
		assert.Nil(t, pagination.Current)
		assert.Nil(t, pagination.PageSize)

		offset, limit = pagination.ToOffsetAndLimitMax(10000)
		assert.Equal(t, 0, offset, "a cleared pagination must fall back to the defaults")
		assert.Equal(t, DefaultPageSize, limit)
	})

	t.Run("non-positive cap disables the cap", func(t *testing.T) {
		pagination := &Pagination{Current: intRef(1), PageSize: intRef(5000)}
		offset, limit := pagination.ToOffsetAndLimitMax(0)
		assert.Equal(t, 0, offset)
		assert.Equal(t, 5000, limit)
		assert.Nil(t, pagination.Current)
		assert.Nil(t, pagination.PageSize)
	})

	t.Run("nil receiver is safe", func(t *testing.T) {
		var pagination *Pagination
		offset, limit := pagination.ToOffsetAndLimitMax(5000)
		assert.Equal(t, 0, offset)
		assert.Equal(t, DefaultPageSize, limit)
	})

	t.Run("matches ToOffsetAndLimit at MaxPageSize", func(t *testing.T) {
		pagination := &Pagination{Current: intRef(3), PageSize: intRef(50)}
		offset, limit := pagination.ToOffsetAndLimit()
		other := &Pagination{Current: intRef(3), PageSize: intRef(50)}
		maxOffset, maxLimit := other.ToOffsetAndLimitMax(MaxPageSize)
		assert.Equal(t, maxOffset, offset)
		assert.Equal(t, maxLimit, limit)
		assert.Nil(t, pagination.Current)
		assert.Nil(t, pagination.PageSize)
		assert.Nil(t, other.Current)
		assert.Nil(t, other.PageSize)
	})
}
