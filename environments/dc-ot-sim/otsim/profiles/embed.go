// Package profiles embeds the built-in region and protocol profiles.
package profiles

import "embed"

// FS holds regions/*.yaml, modbus/*.yaml and snmp/*.yaml.
//
//go:embed regions modbus snmp
var FS embed.FS
