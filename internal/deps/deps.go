// Package deps pins modules the port needs before every package that uses
// them is written, so parallel work doesn't race on go.mod.
package deps

import (
	_ "modernc.org/sqlite"
)
