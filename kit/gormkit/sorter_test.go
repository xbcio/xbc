package gormkit

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gen/field"
)

type stubDao struct {
	table  string
	fields map[string]field.OrderExpr
}

func (d stubDao) TableName() string { return d.table }

func (d stubDao) GetFieldByName(name string) (field.OrderExpr, bool) {
	column, ok := d.fields[name]
	return column, ok
}

func newStubDao() stubDao {
	return stubDao{
		table: "users",
		fields: map[string]field.OrderExpr{
			"id":         field.NewInt("users", "id"),
			"created_at": field.NewTime("users", "created_at"),
		},
	}
}

func TestSorterOrderExpr(t *testing.T) {
	dao := newStubDao()
	db := newTestDB(t)

	t.Run("directions", func(t *testing.T) {
		for _, testCase := range []struct {
			name    string
			order   string
			wantSQL string
		}{
			{name: "ascend", order: "ascend", wantSQL: "`users`.`id` ASC"},
			{name: "asc shorthand", order: "asc", wantSQL: "`users`.`id` ASC"},
			{name: "asc uppercase", order: "ASC", wantSQL: "`users`.`id` ASC"},
			{name: "descend", order: "descend", wantSQL: "`users`.`id` DESC"},
			{name: "desc shorthand", order: "desc", wantSQL: "`users`.`id` DESC"},
			{name: "empty order", order: "", wantSQL: "`users`.`id` DESC"},
			{name: "unknown order", order: "sideways", wantSQL: "`users`.`id` DESC"},
		} {
			t.Run(testCase.name, func(t *testing.T) {
				expression, err := (&Sorter{Field: "id", Order: testCase.order}).OrderExpr(dao, nil)
				require.NoError(t, err)
				sql, vars := render(t, db, expression)
				assert.Equal(t, testCase.wantSQL, sql)
				assert.Empty(t, vars)
			})
		}
	})

	t.Run("default sorter supplies field and direction", func(t *testing.T) {
		expression, err := (&Sorter{}).OrderExpr(dao, NewDescendSorter("created_at"))
		require.NoError(t, err)
		sql, _ := render(t, db, expression)
		assert.Equal(t, "`users`.`created_at` DESC", sql)
	})

	t.Run("default sorter ignores a request direction without a field", func(t *testing.T) {
		expression, err := (&Sorter{Order: "ascend"}).OrderExpr(dao, NewDescendSorter("created_at"))
		require.NoError(t, err)
		sql, _ := render(t, db, expression)
		assert.Equal(t, "`users`.`created_at` DESC", sql)
	})

	t.Run("explicit field overrides the default", func(t *testing.T) {
		expression, err := (&Sorter{Field: "id", Order: "ascend"}).OrderExpr(dao, NewDescendSorter("created_at"))
		require.NoError(t, err)
		sql, _ := render(t, db, expression)
		assert.Equal(t, "`users`.`id` ASC", sql)
	})

	t.Run("unknown field returns the sentinel", func(t *testing.T) {
		_, err := (&Sorter{Field: "missing"}).OrderExpr(dao, nil)
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrUnknownSortField)
		assert.Contains(t, err.Error(), `"missing"`)
		assert.Contains(t, err.Error(), `"users"`)
	})

	t.Run("nil receiver falls back to the default", func(t *testing.T) {
		var sorter *Sorter
		expression, err := sorter.OrderExpr(dao, NewAscendSorter("id"))
		require.NoError(t, err)
		sql, _ := render(t, db, expression)
		assert.Equal(t, "`users`.`id` ASC", sql)
	})
}

func TestSorterToOrderExpr(t *testing.T) {
	dao := newStubDao()
	db := newTestDB(t)

	t.Run("clears fields after conversion", func(t *testing.T) {
		sorter := &Sorter{Field: "id", Order: "ascend"}
		expression, err := sorter.ToOrderExpr(dao, nil)
		require.NoError(t, err)
		sql, _ := render(t, db, expression)
		assert.Equal(t, "`users`.`id` ASC", sql)
		assert.Empty(t, sorter.Field)
		assert.Empty(t, sorter.Order)
	})

	t.Run("keeps fields when resolution fails", func(t *testing.T) {
		sorter := &Sorter{Field: "missing", Order: "ascend"}
		_, err := sorter.ToOrderExpr(dao, nil)
		require.ErrorIs(t, err, ErrUnknownSortField)
		assert.Equal(t, "missing", sorter.Field)
		assert.Equal(t, "ascend", sorter.Order)
	})

	t.Run("pure resolver leaves fields in place", func(t *testing.T) {
		sorter := &Sorter{Field: "id", Order: "descend"}
		_, err := sorter.OrderExpr(dao, nil)
		require.NoError(t, err)
		assert.Equal(t, "id", sorter.Field)
		assert.Equal(t, "descend", sorter.Order)
	})

	t.Run("nil receiver is safe", func(t *testing.T) {
		var sorter *Sorter
		expression, err := sorter.ToOrderExpr(dao, NewDescendSorter("id"))
		require.NoError(t, err)
		sql, _ := render(t, db, expression)
		assert.Equal(t, "`users`.`id` DESC", sql)
	})
}
