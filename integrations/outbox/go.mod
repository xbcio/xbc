module github.com/xbcio/xbc/integrations/outbox

go 1.25.0

// XBC core is resolved by the repository workspace until a real release is
// published. This independently publishable module intentionally has no local
// replace or placeholder XBC requirement.
require (
	github.com/stretchr/testify v1.12.1
	gorm.io/driver/sqlite v1.6.0 // test only
	gorm.io/gorm v1.31.1
)

require (
	github.com/jinzhu/inflection v1.0.0 // indirect
	github.com/jinzhu/now v1.1.5 // indirect
	github.com/mattn/go-sqlite3 v1.14.22 // indirect
	go.yaml.in/yaml/v3 v3.0.5 // indirect
	golang.org/x/text v0.37.0 // indirect
)
