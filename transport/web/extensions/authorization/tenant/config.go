package tenant

import (
	"fmt"
	"strings"
)

const (
	defaultHeader              = "X-Tenant-ID"
	defaultTenantIDAttribute   = "tenant_id"
	defaultTenantIDsAttribute  = "tenant_ids"
	defaultAttributesAttribute = "tenant_attributes"
	defaultMaxIDLength         = 128
)

// Config is bound from plugins.tenant. Header is only a selector among IDs
// derived from verified Principal attributes; it is never an identity source.
type Config struct {
	Required                  bool   `yaml:"required"                    default:"true"`
	Header                    string `yaml:"header"                      default:"X-Tenant-ID"`
	TenantIDAttribute         string `yaml:"tenant_id_attribute"         default:"tenant_id"`
	TenantIDsAttribute        string `yaml:"tenant_ids_attribute"        default:"tenant_ids"`
	TenantAttributesAttribute string `yaml:"tenant_attributes_attribute" default:"tenant_attributes"`
	AutoSelectSingle          bool   `yaml:"auto_select_single"          default:"true"`
	MinIDLength               int    `yaml:"min_id_length"                default:"1" validate:"min=1,max=128"`
	MaxIDLength               int    `yaml:"max_id_length"                default:"128" validate:"min=1,max=512"`
}

type normalizedConfig struct {
	required                  bool
	header                    string
	tenantIDAttribute         string
	tenantIDsAttribute        string
	tenantAttributesAttribute string
	autoSelectSingle          bool
	minIDLength               int
	maxIDLength               int
}

func defaultConfig() Config {
	return Config{
		Required:                  true,
		Header:                    defaultHeader,
		TenantIDAttribute:         defaultTenantIDAttribute,
		TenantIDsAttribute:        defaultTenantIDsAttribute,
		TenantAttributesAttribute: defaultAttributesAttribute,
		AutoSelectSingle:          true,
		MinIDLength:               1,
		MaxIDLength:               defaultMaxIDLength,
	}
}

// Validate checks header and verified-principal attribute configuration.
func (c Config) Validate() error {
	_, err := normalizeConfig(c)
	return err
}

func normalizeConfig(c Config) (normalizedConfig, error) {
	d := defaultConfig()
	if strings.TrimSpace(c.Header) == "" {
		c.Header = d.Header
	}
	if strings.TrimSpace(c.TenantIDAttribute) == "" {
		c.TenantIDAttribute = d.TenantIDAttribute
	}
	if strings.TrimSpace(c.TenantIDsAttribute) == "" {
		c.TenantIDsAttribute = d.TenantIDsAttribute
	}
	if strings.TrimSpace(c.TenantAttributesAttribute) == "" {
		c.TenantAttributesAttribute = d.TenantAttributesAttribute
	}
	if c.MinIDLength == 0 {
		c.MinIDLength = d.MinIDLength
	}
	if c.MaxIDLength == 0 {
		c.MaxIDLength = d.MaxIDLength
	}
	if !validHTTPToken(c.Header) {
		return normalizedConfig{}, fmt.Errorf("tenant: header must be a valid HTTP field name")
	}
	idKey := strings.TrimSpace(c.TenantIDAttribute)
	idsKey := strings.TrimSpace(c.TenantIDsAttribute)
	attributesKey := strings.TrimSpace(c.TenantAttributesAttribute)
	if !validAttributeKey(idKey) || !validAttributeKey(idsKey) || !validAttributeKey(attributesKey) {
		return normalizedConfig{}, fmt.Errorf("tenant: principal attribute keys must be non-empty and contain no control characters")
	}
	if idKey == idsKey || idKey == attributesKey || idsKey == attributesKey {
		return normalizedConfig{}, fmt.Errorf("tenant: principal attribute keys must be distinct")
	}
	if c.MinIDLength < 1 || c.MaxIDLength < c.MinIDLength || c.MaxIDLength > 512 {
		return normalizedConfig{}, fmt.Errorf("tenant: tenant ID length bounds are invalid")
	}
	return normalizedConfig{
		required:                  c.Required,
		header:                    c.Header,
		tenantIDAttribute:         idKey,
		tenantIDsAttribute:        idsKey,
		tenantAttributesAttribute: attributesKey,
		autoSelectSingle:          c.AutoSelectSingle,
		minIDLength:               c.MinIDLength,
		maxIDLength:               c.MaxIDLength,
	}, nil
}

func validHTTPToken(value string) bool {
	if value == "" {
		return false
	}
	for i := range len(value) {
		ch := value[i]
		if (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9') {
			continue
		}
		switch ch {
		case '!', '#', '$', '%', '&', '\'', '*', '+', '-', '.', '^', '_', '`', '|', '~':
			continue
		default:
			return false
		}
	}
	return true
}

func validAttributeKey(value string) bool {
	if value == "" || len(value) > 256 {
		return false
	}
	for i := range len(value) {
		if value[i] < 0x20 || value[i] == 0x7f {
			return false
		}
	}
	return true
}
