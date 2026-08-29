package web

import "time"

// Config is web's own configuration section, bound at "plugins.web" by
// package assembly calling Server.ConfigPtr(). shutdown_timeout deliberately does
// NOT live here (package-layout design §5.3): it answers "how long may this
// whole process take to disappear", a core-owned question, not a web-owned
// one. Server.Stop instead drains for however long is left on the deadline
// core hands it through Stop's context.
//
// Tag names and default values are copied verbatim from the field-by-field
// walk of the pre-split root config.go's ServerConfig (BasePath default "/",
// ReadTimeout default "10s") -- not from the illustrative snippet in the
// design doc, which the doc itself instructs be double-checked against that
// source file rather than taken as gospel.
type Config struct {
	Addr         string        `yaml:"addr"          default:":8080"`
	BasePath     string        `yaml:"base_path"     default:"/"`
	ReadTimeout  time.Duration `yaml:"read_timeout"  default:"10s"`
	WriteTimeout time.Duration `yaml:"write_timeout" default:"30s"`
}
