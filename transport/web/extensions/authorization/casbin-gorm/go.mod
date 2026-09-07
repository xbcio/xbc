module github.com/xbcio/xbc/transport/web/extensions/authorization/casbin-gorm

go 1.25.0

// XBC modules are deliberately absent from require while they have no
// published tag. Repository builds resolve them through go.work; adding local
// replaces or placeholder versions here would make this module non-publishable.
require (
	github.com/casbin/casbin/v2 v2.135.0
	github.com/casbin/gorm-adapter/v3 v3.39.0
	github.com/stretchr/testify v1.12.1
	gorm.io/driver/sqlite v1.6.0
	gorm.io/gorm v1.31.1
)
