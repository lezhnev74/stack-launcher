// Package runlog writes lifecycle diagnostics to the launcher's standard log.
package runlog

import (
	"fmt"
	"log"
)

// Printf writes a message with the same run identifier used by the UI and API.
func Printf(id, format string, args ...any) {
	log.Printf("run_id=%q %s", id, fmt.Sprintf(format, args...))
}
