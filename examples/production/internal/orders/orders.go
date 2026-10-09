// Package orders is the production example's authenticated business Plugin. It
// owns the orders table, writes the domain row and its event in one database
// transaction, and contributes the routes that demonstrate authentication,
// authorization, and owner-scoped data access.
//
// Shutdown: orders accepts no asynchronous work of its own, so it needs no
// Drain. Every request it handles is finished by the Web plugin's own Stop
// before this Plugin is stopped, the database handle it writes through belongs
// to the gorm Plugin, and the outbox it enqueues into publishes nothing in this
// composition because plugins.outbox.worker.enabled is false. A deployment that
// turns that worker on makes the outbox the plugin with accepted work to finish:
// its Drain then runs in the drain phase, after the transport has stopped
// admitting requests and before the database it publishes through is stopped.
// What is left to this Plugin in every case is one connection-owning value it
// must not close: closing it here would break the outbox and the readiness check
// that share it.
package orders

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	gormlib "gorm.io/gorm"

	"github.com/xbcio/xbc/extensions/messaging/outbox"
	"github.com/xbcio/xbc/extensions/reliability/health"
	gormplugin "github.com/xbcio/xbc/extensions/storage/gorm"
	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/transport/web"
)

// Key is orders' stable configuration and runtime identity.
const Key plugin.Key = "orders"

// tableName is the table this Plugin migrates and owns. The name is prefixed
// with the example's own name so it can share a database with anything else.
const tableName = "production_orders"

// listLimit bounds the list endpoint. It exists so the endpoint cannot be used
// to walk an unbounded result set; a real service paginates instead.
const listLimit = 100

// Inputs are required, not optional: an instance of orders is a writer of
// orders *and* of their events, so a composition that omits the database or
// the outbox is a mistake that must fail at startup rather than degrade into a
// writer that silently drops events.
var (
	database = plugin.RefToInstance[*gormlib.DB](gormplugin.Key, plugin.DefaultInstance)
	events   = plugin.RequireOne[outbox.Dispatcher]()
)

// Plugin is the orders primary value.
type Plugin struct {
	db     *gormlib.DB
	events outbox.Dispatcher
}

var (
	_ web.RouteContributor = (*Plugin)(nil)
	_ health.Contributor   = (*Plugin)(nil)
)

var definition = plugin.Define(
	Key,
	func(ctx plugin.BuildContext) (*Plugin, error) {
		return &Plugin{
			db:     database.Get(ctx).Value,
			events: events.Get(ctx).Value,
		}, nil
	},
	plugin.Options[*Plugin]{
		Inputs: plugin.Inputs(database, events),
		Exports: plugin.Contracts(
			plugin.ExportAs[web.RouteContributor](func(value *Plugin) web.RouteContributor { return value }),
			plugin.ExportAs[health.Contributor](func(value *Plugin) health.Contributor { return value }),
		),
		Lifecycle: plugin.Lifecycle[*Plugin]{
			Migrate: (*Plugin).migrate,
		},
	},
)

var bundle = plugin.BundleOf(definition)

// Definition returns orders' canonical immutable declaration handle.
func Definition() plugin.Definition { return definition }

// Bundle returns orders' side-effect-free explicit composition Bundle.
func Bundle() plugin.Bundle { return bundle }

// orderRow is the persisted shape. Owner is the authenticated subject, stored
// so every read can be scoped by it rather than trusted from the request.
type orderRow struct {
	ID        string    `gorm:"column:id;primaryKey;size:32"`
	Owner     string    `gorm:"column:owner;size:128;not null;index:idx_production_orders_owner"`
	Item      string    `gorm:"column:item;size:200;not null"`
	Quantity  int       `gorm:"column:quantity;not null"`
	CreatedAt time.Time `gorm:"column:created_at;not null"`
}

// TableName pins the table name; without it GORM would derive one from the
// struct name and this Plugin would own something it did not declare.
func (orderRow) TableName() string { return tableName }

// Order is the API representation of one stored order. It deliberately does
// not carry Owner: a caller is already scoped to its own subject, so echoing
// the owner back would publish an identity the caller never supplied.
type Order struct {
	ID        string    `json:"id"`
	Item      string    `json:"item"`
	Quantity  int       `json:"quantity"`
	CreatedAt time.Time `json:"created_at"`
}

func (row orderRow) api() Order {
	return Order{ID: row.ID, Item: row.Item, Quantity: row.Quantity, CreatedAt: row.CreatedAt}
}

// CreateRequest is the accepted body of POST /orders.
type CreateRequest struct {
	Item     string `json:"item"     binding:"required,min=1,max=200"`
	Quantity int    `json:"quantity" binding:"required,min=1,max=1000"`
}

// Stats is the payload of the administrative endpoint.
type Stats struct {
	Orders int64 `json:"orders"`
	Owners int64 `json:"owners"`
}

// migrate creates the orders table. It runs in XBC's explicit migration stage,
// never from the factory, so constructing a plan still touches nothing.
func (p *Plugin) migrate(ctx *plugin.Context) error {
	if ctx == nil {
		return errors.New("orders: migrate requires a non-nil plugin context")
	}
	return p.db.WithContext(ctx).AutoMigrate(&orderRow{})
}

// HealthChecks implements health.Contributor. The check pings the connection
// pool this Plugin owns, so a database that stopped answering makes /readyz
// fail and the instance leaves rotation instead of failing requests.
func (p *Plugin) HealthChecks() []health.NamedChecker {
	return []health.NamedChecker{{
		Name:    "database",
		Kind:    health.Readiness,
		Timeout: 500 * time.Millisecond,
		Checker: health.CheckFunc(func(ctx context.Context) error {
			pool, err := p.db.DB()
			if err != nil {
				return err
			}
			return pool.PingContext(ctx)
		}),
	}}
}

// RegisterRoutes contributes the business routes.
//
// None of them calls Auth, and that is the point: an undeclared route resolves
// through web.security.default (deny) and then through every registered
// authentication scheme in the order web.security.schemes declares. Naming the
// schemes with Accepts here instead would fail startup whenever the deployment
// composed only one of them, which is exactly the deployment the container
// smoke test runs.
func (p *Plugin) RegisterRoutes(router *web.Router) {
	router.GET("/orders", p.list).Name("orders.list")
	router.GET("/orders/:id", p.get).Name("orders.get")
	router.POST("/orders", p.create).Name("orders.create")

	// Authorization for every route above is Casbin's: the configured policy
	// keys on the route's path and method, so a subject without a matching
	// policy row is refused even though it authenticated successfully.
	router.GET("/admin/orders/stats", p.stats).Name("orders.admin.stats")
}

func (p *Plugin) list(ctx context.Context, c *web.Ctx) error {
	subject, ok := owner(c)
	if !ok {
		return nil
	}

	var rows []orderRow
	err := p.db.WithContext(ctx).
		Where("owner = ?", subject).
		Order("created_at DESC").
		Limit(listLimit).
		Find(&rows).Error
	if err != nil {
		return err
	}

	orders := make([]Order, 0, len(rows))
	for _, row := range rows {
		orders = append(orders, row.api())
	}
	c.JSON(http.StatusOK, orders)
	return nil
}

func (p *Plugin) get(ctx context.Context, c *web.Ctx) error {
	subject, ok := owner(c)
	if !ok {
		return nil
	}

	var row orderRow
	// The owner is part of the lookup rather than a check performed after it:
	// an order belonging to another subject is then indistinguishable from a
	// missing one, so the endpoint never confirms that an order it will not
	// serve exists. Doing this in one statement also removes the window a
	// read-then-compare pair would leave open.
	err := p.db.WithContext(ctx).
		Where("id = ? AND owner = ?", c.Param("id"), subject).
		First(&row).Error
	if errors.Is(err, gormlib.ErrRecordNotFound) {
		web.AbortProblem(c, web.NewProblem(http.StatusNotFound, "order_not_found"))
		return nil
	}
	if err != nil {
		return err
	}

	c.JSON(http.StatusOK, row.api())
	return nil
}

func (p *Plugin) create(ctx context.Context, c *web.Ctx) error {
	subject, ok := owner(c)
	if !ok {
		return nil
	}

	var request CreateRequest
	if err := c.Bind(&request); err != nil {
		return err
	}

	id, err := newID()
	if err != nil {
		return err
	}
	row := orderRow{
		ID:        id,
		Owner:     subject,
		Item:      request.Item,
		Quantity:  request.Quantity,
		CreatedAt: time.Now().UTC(),
	}
	payload, err := json.Marshal(row.api())
	if err != nil {
		return fmt.Errorf("orders: encode created event: %w", err)
	}

	// One transaction, two writes: the outbox event exists if and only if the
	// order it describes does. Enqueue never commits or rolls back by itself,
	// so this Transaction is the only thing that decides whether either write
	// survives.
	err = p.db.WithContext(ctx).Transaction(func(tx *gormlib.DB) error {
		if err := tx.Create(&row).Error; err != nil {
			return err
		}
		_, err := p.events.Enqueue(ctx, tx, outbox.Event{
			Topic:   "orders.created",
			Key:     row.ID,
			Payload: payload,
		})
		return err
	})
	if err != nil {
		return err
	}

	c.JSON(http.StatusCreated, row.api())
	return nil
}

func (p *Plugin) stats(ctx context.Context, c *web.Ctx) error {
	var summary Stats
	err := p.db.WithContext(ctx).Model(&orderRow{}).
		Select("COUNT(*) AS orders, COUNT(DISTINCT owner) AS owners").
		Scan(&summary).Error
	if err != nil {
		return err
	}

	c.JSON(http.StatusOK, summary)
	return nil
}

// owner reads the identity the framework verified for this request. It is
// never read from a header, query parameter, or body: on a protected route the
// authentication middleware has already published a Principal, and a handler
// that cannot find one is behind a composition mistake -- answering 401 is the
// safe side of that mistake rather than serving the request as anonymous.
//
// A false return means the 401 response has already been written and the
// handler must return immediately. Callers must not treat the empty subject as
// usable: continuing would let the request reach the database as an anonymous
// caller and overwrite the response.
func owner(c *web.Ctx) (string, bool) {
	principal, ok := c.Principal()
	if !ok || principal.Subject == "" {
		web.AbortProblem(c, web.NewProblem(http.StatusUnauthorized, "unauthenticated"))
		return "", false
	}
	return principal.Subject, true
}

// newID returns a random 128-bit identifier in hex. Order identifiers are
// exposed in URLs, so they must not be guessable or enumerable: a sequential
// identifier would let any caller walk every order in the table.
func newID() (string, error) {
	buffer := make([]byte, 16)
	if _, err := rand.Read(buffer); err != nil {
		return "", fmt.Errorf("orders: generate id: %w", err)
	}
	return hex.EncodeToString(buffer), nil
}
