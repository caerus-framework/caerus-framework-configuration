package cf_configuration

import (
	cf "github.com/caerus-framework/caerus-framework"
)

// Register the configuration core factory so cf.New(FrameworkOptions) can build
// the always-on configuration component. init() runs when the module is
// imported, which is guaranteed by any package that uses cf_configuration
// symbols.
func init() {
	cf.RegisterConfigurationFactory(func() (cf.CaerusComponent, error) {
		return New(), nil
	})
}
