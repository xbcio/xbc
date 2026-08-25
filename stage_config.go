package xbc

import (
	"errors"
	"fmt"

	"github.com/xbcio/xbc/internal/conf"
)

// configPath computes the config subtree path for one instance, following
// the shape spec §6.5 defines: plugins.<name> for single-instance plugins,
// plugins.<name>.<instance> for multi-instance ones.
func (i *instance) configPath() string {
	if isMultiInstance(i.plugin) {
		return "plugins." + i.name + "." + i.instance
	}
	return "plugins." + i.name
}

// bindConfigs is stage 3 of the assembly pipeline: for every instance that
// implements Configurable, it binds that instance's config subtree into
// ConfigPtr() and validates it.
//
// Validation errors are aggregated across every instance instead of
// returning on the first failure -- config mistakes tend to arrive in
// clusters, and stopping at the first one means restarting the process once
// per mistake instead of fixing them all in one pass.
//
// Instances are processed in the order expand() returned them (registration
// order), so that when several instances fail at once, the resulting error
// lines are in a deterministic order and tests never flake on map iteration
// order.
func (a *App) bindConfigs(insts []*instance) error {
	var lines []string

	for _, inst := range insts {
		cfgable, ok := inst.plugin.(Configurable)
		if !ok {
			continue
		}

		ptr := cfgable.ConfigPtr()
		path := inst.configPath()

		if err := a.cfg.Unmarshal(path, ptr); err != nil {
			return fmt.Errorf("xbc: 绑定配置 %s 失败：%w", path, err)
		}

		if err := conf.Validate(ptr, path); err != nil {
			var verr *conf.ValidationError
			if errors.As(err, &verr) {
				lines = append(lines, verr.Lines...)
				continue
			}
			return err
		}
	}

	if len(lines) > 0 {
		return &conf.ValidationError{Lines: lines}
	}
	return nil
}
