package container

import (
	"errors"
	"fmt"

	"github.com/xbcio/xbc/config"
	"github.com/xbcio/xbc/plugin"
)

// bindConfigs is the second of Assemble's three stages, between expand and
// resolve. It lives in its own file so each stage of that pipeline maps to a
// filename a reader can find: it used to sit at the bottom of expand.go, where
// nothing about the name expand.go suggested that the config-binding stage was
// hiding in it.
//
// Validation errors are accumulated rather than returned on the first failure,
// so a misconfigured deployment reports every bad plugin section in one run
// instead of forcing the operator to fix and restart once per section. Binding
// errors are different and return immediately: a bind failure means the config
// shape itself is wrong, and continuing would report validation errors derived
// from a struct that was never populated.
func (c *Container) bindConfigs(insts []*Instance) error {
	var verr *config.ValidationError
	for _, inst := range insts {
		cfgable, ok := inst.plugin.(plugin.Configurable)
		if !ok {
			continue
		}
		path := inst.configPath()
		ptr, err := callConfigPtr(inst, cfgable)
		if err != nil {
			return err
		}
		if err := c.env.BindWithOptions(path, ptr, config.BindOptions{
			AllowedKeys: []string{"enabled"},
		}); err != nil {
			return fmt.Errorf("xbc: 绑定配置 %s 失败：%w", path, err)
		}
		if err := config.Validate(ptr, path); err != nil {
			var ve *config.ValidationError
			if errors.As(err, &ve) {
				if verr == nil {
					verr = &config.ValidationError{}
				}
				verr.Append(ve)
				continue
			}
			return err
		}
	}
	if verr != nil {
		return verr
	}
	return nil
}
