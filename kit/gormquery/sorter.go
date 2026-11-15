package gormquery

import (
	"errors"
	"fmt"
	"strings"

	"gorm.io/gen/field"
)

// ErrUnknownSortField reports a sort request naming a field the table does not
// expose. Callers match it with errors.Is and decide their own response; this
// package does not assign it an HTTP meaning.
var ErrUnknownSortField = errors.New("unknown sort field")

// Dao is the subset of a GORM Gen query handle needed to resolve a sort field.
type Dao interface {
	TableName() string
	GetFieldByName(fieldName string) (field.OrderExpr, bool)
}

// Sorter binds the sort field and sort direction of a list request.
type Sorter struct {
	Field string `form:"sorterField"`
	Order string `form:"sorterOrder"`
}

// NewDescendSorter returns a sorter that orders by name descending. It is
// useful as the default passed to ToOrderExpr.
func NewDescendSorter(name string) *Sorter {
	return &Sorter{Field: name, Order: "descend"}
}

// NewAscendSorter returns a sorter that orders by name ascending. It is useful
// as the default passed to ToOrderExpr.
func NewAscendSorter(name string) *Sorter {
	return &Sorter{Field: name, Order: "ascend"}
}

// OrderExpr resolves the sort request to a GORM Gen order expression without
// changing the receiver.
//
// When the request names no field and defaultSorter is not nil, the default
// supplies both the field and the direction. An unknown field returns an error
// wrapping ErrUnknownSortField.
func (s *Sorter) OrderExpr(dao Dao, defaultSorter *Sorter) (field.Expr, error) {
	name, order := "", ""
	if s != nil {
		name, order = s.Field, s.Order
	}
	if name == "" && defaultSorter != nil {
		name, order = defaultSorter.Field, defaultSorter.Order
	}

	column, ok := dao.GetFieldByName(name)
	if !ok {
		return nil, fmt.Errorf("%w: field %q not found in table %q", ErrUnknownSortField, name, dao.TableName())
	}

	// "ascend" is what existing clients send for ascending order; "asc" is
	// accepted as a case-insensitive convenience. Every other value, including
	// the empty string, sorts descending.
	if order == "ascend" || strings.EqualFold(order, "asc") {
		return column.Asc(), nil
	}
	return column.Desc(), nil
}

// ToOrderExpr resolves the sort request like OrderExpr, then clears Field and
// Order. Callers that embed Sorter in a request value object and later pass
// that object to GORM as a condition rely on the clearing so the sorter fields
// are not mistaken for columns. The fields are left in place when resolution
// fails.
func (s *Sorter) ToOrderExpr(dao Dao, defaultSorter *Sorter) (field.Expr, error) {
	expression, err := s.OrderExpr(dao, defaultSorter)
	if err != nil {
		return nil, err
	}
	if s != nil {
		s.Field = ""
		s.Order = ""
	}
	return expression, nil
}
